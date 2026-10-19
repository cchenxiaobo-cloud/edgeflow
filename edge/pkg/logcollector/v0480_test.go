package logcollector

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"edgeflow/pkg/protocol"
)

// captureDispatch 捕获上行消息（测试用直发捕获器）。
type captureDispatch struct {
	mu   sync.Mutex
	msgs []*protocol.Message
}

func (c *captureDispatch) send(m *protocol.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, m)
	return nil
}

func (c *captureDispatch) batches() []protocol.LogBatch {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []protocol.LogBatch
	for _, m := range c.msgs {
		var b protocol.LogBatch
		if m.Type == protocol.TypeLogSync && m.DecodePayload(&b) == nil {
			out = append(out, b)
		}
	}
	return out
}

func newTestCollector(t *testing.T, dir string, batchN int) (*Collector, *captureDispatch) {
	t.Helper()
	t.Setenv(EnvEnabled, "on")
	t.Setenv(EnvLogDir, dir)
	t.Setenv(EnvBatch, itoa(batchN))
	t.Setenv(EnvFlushSec, "3600") // 测试手动 flush，排除定时干扰
	d := &captureDispatch{}
	return New("n-test", d.send), d
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [12]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

func TestV0480CollectorDisabledByDefault(t *testing.T) {
	d := &captureDispatch{}
	c := New("n1", d.send) // 不设 EnvEnabled
	if c.Enabled() {
		t.Fatalf("默认应关闭")
	}
	c.Emit("app", "info", "x")
	c.Start() // 空壳零行为（不 panic 即可）
	if st := c.Stats(); st.Collected != 0 || st.Buffered != 0 {
		t.Fatalf("未启用不应采集: %+v", st)
	}
}

func TestV0480FileTailAndLevelInference(t *testing.T) {
	dir := t.TempDir()
	c, d := newTestCollector(t, dir, 10)
	c.Start()
	defer c.Stop()

	logPath := filepath.Join(dir, "app.log")
	f, _ := os.Create(logPath)
	f.WriteString("[INFO] service started\n")
	f.WriteString("[ERROR] disk almost full\n")
	f.WriteString("[WARN] slow query\n")
	f.WriteString("plain line without level\n")
	f.Close()

	waitFor(t, "文件行入缓冲", func() bool { return c.Stats().Buffered >= 4 })
	if n := c.flushOnce(); n != 4 {
		t.Fatalf("flush 应 4 条: %d", n)
	}
	bs := d.batches()
	if len(bs) != 1 || len(bs[0].Entries) != 4 {
		t.Fatalf("应为一批 4 条: %+v", bs)
	}
	e := bs[0].Entries
	if e[0].Level != "info" || e[0].Message != "service started" {
		t.Fatalf("[INFO] 解析错误: %+v", e[0])
	}
	if e[1].Level != "error" || e[1].Message != "disk almost full" {
		t.Fatalf("[ERROR] 解析错误: %+v", e[1])
	}
	if e[2].Level != "warn" {
		t.Fatalf("[WARN] 解析错误: %+v", e[2])
	}
	if e[3].Level != "info" || e[3].Message != "plain line without level" {
		t.Fatalf("无前缀行应 info+全文: %+v", e[3])
	}
	if e[0].Source != "app" || e[0].Ts == 0 {
		t.Fatalf("source/ts 口径错误: %+v", e[0])
	}
	if st := c.Stats(); st.Sent != 4 || st.Collected != 4 {
		t.Fatalf("统计口径错误: %+v", st)
	}
}

func TestV0480TailIncrementalAndTruncateReset(t *testing.T) {
	dir := t.TempDir()
	c, d := newTestCollector(t, dir, 100)
	c.Start()
	defer c.Stop()

	logPath := filepath.Join(dir, "app.log")
	f, _ := os.Create(logPath)
	f.WriteString("[INFO] first\n")
	f.Close()
	waitFor(t, "首批采集", func() bool { return c.Stats().Buffered >= 1 })

	f2, _ := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0600)
	f2.WriteString("[INFO] second\n")
	f2.Close()
	waitFor(t, "增量采集", func() bool { return c.Stats().Buffered >= 2 })
	c.flushOnce()

	// truncate 后重写：offset 重置，新内容重读
	os.Truncate(logPath, 0)
	f3, _ := os.OpenFile(logPath, os.O_WRONLY, 0600)
	f3.WriteString("[INFO] after-truncate\n")
	f3.Close()
	waitFor(t, "truncate 重置后采集", func() bool { return c.Stats().Buffered >= 1 })
	c.flushOnce()

	var last protocol.LogBatch
	for _, b := range d.batches() {
		last = b
	}
	if last.Entries[len(last.Entries)-1].Message != "after-truncate" {
		t.Fatalf("truncate 重置应重读: %+v", last.Entries)
	}
}

func TestV0480MessageTruncation4KB(t *testing.T) {
	dir := t.TempDir()
	c, d := newTestCollector(t, dir, 10)
	c.Emit("app", "info", strings.Repeat("汉", 3000)) // 9000 字节 UTF-8
	if n := c.flushOnce(); n != 1 {
		t.Fatalf("flush 应 1 条")
	}
	e := d.batches()[0].Entries[0]
	if len(e.Message) > protocol.LogMaxMessageBytes {
		t.Fatalf("应截断到 4KB 内: %d", len(e.Message))
	}
	if !strings.HasSuffix(e.Message, "汉") {
		t.Fatalf("截断点应回退 rune 边界: %q", e.Message[len(e.Message)-4:])
	}
}

func TestV0480BufferBackpressureDropOldest(t *testing.T) {
	dir := t.TempDir()
	c, _ := newTestCollector(t, dir, 2000) // batchN 高于缓冲上限 → 不触发自动 flush
	for i := 0; i < bufferMax+50; i++ {
		c.Emit("app", "info", itoa(i))
	}
	st := c.Stats()
	if st.Buffered != bufferMax {
		t.Fatalf("缓冲应封顶 %d: %d", bufferMax, st.Buffered)
	}
	if st.Dropped != 50 {
		t.Fatalf("应丢 50 条最旧: %d", st.Dropped)
	}
	if n := c.flushOnce(); n != bufferMax {
		t.Fatalf("flush 应 %d 条: %d", bufferMax, n)
	}
	// 最旧被丢：首条应是第 50 条
}

func TestV0480BatchSplitAt200(t *testing.T) {
	dir := t.TempDir()
	c, d := newTestCollector(t, dir, 250)
	for i := 0; i < 250; i++ {
		c.Emit("app", "info", itoa(i))
	}
	if n := c.flushOnce(); n != 250 {
		t.Fatalf("flush 应 250 条: %d", n)
	}
	bs := d.batches()
	if len(bs) != 2 || len(bs[0].Entries) != protocol.LogMaxBatchEntries || len(bs[1].Entries) != 50 {
		t.Fatalf("应拆两批 200+50: %d 批", len(bs))
	}
	for _, b := range bs {
		if err := b.Validate(); err != nil {
			t.Fatalf("批校验失败: %v", err)
		}
	}
}

func TestV0480InitialSnapshotSkipsHistory(t *testing.T) {
	// 复核 P1-2 回归：启动前已存在的文件不回放历史（scanInitial 登记 EOF）
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")
	f, _ := os.Create(logPath)
	for i := 0; i < 100; i++ {
		fmt.Fprintf(f, "[INFO] history line %d\n", i)
	}
	f.Close()
	c, d := newTestCollector(t, dir, 10)
	c.Start()
	defer c.Stop()
	time.Sleep(1500 * time.Millisecond) // 覆盖至少一轮 tail
	if n := c.flushOnce(); n != 0 {
		t.Fatalf("启动前历史不应被采集: %d", n)
	}
	if len(d.batches()) != 0 {
		t.Fatalf("不应有任何批次")
	}
	// 启动后追加 → 仅增量被采集
	f2, _ := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0600)
	fmt.Fprintln(f2, "[INFO] fresh line")
	f2.Close()
	waitFor(t, "增量采集", func() bool { return c.Stats().Buffered >= 1 })
	if n := c.flushOnce(); n != 1 {
		t.Fatalf("增量应 1 条: %d", n)
	}
	e := d.batches()[0].Entries[0]
	if e.Message != "fresh line" {
		t.Fatalf("应只有 fresh line: %+v", e)
	}
}

func TestV0480LongLineNoHotSpin(t *testing.T) {
	// 复核 P1-3 回归：>64KB 无换行单行不得热自旋/卡死，且强制成行后继续采集
	dir := t.TempDir()
	c, d := newTestCollector(t, dir, 10)
	c.Start()
	defer c.Stop()
	logPath := filepath.Join(dir, "app.log")
	f, _ := os.Create(logPath)
	f.WriteString(strings.Repeat("x", 150*1024)) // 无换行长行（>64KB 块 2.3 倍）
	fmt.Fprintln(f, "")
	fmt.Fprintln(f, "[INFO] after-big")
	f.Close()
	waitFor(t, "长行+后续行采集", func() bool { return c.Stats().Buffered >= 2 })
	if n := c.flushOnce(); n != 2 {
		t.Fatalf("应 2 条（长行强制成行 + after-big）: %d", n)
	}
	e := d.batches()[0].Entries
	if len(e[0].Message) > protocol.LogMaxMessageBytes {
		t.Fatalf("长行应截断 4KB: %d", len(e[0].Message))
	}
	if e[1].Message != "after-big" {
		t.Fatalf("后续行应正常: %+v", e[1])
	}
}

func TestV0480CarryAcrossPolls(t *testing.T) {
	// 复核 P1-3 回归：跨轮半行拼接（EOF 到达时行未完 → 下轮续上）
	dir := t.TempDir()
	c, d := newTestCollector(t, dir, 10)
	c.Start()
	defer c.Stop()
	logPath := filepath.Join(dir, "app.log")
	f, _ := os.Create(logPath)
	f.WriteString("[INFO] part1-") // 无换行：进 carry，offset 前进
	f.Close()
	waitFor(t, "半行进 carry", func() bool { return c.Stats().Collected > 0 || c.offsetsForTest(logPath) > 0 })
	f2, _ := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0600)
	f2.WriteString("part2")
	fmt.Fprintln(f2, "")
	f2.Close()
	waitFor(t, "续行完成", func() bool { return c.Stats().Buffered >= 1 })
	if n := c.flushOnce(); n != 1 {
		t.Fatalf("应 1 条: %d", n)
	}
	e := d.batches()[0].Entries[0]
	if e.Message != "part1-part2" {
		t.Fatalf("跨轮 carry 应拼接完整行: %q", e.Message)
	}
}

func TestV0480BackpressureDropOldestContent(t *testing.T) {
	// 复核 P2-5 回归：背压丢最旧的内容断言（首条=51 次 emit）
	dir := t.TempDir()
	c, d := newTestCollector(t, dir, 2000)
	for i := 0; i < bufferMax+50; i++ {
		c.Emit("app", "info", itoa(i))
	}
	if n := c.flushOnce(); n != bufferMax {
		t.Fatalf("flush 应 %d 条: %d", bufferMax, n)
	}
	bs := d.batches()
	first := bs[0].Entries[0].Message
	last := bs[len(bs)-1].Entries[len(bs[len(bs)-1].Entries)-1].Message
	if first != "50" || last != "1049" {
		t.Fatalf("丢最旧口径错误（应 50..1049）: first=%s last=%s", first, last)
	}
}

func TestV0480SanitizeInvalidUTF8(t *testing.T) {
	// 复核 P2-1 回归：非法 UTF-8（GBK 形态）净化后不膨胀、JSON 往返后仍过校验
	dir := t.TempDir()
	c, d := newTestCollector(t, dir, 10)
	bad := "\xC4\xE3\xBA\xC3" + strings.Repeat("y", 4090) // 非法字节 + 贴边长度
	c.Emit("app", "info", bad)
	if n := c.flushOnce(); n != 1 {
		t.Fatalf("flush 应 1 条")
	}
	e := d.batches()[0].Entries[0]
	raw, _ := json.Marshal(e)
	var back protocol.LogEntry
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("json 往返失败: %v", err)
	}
	batch := protocol.LogBatch{Entries: []protocol.LogEntry{back}}
	if err := batch.Validate(); err != nil {
		t.Fatalf("净化后应过云端校验（P2-1 静默丢失回归）: %v", err)
	}
	if strings.ContainsRune(e.Message, 0xFFFD) && len(e.Message) > protocol.LogMaxMessageBytes {
		t.Fatalf("不应有 U+FFFD 膨胀")
	}
}

// offsetsForTest 读取指定文件的已登记 offset（测试轮询用）。
func (c *Collector) offsetsForTest(path string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.offsets[path]
}
