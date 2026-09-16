// Package tsdb 提供自研零依赖的边缘轻量时序库（v0.38.0，spec 0011）。
//
// 能力：追加写（批/流）+ 时间窗查询 + 窗口聚合 + 段粒度保留/降采样 +
// 容量水位与背压 + 崩溃恢复。
//
// 存储模型：
//   - 序列（series）：metric + tags 有序归一化后杂凑为稳定 ID；目录
//     <dir>/series/<hash16>/，其中 name 文件记录 metric/tags 映射；
//   - 段（segment）：每序列一个活动段追加写，满 SegmentPoints 封段；
//     raw 段 16B/点（ts+value）；rollup 段 48B/桶（ts+min+max+sum+count+last）；
//   - 崩溃一致性：记录定长，读取时尾部残缺记录自动忽略；重开时原活动段
//     一律按封段处理（新写入从新段开始），保证残尾永不被续写污染；
//   - flush：活动段缓冲按 FlushInterval 周期刷盘；Close/Flush 显式刷盘
//     （断电窗口 ≤ flush 周期，登记 KI §39）。
package tsdb

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"edgeflow/pkg/log"
)

// 段文件常量。
const (
	segmentMagic     = "EFTS0001" // 8B magic
	segmentHeaderLen = 32         // 头长度：magic[8] + kind[4] + reserved[4] + bucketMs[8] + createdMs[8]
	rawRecordLen     = 16         // ts int64 + value float64
	rollupRecordLen  = 48         // ts + min + max + sum + count(int64) + last

	kindRaw    uint32 = 0
	kindRollup uint32 = 1

	// DefaultSegmentPoints 是段滚动默认点数。
	DefaultSegmentPoints = 4096
	// DefaultFlushInterval 是默认刷盘周期。
	DefaultFlushInterval = time.Second
	// DefaultDownsampleEvery 是默认降采样桶宽。
	DefaultDownsampleEvery = time.Minute
)

// 哨兵错误。
var (
	// ErrClosed 表示库已关闭（后续写入拒绝）。
	ErrClosed = errors.New("tsdb: 库已关闭")
	// ErrBackpressure 表示容量水位超限且泄洪不足，本次写入被拒绝。
	ErrBackpressure = errors.New("tsdb: 容量水位超限（背压拒绝写入）")
)

// Point 是一条时序点。Metric 为序列键（装配约定 device/namespace/property）；
// Tags 参与序列身份（同 Metric 不同 Tags 是不同序列）。
type Point struct {
	Metric string            `json:"metric"`
	Tags   map[string]string `json:"tags,omitempty"`
	Ts     int64             `json:"ts"` // 毫秒时间戳
	Value  float64           `json:"value"`
}

// AggFn 是聚合函数类型。
type AggFn string

// 支持的聚合函数。
const (
	AggAvg   AggFn = "avg"
	AggMin   AggFn = "min"
	AggMax   AggFn = "max"
	AggCount AggFn = "count"
	AggSum   AggFn = "sum"
)

// Bucket 是一个聚合窗口结果：Ts=窗起点（毫秒）；count 时 Value=点数。
type Bucket struct {
	Ts    int64   `json:"ts"`
	Value float64 `json:"value"`
	Count int64   `json:"count"`
}

// Options 是 Open 的配置。Retention/DownsampleAfter 为 0 表示对应策略禁用。
type Options struct {
	Dir             string        // 数据目录（必填）
	MaxBytes        int64         // 容量水位（0=不限）
	Retention       time.Duration // 原始/聚合数据保留期（0=不裁剪）
	DownsampleAfter time.Duration // 超过此年龄的 raw 封段降采样（0=不降采样）
	DownsampleEvery time.Duration // 降采样桶宽（默认 1m）
	SegmentPoints   int           // 段滚动点数（默认 4096）
	FlushInterval   time.Duration // 刷盘周期（默认 1s）
}

// Stats 是库的运行统计快照。
type Stats struct {
	Series         int   `json:"series"`
	Points         int64 `json:"points"`
	SealedSegments int   `json:"sealedSegments"`
	Bytes          int64 `json:"bytes"`
	DroppedWrites  int64 `json:"droppedWrites"`
}

// RetentionResult 是一次保留/降采样执行的结果。
type RetentionResult struct {
	Downsampled int   `json:"downsampled"` // 降采样替换的段数
	Deleted     int   `json:"deleted"`     // 删除的段数
	BytesFreed  int64 `json:"bytesFreed"`  // 释放字节
}

// DB 是时序库实例（并发安全）。
type DB struct {
	opts Options

	mu      sync.Mutex
	closed  bool
	series  map[string]*series // by id
	points  int64
	bytes   int64
	dropped int64
	seq     uint64 // 段文件序号（进程内递增，保证文件名唯一）

	flushStop chan struct{}
	flushDone chan struct{}
}

// segment 是一个段文件。仅活动段持有 file/bw（append 写入）；
// 封段/加载段的 file/bw 为 nil。
type segment struct {
	path      string
	kind      uint32
	bucketMs  int64
	createdMs int64
	minTs     int64
	maxTs     int64
	count     int64 // 记录数（raw=点，rollup=桶）
	size      int64 // 文件字节（含头）

	file *os.File      // 仅活动段
	bw   *bufio.Writer // 仅活动段
}

// series 是一个序列。mu 保护 active/sealed 的变更（写/封/替换/删除）。
type series struct {
	id     string
	metric string
	tags   map[string]string
	dir    string

	mu     sync.Mutex
	active *segment
	sealed []*segment
}

// Open 打开（或创建）一个时序库：扫描目录重建序列清单与段索引。
// 单个损坏段 Warn 跳过（不阻断；其余序列可用）。
func Open(opts Options) (*DB, error) {
	if opts.Dir == "" {
		return nil, errors.New("tsdb: Options.Dir 必填")
	}
	if opts.SegmentPoints <= 0 {
		opts.SegmentPoints = DefaultSegmentPoints
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = DefaultFlushInterval
	}
	if opts.DownsampleEvery <= 0 {
		opts.DownsampleEvery = DefaultDownsampleEvery
	}
	if err := os.MkdirAll(filepath.Join(opts.Dir, "series"), 0o755); err != nil {
		return nil, fmt.Errorf("tsdb: 创建数据目录失败: %w", err)
	}
	db := &DB{
		opts:      opts,
		series:    make(map[string]*series),
		flushStop: make(chan struct{}),
		flushDone: make(chan struct{}),
	}
	if err := db.load(); err != nil {
		return nil, err
	}
	go db.flushLoop()
	return db, nil
}

// load 扫描 series/ 目录重建内存索引。
func (db *DB) load() error {
	root := filepath.Join(db.opts.Dir, "series")
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("tsdb: 扫描数据目录失败: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		nameRaw, err := os.ReadFile(filepath.Join(dir, "name"))
		if err != nil {
			log.Warnf("tsdb: 序列目录 %s 缺少 name 文件，跳过", e.Name())
			continue
		}
		var nm struct {
			Metric string            `json:"metric"`
			Tags   map[string]string `json:"tags"`
		}
		if err := json.Unmarshal(nameRaw, &nm); err != nil || nm.Metric == "" {
			log.Warnf("tsdb: 序列 %s name 文件损坏，跳过", e.Name())
			continue
		}
		s := &series{id: e.Name(), metric: nm.Metric, tags: nm.Tags, dir: dir}
		files, err := os.ReadDir(dir)
		if err != nil {
			log.Warnf("tsdb: 读取序列 %s 目录失败: %v", e.Name(), err)
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasPrefix(f.Name(), "seg-") || !strings.HasSuffix(f.Name(), ".dat") {
				continue
			}
			seg, err := loadSegment(filepath.Join(dir, f.Name()))
			if err != nil {
				log.Warnf("tsdb: 段文件 %s 损坏，跳过: %v", f.Name(), err)
				continue
			}
			s.sealed = append(s.sealed, seg)
			db.points += seg.count
			db.bytes += seg.size
		}
		sort.Slice(s.sealed, func(i, j int) bool { return cmpSegments(s.sealed[i], s.sealed[j]) < 0 })
		db.series[s.id] = s
	}
	return nil
}

// loadSegment 读取段头与统计（点数/时间范围）；尾部残缺记录自动忽略。
func loadSegment(path string) (*segment, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	hdr := make([]byte, segmentHeaderLen)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return nil, fmt.Errorf("读段头失败: %w", err)
	}
	if string(hdr[:8]) != segmentMagic {
		return nil, fmt.Errorf("magic 不符")
	}
	kind := binary.LittleEndian.Uint32(hdr[8:12])
	if kind != kindRaw && kind != kindRollup {
		return nil, fmt.Errorf("未知段类型 %d", kind)
	}
	seg := &segment{
		path:      path,
		kind:      kind,
		bucketMs:  int64(binary.LittleEndian.Uint64(hdr[16:24])),
		createdMs: int64(binary.LittleEndian.Uint64(hdr[24:32])),
	}
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	recLen := recordLen(seg.kind)
	cnt := (st.Size() - segmentHeaderLen) / recLen // 尾部残缺忽略
	if cnt < 0 {
		cnt = 0
	}
	seg.size = st.Size()
	// 扫描时间范围（段 ≤ SegmentPoints 条，开销可控）
	buf := make([]byte, recLen)
	first := true
	for i := int64(0); i < cnt; i++ {
		if _, err := io.ReadFull(f, buf); err != nil {
			break
		}
		ts := int64(binary.LittleEndian.Uint64(buf[:8]))
		if first {
			seg.minTs, seg.maxTs = ts, ts
			first = false
		} else {
			if ts < seg.minTs {
				seg.minTs = ts
			}
			if ts > seg.maxTs {
				seg.maxTs = ts
			}
		}
	}
	seg.count = cnt
	return seg, nil
}

// recordLen 返回段类型的记录长度。
func recordLen(kind uint32) int64 {
	if kind == kindRollup {
		return rollupRecordLen
	}
	return rawRecordLen
}

// flushLoop 周期刷盘活动段。
func (db *DB) flushLoop() {
	defer close(db.flushDone)
	ticker := time.NewTicker(db.opts.FlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := db.Flush(); err != nil {
				log.Warnf("tsdb: 周期刷盘失败: %v", err)
			}
		case <-db.flushStop:
			return
		}
	}
}

// Flush 刷盘全部活动段缓冲。
func (db *DB) Flush() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}
	var firstErr error
	for _, s := range db.series {
		s.mu.Lock()
		if s.active != nil && s.active.bw != nil {
			if err := s.active.bw.Flush(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		s.mu.Unlock()
	}
	return firstErr
}

// Close 刷盘并关闭库（幂等）。关闭后写入返回 ErrClosed。
func (db *DB) Close() error {
	db.mu.Lock()
	if db.closed {
		db.mu.Unlock()
		return nil
	}
	db.closed = true
	close(db.flushStop)
	db.mu.Unlock()
	<-db.flushDone

	db.mu.Lock()
	defer db.mu.Unlock()
	var firstErr error
	for _, s := range db.series {
		s.mu.Lock()
		if s.active != nil {
			if err := s.sealActiveLocked(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		s.mu.Unlock()
	}
	return firstErr
}

// Stats 返回运行统计快照。
func (db *DB) Stats() Stats {
	db.mu.Lock()
	defer db.mu.Unlock()
	st := Stats{
		Series:        len(db.series),
		Points:        db.points,
		Bytes:         db.bytes,
		DroppedWrites: db.dropped,
	}
	for _, s := range db.series {
		st.SealedSegments += len(s.sealed)
	}
	return st
}

// Series 返回全部序列键（metric，含 tags 时以 {k=v} 归一化后缀）。
func (db *DB) Series() []string {
	db.mu.Lock()
	defer db.mu.Unlock()
	out := make([]string, 0, len(db.series))
	for _, s := range db.series {
		out = append(out, seriesKey(s.metric, s.tags))
	}
	sort.Strings(out)
	return out
}

// —— 写入路径 ——

// WriteOne 写单条（metric 无 tags）。
func (db *DB) WriteOne(metric string, ts int64, v float64) error {
	return db.Write(Point{Metric: metric, Ts: ts, Value: v})
}

// Write 批量写入。任一条失败（如水位背压）返回错误；已写条目保留
// （追加语义：不做事务回滚——调用方按丢弃计数观测）。
func (db *DB) Write(points ...Point) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}
	if len(points) == 0 {
		return nil
	}
	// 容量水位：先泄洪（删最老封段），仍超则背压拒绝。
	if db.opts.MaxBytes > 0 && db.bytes > db.opts.MaxBytes {
		db.shedLocked()
		if db.bytes > db.opts.MaxBytes {
			db.dropped += int64(len(points))
			return ErrBackpressure
		}
	}
	var firstErr error
	for _, p := range points {
		if err := db.writeOneLocked(p); err != nil {
			db.dropped++
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// writeOneLocked 写入单条（db.mu 已持有）。
func (db *DB) writeOneLocked(p Point) error {
	if p.Metric == "" {
		return fmt.Errorf("tsdb: Point.Metric 为空")
	}
	s, err := db.seriesForLocked(p.Metric, p.Tags)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil {
		if err := db.openActiveSegmentLocked(s); err != nil {
			return err
		}
	}
	if s.active.count >= int64(db.opts.SegmentPoints) {
		if err := s.sealActiveLocked(); err != nil {
			return err
		}
		if err := db.openActiveSegmentLocked(s); err != nil {
			return err
		}
	}
	rec := make([]byte, rawRecordLen)
	binary.LittleEndian.PutUint64(rec[0:8], uint64(p.Ts))
	binary.LittleEndian.PutUint64(rec[8:16], math.Float64bits(p.Value))
	if s.active.bw != nil {
		if _, err := s.active.bw.Write(rec); err != nil {
			return fmt.Errorf("tsdb: 段写入失败: %w", err)
		}
	}
	a := s.active
	a.count++
	a.size += rawRecordLen
	if a.count == 1 || p.Ts < a.minTs {
		a.minTs = p.Ts
	}
	if a.count == 1 || p.Ts > a.maxTs {
		a.maxTs = p.Ts
	}
	db.points++
	db.bytes += rawRecordLen
	return nil
}

// seriesForLocked 按 metric+tags 取或创建序列（db.mu 已持有）。
func (db *DB) seriesForLocked(metric string, tags map[string]string) (*series, error) {
	id := seriesID(metric, tags)
	if s, ok := db.series[id]; ok {
		return s, nil
	}
	dir := filepath.Join(db.opts.Dir, "series", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("tsdb: 创建序列目录失败: %w", err)
	}
	nm, err := json.Marshal(struct {
		Metric string            `json:"metric"`
		Tags   map[string]string `json:"tags,omitempty"`
	}{metric, tags})
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "name"), nm, 0o644); err != nil {
		return nil, fmt.Errorf("tsdb: 写入序列名映射失败: %w", err)
	}
	s := &series{id: id, metric: metric, tags: tags, dir: dir}
	db.series[id] = s
	return s, nil
}

// openActiveSegmentLocked 为序列打开一个新活动段（db.mu + s.mu 已持有）。
// 文件名 seg-<createdMs>-<seq>.dat：seq 为进程内递增序号，保证同一毫秒
// 内连续建段（小段测试/高频封段）文件名不撞。
func (db *DB) openActiveSegmentLocked(s *series) error {
	created := time.Now().UnixMilli()
	db.seq++
	path := filepath.Join(s.dir, fmt.Sprintf("seg-%d-%d.dat", created, db.seq))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("tsdb: 创建段文件失败: %w", err)
	}
	if _, err := f.Write(encodeSegmentHeader(kindRaw, 0, created)); err != nil {
		_ = f.Close()
		return fmt.Errorf("tsdb: 写段头失败: %w", err)
	}
	bw := bufio.NewWriterSize(f, 64*1024)
	s.active = &segment{
		path: path, kind: kindRaw, createdMs: created,
		size: segmentHeaderLen, file: f, bw: bw,
	}
	db.bytes += segmentHeaderLen
	return nil
}

// sealActiveLocked 封段：刷盘并关闭文件句柄，移入 sealed（s.mu 已持有）。
func (s *series) sealActiveLocked() error {
	a := s.active
	if a == nil {
		return nil
	}
	var firstErr error
	if a.bw != nil {
		if err := a.bw.Flush(); err != nil {
			firstErr = err
		}
	}
	if a.file != nil {
		if err := a.file.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	a.file = nil
	a.bw = nil
	s.sealed = append(s.sealed, a)
	s.active = nil
	return firstErr
}

// cmpSegments 是段的全序比较：先 createdMs，后路径（同毫秒时顺序稳定，
// 保证“最老段”选择与加载排序确定性）。
func cmpSegments(a, b *segment) int {
	if a.createdMs != b.createdMs {
		if a.createdMs < b.createdMs {
			return -1
		}
		return 1
	}
	if a.path < b.path {
		return -1
	}
	if a.path > b.path {
		return 1
	}
	return 0
}

// encodeSegmentHeader 编码 32B 段头。
func encodeSegmentHeader(kind uint32, bucketMs, createdMs int64) []byte {
	hdr := make([]byte, segmentHeaderLen)
	copy(hdr[:8], segmentMagic)
	binary.LittleEndian.PutUint32(hdr[8:12], kind)
	binary.LittleEndian.PutUint32(hdr[12:16], 0)
	binary.LittleEndian.PutUint64(hdr[16:24], uint64(bucketMs))
	binary.LittleEndian.PutUint64(hdr[24:32], uint64(createdMs))
	return hdr
}

// —— 键与工具函数 ——

// seriesID 计算序列 ID：各字段有序归一化后 FNV-1a 杂凑（16 位十六进制）。
func seriesID(metric string, tags map[string]string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(metric))
	_, _ = h.Write([]byte{0})
	for _, k := range sortedTagKeys(tags) {
		_, _ = h.Write([]byte(k))
		_, _ = h.Write([]byte{'='})
		_, _ = h.Write([]byte(tags[k]))
		_, _ = h.Write([]byte{';'})
	}
	return fmt.Sprintf("%016x", h.Sum64())
}

// seriesKey 返回序列的可读键（metric，含 tags 后缀）。
func seriesKey(metric string, tags map[string]string) string {
	if len(tags) == 0 {
		return metric
	}
	var sb strings.Builder
	sb.WriteString(metric)
	sb.WriteString("{")
	for i, k := range sortedTagKeys(tags) {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(k)
		sb.WriteString("=")
		sb.WriteString(tags[k])
	}
	sb.WriteString("}")
	return sb.String()
}

// sortedTagKeys 返回排序后的 tags 键。
func sortedTagKeys(tags map[string]string) []string {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
