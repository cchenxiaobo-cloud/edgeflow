// 边缘日志采集器（v0.48.0，spec 0021 US-1）：文件 tail + 程序内事件注入 +
// 批量上行（复用上行补传通道，零新队列）。
//
// 启停语义（与 scriptrunner 同口径的 opt-in）：EDGEFLOW_EDGED_LOGCOLLECTOR=on
// 才启用；默认关——零 goroutine、零行为。
//
// 采集形态（spec §3.4/§3.7 裁决）：
//   - 文件源：EDGEFLOW_EDGED_LOG_DIR 下 *.log 追加 tail（启动从 EOF 不回放
//     历史；truncate 检测 size<offset 重置从头）；1s 轮询；
//   - 事件源：Emit(source, level, message)（scriptrunner 生命周期接线示范）；
//   - 行解析：[LEVEL] 前缀推断级别（缺省 info）；ts 取采集时刻；
//   - 批量：满 batchN 或 flush 间隔到 → 一条 LogSync（≤200 条/批，超限拆批）；
//   - 背压：缓冲上限 1000 条，满丢最旧 + dropped 计数；
//   - 上行：dispatch 由装配层注入（UPLINK on 入补传队列 / off 直发）——
//     断网不丢 = 队列持久化 + 恢复续传（v0.39 语义同构）。
package logcollector

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"
)

// 环境变量（spec §4 env 清单）。
const (
	EnvEnabled  = "EDGEFLOW_EDGED_LOGCOLLECTOR"  // =on 启用（默认关）
	EnvLogDir   = "EDGEFLOW_EDGED_LOG_DIR"       // 文件源目录（*.log）
	EnvBatch    = "EDGEFLOW_EDGED_LOG_BATCH"     // 批量阈值（默认 50，1..200）
	EnvFlushSec = "EDGEFLOW_EDGED_LOG_FLUSH_SEC" // flush 间隔秒（默认 5，≥1）
)

// 采集器常量。
const (
	bufferMax     = 1000                        // 缓冲上限（满丢最旧）
	tailPollEvery = 1 * time.Second             // 文件轮询周期
	maxMessageLen = protocol.LogMaxMessageBytes // 单条 message 截断（4KB）
	maxLineBytes  = 1 << 20                     // 单行上限 1MB（超限强制成行，防 carry 无界——复核 P1-3）
)

// Dispatch 是上行分发函数（与 alarm 的 uplinkDispatch 同构：入队或直发由
// 装配层决定；返回 error 仅用于直发路径的失败日志，入队路径不失败）。
type Dispatch func(msg *protocol.Message) error

// Stats 是采集器观测统计。
type Stats struct {
	Collected int64 // 累计采集（含后因背压丢弃的）
	Sent      int64 // 累计上行条数
	Dropped   int64 // 背压丢弃条数
	Buffered  int   // 当前缓冲条数
}

// Collector 是边缘日志采集器。零值不可用；New 构造（未启用时为惰性空壳）。
type Collector struct {
	enabled  bool
	nodeID   string
	dir      string
	batchN   int
	flush    time.Duration
	dispatch Dispatch

	mu        sync.Mutex
	buf       []protocol.LogEntry
	offsets   map[string]int64  // 文件绝对路径 → 已读偏移
	carries   map[string]string // 文件绝对路径 → 跨轮半行缓冲（复核 P1-3）
	dropped   int64
	collected int64

	sent     atomic.Int64
	wake     chan struct{} // 容量 1：批量阈值到达通知（非阻塞投递）
	stopCh   chan struct{}
	stopOnce sync.Once   // Stop 重入保护（复核 P3-2）
	stopped  atomic.Bool // Stop 后 Emit 拒绝（复核 P3-5）
	started  atomic.Bool
	wg       sync.WaitGroup
}

// New 构造采集器（读环境变量；未启用返回惰性空壳——Emit/Start 均零行为）。
func New(nodeID string, dispatch Dispatch) *Collector {
	c := &Collector{
		nodeID:   nodeID,
		dispatch: dispatch,
		offsets:  map[string]int64{},
		carries:  map[string]string{},
		wake:     make(chan struct{}, 1),
		stopCh:   make(chan struct{}),
	}
	if os.Getenv(EnvEnabled) != "on" {
		return c
	}
	c.enabled = true
	c.dir = os.Getenv(EnvLogDir)
	c.batchN = envInt(EnvBatch, 50, 1, protocol.LogMaxBatchEntries)
	c.flush = time.Duration(envInt(EnvFlushSec, 5, 1, 3600)) * time.Second
	if dispatch == nil {
		log.Warnf("[logcollector] 启用但未注入上行分发——降级为仅缓冲")
	}
	return c
}

// Enabled 返回是否启用（opt-in）。
func (c *Collector) Enabled() bool { return c.enabled }

// Start 启动采集循环（文件轮询 + 定时 flush）；未启用零行为；幂等。
func (c *Collector) Start() {
	if !c.enabled {
		return
	}
	if !c.started.CompareAndSwap(false, true) {
		return
	}
	c.scanInitial() // 启动快照：既有文件登记 EOF（复核 P1-2）
	c.wg.Add(1)
	go c.loop()
	log.Infof("[logcollector] 日志采集已启用（dir=%s batch=%d flush=%s）", c.dir, c.batchN, c.flush)
}

// scanInitial 启动快照：启动时已存在的 *.log 一律登记 offset=size（从 EOF 起，
// 不回放历史——spec §3.4/§3.7「启动从 EOF」）；此后新出现的文件由 tailFile 以
// offset=0 收集。二分语义：启动前 = 历史（跳过），启动后 = 增量（收集）。
func (c *Collector) scanInitial() {
	if c.dir == "" {
		return
	}
	matches, err := filepath.Glob(filepath.Join(c.dir, "*.log"))
	if err != nil {
		return
	}
	for _, path := range matches {
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			c.mu.Lock()
			if _, seen := c.offsets[path]; !seen {
				c.offsets[path] = st.Size()
			}
			c.mu.Unlock()
		}
	}
}

// Stop 停止并尽力 flush 残留缓冲（重入安全——复核 P3-2 sync.Once）。
func (c *Collector) Stop() {
	if !c.enabled {
		return
	}
	c.stopped.Store(true)
	c.stopOnce.Do(func() { close(c.stopCh) })
	c.wg.Wait()
	if n := c.flushOnce(); n > 0 {
		log.Infof("[logcollector] 停止前 flush %d 条", n)
	}
}

// Emit 注入一条程序内事件日志（级别白名单外降级 info；空 source 忽略）。
// 并发安全；缓冲满丢最旧（背压保护，不阻塞调用方）。
func (c *Collector) Emit(source, level, message string) {
	if !c.enabled || c.stopped.Load() || source == "" {
		return
	}
	switch level {
	case protocol.LogLevelDebug, protocol.LogLevelInfo, protocol.LogLevelWarn, protocol.LogLevelError:
	default:
		level = protocol.LogLevelInfo
	}
	c.append(protocol.LogEntry{
		Ts:      time.Now().UnixMilli(),
		Source:  clampSource(source),
		Level:   level,
		Message: sanitizeMessage(message),
	})

}

// Stats 返回观测统计快照。
func (c *Collector) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stats{
		Collected: c.collected,
		Sent:      c.sent.Load(),
		Dropped:   c.dropped,
		Buffered:  len(c.buf),
	}
}

// ── 内部 ──

// loop 主循环：文件轮询 + flush 定时/唤醒。
func (c *Collector) loop() {
	defer c.wg.Done()
	tail := time.NewTicker(tailPollEvery)
	defer tail.Stop()
	flush := time.NewTicker(c.flush)
	defer flush.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-tail.C:
			c.tailOnce()
		case <-flush.C:
			c.flushOnce()
		case <-c.wake:
			c.flushOnce()
		}
	}
}

// tailOnce 扫描目录增量读取（新文件出现/追加/truncate 三态）。
func (c *Collector) tailOnce() {
	if c.dir == "" {
		return
	}
	matches, err := filepath.Glob(filepath.Join(c.dir, "*.log"))
	if err != nil {
		return
	}
	sort.Strings(matches)
	for _, path := range matches {
		c.tailFile(path)
	}
}

// tailFile 增量读取单个文件（复核 P1-3 重构：offset 恒前进 + 跨轮 carry 半行
// 缓冲 + 1MB 单行上限——原「无换行回退整块」在 >64KB 单行时热自旋卡死采集）。
func (c *Collector) tailFile(path string) {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return
	}
	c.mu.Lock()
	off, seen := c.offsets[path]
	frag := c.carries[path]
	c.mu.Unlock()
	if !seen {
		// 启动快照后新出现的文件：从 0 收集（启动前已存在者已在 scanInitial
		// 登记为 EOF——「启动从 EOF 不回放历史」由该二分语义保证，复核 P1-2）。
		off = 0
	} else if st.Size() < off {
		// truncate/rotate：重置从头 + 半行缓冲作废（rotate 窗口可能短时
		// 重复/丢行——KI §49 #3 登记）。
		off = 0
		frag = ""
	}
	if st.Size() == off && frag == "" {
		return // 无新增
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	buf := make([]byte, 64*1024)
	var consumed int64
	source := strings.TrimSuffix(filepath.Base(path), ".log")
	for {
		if off+consumed >= st.Size() {
			break
		}
		n, rerr := f.ReadAt(buf, off+consumed)
		if n <= 0 {
			break // EOF/错误：下轮再看
		}
		chunk := buf[:n]
		consumed += int64(n)
		for {
			idx := bytes.IndexByte(chunk, '\n')
			if idx < 0 {
				break
			}
			line := frag + string(chunk[:idx])
			frag = ""
			if msg := strings.TrimRight(line, "\r"); msg != "" {
				c.append(parseLine(source, msg))
				c.maybeFlush() // burst 采集同步放水（e2e run5 教训：tailOnce 同步
				// 循环内 wake 异步 flush 来不及，缓冲 1000 上限会吞 burst 头部）
			}
			chunk = chunk[idx+1:]
		}
		if len(chunk) > 0 {
			// 半行进 carry：offset 已前进（不回退、无热自旋），下轮拼接续行
			frag += string(chunk)
			if len(frag) > maxLineBytes {
				c.append(parseLine(source, frag)) // 单行超限强制成行（4KB 截断兜底）
				frag = ""
			}
		}
		if rerr != nil { // ReadAt n>0 且 err==EOF：已到尾
			break
		}
	}
	c.mu.Lock()
	c.offsets[path] = off + consumed
	c.carries[path] = frag
	c.mu.Unlock()
}

// parseLine 解析一行：[LEVEL] 前缀推断级别，余下为正文。
func parseLine(source, line string) protocol.LogEntry {
	level := protocol.LogLevelInfo
	msg := line
	if len(line) > 2 && line[0] == '[' {
		if end := strings.IndexByte(line, ']'); end > 0 {
			switch strings.ToLower(line[1:end]) {
			case "debug":
				level = protocol.LogLevelDebug
			case "info":
				level = protocol.LogLevelInfo
			case "warn", "warning":
				level = protocol.LogLevelWarn
			case "error", "fatal":
				level = protocol.LogLevelError
			default:
				level = protocol.LogLevelInfo
				msg = line // 非级别前缀：整体为正文
				return protocol.LogEntry{Ts: time.Now().UnixMilli(), Source: clampSource(source), Level: level, Message: sanitizeMessage(msg)}
			}
			msg = strings.TrimSpace(line[end+1:])
		}
	}
	return protocol.LogEntry{Ts: time.Now().UnixMilli(), Source: clampSource(source), Level: level, Message: sanitizeMessage(msg)}
}

// append 入缓冲（背压：满丢最旧）+ 批量阈值唤醒。
func (c *Collector) append(e protocol.LogEntry) {
	c.mu.Lock()
	if len(c.buf) >= bufferMax {
		copy(c.buf, c.buf[1:])
		c.buf[len(c.buf)-1] = e
		c.dropped++
	} else {
		c.buf = append(c.buf, e)
	}
	c.collected++
	full := len(c.buf) >= c.batchN
	c.mu.Unlock()
	if full {
		select {
		case c.wake <- struct{}{}:
		default:
		}
	}
}

// maybeFlush 缓冲达批量阈值时同步 flush（tailFile burst 场景专用——
// loop goroutine 内调用，与 append 互斥安全）。
func (c *Collector) maybeFlush() {
	c.mu.Lock()
	full := len(c.buf) >= c.batchN
	c.mu.Unlock()
	if full {
		c.flushOnce()
	}
}

// flushOnce 取走缓冲（≤200/批拆批发送）；返回发送条数。
func (c *Collector) flushOnce() int {
	c.mu.Lock()
	batch := c.buf
	c.buf = nil
	c.mu.Unlock()
	if len(batch) == 0 {
		return 0
	}
	sent := 0
	for start := 0; start < len(batch); start += protocol.LogMaxBatchEntries {
		end := start + protocol.LogMaxBatchEntries
		if end > len(batch) {
			end = len(batch)
		}
		part := batch[start:end]
		if c.dispatch == nil {
			sent += len(part)
			continue
		}
		msg, err := protocol.NewMessage(protocol.TypeLogSync, c.nodeID, "cloud", protocol.LogBatch{Entries: part})
		if err != nil {
			log.Warnf("[logcollector] 构造 LogSync 失败（丢弃 %d 条）: %v", len(part), err)
			continue
		}
		if err := c.dispatch(msg); err != nil {
			// 直发路径失败：条目丢弃（补传路径不失败——断网不丢由队列保证）。
			log.Warnf("[logcollector] LogSync 直发失败（丢弃 %d 条）: %v", len(part), err)
			continue
		}
		sent += len(part)
	}
	c.sent.Add(int64(sent))
	return sent
}

// clampSource 收敛 source 长度（≤64 字节且 rune 对齐——复核 P2-2：字节切断
// 可破 rune 边界，JSON 往返 U+FFFD 膨胀后超 64B 致整批拒收；预留 2B 余量）。
func clampSource(s string) string {
	if len(s) <= protocol.LogMaxSourceLen {
		return s
	}
	out := make([]byte, 0, protocol.LogMaxSourceLen)
	for _, r := range s {
		n := utf8.RuneLen(r)
		if n < 0 {
			n = 1
		}
		if len(out)+n > protocol.LogMaxSourceLen-2 {
			break
		}
		out = append(out, string(r)...)
	}
	return string(out)
}

// truncateMessage 截断正文到 4KB，且截断点必须落在 rune 边界（丢掉悬空的
// 不完整序列尾部——否则 JSON 序列化会把非法字节替换为 U+FFFD 造成膨胀）。
func truncateMessage(msg string) string {
	if len(msg) <= maxMessageLen {
		return msg
	}
	cut := msg[:maxMessageLen]
	for i := 0; i < 3 && len(cut) > 0; i++ {
		if r, size := utf8.DecodeLastRuneInString(cut); r != utf8.RuneError || size != 1 {
			break // 末尾是完整 rune（含合法 ASCII）
		}
		cut = cut[:len(cut)-1] // 末尾是不完整序列的首字节：丢弃后重试
	}
	return cut
}

// sanitizeMessage 正文净化：非法 UTF-8 字节序替换为 '?'（复核 P2-1：原样透传
// 时 JSON 序列化按 U+FFFD 膨胀，云端按字节校验将整批拒收且补传不重发——静默
// 丢失，GBK 日志必现），再 rune 边界安全截断 4KB。
func sanitizeMessage(msg string) string {
	return truncateMessage(strings.ToValidUTF8(msg, "?"))
}

// envInt 读环境变量整数（越界/非法用默认）。
func envInt(key string, def, min, max int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < min || n > max {
		return def
	}
	return n
}
