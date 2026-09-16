// v0.38.0 时序库测试（spec 0011）：写入/查询/聚合/保留/降采样/水位背压/
// 崩溃恢复/并发/边界。基准（Benchmark*）覆盖 720 条/s 场景，数字入文档。
package tsdb

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

// baseTs 是测试用固定基准时间戳（2023-11-14T22:13:20Z）。
const baseTs = int64(1_700_000_000_000)

// newTestDB 打开测试库（默认小段 4 点便于验证滚动）。
func newTestDB(t *testing.T, opts Options) *DB {
	t.Helper()
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	if opts.SegmentPoints <= 0 {
		opts.SegmentPoints = 4
	}
	db, err := Open(opts)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// writeN 顺序写 n 条（ts 从 start 起每毫秒一条，值由 fn 生成）。
func writeN(t *testing.T, db *DB, metric string, start, n int64, fn func(i int64) float64) {
	t.Helper()
	for i := int64(0); i < n; i++ {
		if err := db.WriteOne(metric, start+i, fn(i)); err != nil {
			t.Fatalf("写入第 %d 条失败: %v", i, err)
		}
	}
}

// queryLen 查询并返回结果条数。
func queryLen(t *testing.T, db *DB, metric string) int {
	t.Helper()
	pts, err := db.Query(metric, nil, 0, 0)
	if err != nil {
		t.Fatalf("Query 失败: %v", err)
	}
	return len(pts)
}

func TestOpenCreatesDirs(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer db.Close()
	if _, err := os.Stat(filepath.Join(dir, "series")); err != nil {
		t.Fatalf("series 目录未创建: %v", err)
	}
}

func TestOpenDefaults(t *testing.T) {
	db, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer db.Close()
	if db.opts.SegmentPoints != DefaultSegmentPoints {
		t.Fatalf("SegmentPoints 默认值错误: %d", db.opts.SegmentPoints)
	}
	if db.opts.FlushInterval != DefaultFlushInterval {
		t.Fatalf("FlushInterval 默认值错误: %v", db.opts.FlushInterval)
	}
	if db.opts.DownsampleEvery != DefaultDownsampleEvery {
		t.Fatalf("DownsampleEvery 默认值错误: %v", db.opts.DownsampleEvery)
	}
}

func TestOpenRequiresDir(t *testing.T) {
	if _, err := Open(Options{}); err == nil {
		t.Fatal("空 Dir 应报错")
	}
}

func TestWriteAndQueryBasic(t *testing.T) {
	db := newTestDB(t, Options{})
	if err := db.Write(
		Point{Metric: "dev/ns/temp", Ts: baseTs + 1, Value: 21.5},
		Point{Metric: "dev/ns/temp", Ts: baseTs + 2, Value: 22.5},
	); err != nil {
		t.Fatalf("批量写失败: %v", err)
	}
	pts, err := db.Query("dev/ns/temp", nil, 0, 0)
	if err != nil {
		t.Fatalf("Query 失败: %v", err)
	}
	if len(pts) != 2 || pts[0].Value != 21.5 || pts[1].Value != 22.5 {
		t.Fatalf("查询结果不符: %+v", pts)
	}
	if pts[0].Metric != "dev/ns/temp" {
		t.Fatalf("Metric 未回填: %q", pts[0].Metric)
	}
}

func TestQueryWindowBounds(t *testing.T) {
	db := newTestDB(t, Options{})
	writeN(t, db, "m", baseTs, 10, func(i int64) float64 { return float64(i) })
	// [from, to)：含 from、不含 to
	pts, err := db.Query("m", nil, baseTs+3, baseTs+6)
	if err != nil {
		t.Fatalf("Query 失败: %v", err)
	}
	if len(pts) != 3 || pts[0].Ts != baseTs+3 || pts[2].Ts != baseTs+5 {
		t.Fatalf("窗口边界不符: %+v", pts)
	}
	// from=0 不限起点
	pts, _ = db.Query("m", nil, 0, baseTs+2)
	if len(pts) != 2 {
		t.Fatalf("from=0 语义不符: %d", len(pts))
	}
	// to=0 不限终点
	pts, _ = db.Query("m", nil, baseTs+8, 0)
	if len(pts) != 2 {
		t.Fatalf("to=0 语义不符: %d", len(pts))
	}
	// 空窗
	pts, _ = db.Query("m", nil, baseTs+100, baseTs+200)
	if len(pts) != 0 {
		t.Fatalf("空窗应为空: %d", len(pts))
	}
}

func TestSeriesIsolation(t *testing.T) {
	db := newTestDB(t, Options{})
	if err := db.WriteOne("a/x/1", baseTs, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.WriteOne("b/x/2", baseTs, 2); err != nil {
		t.Fatal(err)
	}
	if n := queryLen(t, db, "a/x/1"); n != 1 {
		t.Fatalf("序列 a 条数 %d", n)
	}
	if n := queryLen(t, db, "b/x/2"); n != 1 {
		t.Fatalf("序列 b 条数 %d", n)
	}
	if n := queryLen(t, db, "c/x/3"); n != 0 {
		t.Fatalf("未知序列应为空: %d", n)
	}
}

func TestTagsIdentity(t *testing.T) {
	db := newTestDB(t, Options{})
	if err := db.Write(
		Point{Metric: "m", Tags: map[string]string{"rack": "a"}, Ts: baseTs, Value: 1},
		Point{Metric: "m", Tags: map[string]string{"rack": "b"}, Ts: baseTs, Value: 2},
	); err != nil {
		t.Fatal(err)
	}
	ptsA, _ := db.Query("m", map[string]string{"rack": "a"}, 0, 0)
	ptsB, _ := db.Query("m", map[string]string{"rack": "b"}, 0, 0)
	if len(ptsA) != 1 || len(ptsB) != 1 || ptsA[0].Value == ptsB[0].Value {
		t.Fatalf("tags 身份区分失败: A=%+v B=%+v", ptsA, ptsB)
	}
	keys := db.Series()
	if len(keys) != 2 || keys[0] != "m{rack=a}" || keys[1] != "m{rack=b}" {
		t.Fatalf("序列键不符: %v", keys)
	}
}

func TestSegmentRolling(t *testing.T) {
	db := newTestDB(t, Options{SegmentPoints: 4})
	writeN(t, db, "m", baseTs, 10, func(i int64) float64 { return float64(i) })
	st := db.Stats()
	if st.SealedSegments != 2 {
		t.Fatalf("封段数不符: %d", st.SealedSegments)
	}
	if st.Points != 10 {
		t.Fatalf("点数不符: %d", st.Points)
	}
	if n := queryLen(t, db, "m"); n != 10 {
		t.Fatalf("查询跨段条数 %d", n)
	}
}

func TestQueryOrdering(t *testing.T) {
	db := newTestDB(t, Options{})
	// 乱序写入
	for _, ts := range []int64{baseTs + 3, baseTs + 1, baseTs + 2} {
		if err := db.WriteOne("m", ts, float64(ts)); err != nil {
			t.Fatal(err)
		}
	}
	pts, _ := db.Query("m", nil, 0, 0)
	if len(pts) != 3 || pts[0].Ts != baseTs+1 || pts[2].Ts != baseTs+3 {
		t.Fatalf("乱序写入后查询未按时间排序: %+v", pts)
	}
}

func TestAggregateFunctions(t *testing.T) {
	db := newTestDB(t, Options{})
	// 桶1（baseTs..+60s）：1,2,3；桶2（+60s..+120s）：10,20
	for _, p := range []Point{
		{Metric: "m", Ts: baseTs + 0, Value: 1},
		{Metric: "m", Ts: baseTs + 1_000, Value: 2},
		{Metric: "m", Ts: baseTs + 2_000, Value: 3},
		{Metric: "m", Ts: baseTs + 60_000, Value: 10},
		{Metric: "m", Ts: baseTs + 61_000, Value: 20},
	} {
		if err := db.Write(p); err != nil {
			t.Fatal(err)
		}
	}
	from, to, win := baseTs, baseTs+120_000, int64(60_000)
	check := func(fn AggFn, want []float64) {
		t.Helper()
		bs, err := db.Aggregate("m", nil, from, to, win, fn)
		if err != nil {
			t.Fatalf("%s 聚合失败: %v", fn, err)
		}
		if len(bs) != len(want) {
			t.Fatalf("%s 桶数 %d != %d", fn, len(bs), len(want))
		}
		for i := range want {
			if bs[i].Value != want[i] {
				t.Fatalf("%s 第 %d 桶值 %v != %v", fn, i, bs[i].Value, want[i])
			}
		}
	}
	check(AggAvg, []float64{2, 15})
	check(AggMin, []float64{1, 10})
	check(AggMax, []float64{3, 20})
	check(AggCount, []float64{3, 2})
	check(AggSum, []float64{6, 30})
	// 桶 Ts 对齐断言
	bs, _ := db.Aggregate("m", nil, from, to, win, AggCount)
	if bs[0].Ts != baseTs || bs[1].Ts != baseTs+60_000 {
		t.Fatalf("桶对齐不符: %+v", bs)
	}
}

func TestAggregateValidation(t *testing.T) {
	db := newTestDB(t, Options{})
	if _, err := db.Aggregate("m", nil, 0, 0, 0, AggAvg); err == nil {
		t.Fatal("window=0 应报错")
	}
	if _, err := db.Aggregate("m", nil, 0, 0, 60_000, AggFn("p99")); err == nil {
		t.Fatal("未知聚合函数应报错")
	}
}

func TestPersistenceReopen(t *testing.T) {
	dir := t.TempDir()
	db := newTestDB(t, Options{Dir: dir, SegmentPoints: 4})
	writeN(t, db, "m", baseTs, 10, func(i int64) float64 { return float64(i) * 2 })
	if err := db.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	db2 := newTestDB(t, Options{Dir: dir, SegmentPoints: 4})
	st := db2.Stats()
	if st.Points != 10 {
		t.Fatalf("重开后点数 %d != 10", st.Points)
	}
	if st.SealedSegments != 3 {
		t.Fatalf("重开后封段数 %d != 3", st.SealedSegments)
	}
	pts, err := db2.Query("m", nil, 0, 0)
	if err != nil || len(pts) != 10 {
		t.Fatalf("重开后查询失败: n=%d err=%v", len(pts), err)
	}
	if pts[9].Value != 18 {
		t.Fatalf("重开后末值 %v != 18", pts[9].Value)
	}
}

func TestFlushVisibilityWithoutClose(t *testing.T) {
	dir := t.TempDir()
	db1 := newTestDB(t, Options{Dir: dir, SegmentPoints: 4096})
	if err := db1.WriteOne("m", baseTs, 7); err != nil {
		t.Fatal(err)
	}
	if err := db1.Flush(); err != nil {
		t.Fatalf("Flush 失败: %v", err)
	}
	db2 := newTestDB(t, Options{Dir: dir, SegmentPoints: 4096})
	if n := queryLen(t, db2, "m"); n != 1 {
		t.Fatalf("Flush 后未 Close 应可见: %d", n)
	}
	if err := db1.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCorruptTailIgnored(t *testing.T) {
	dir := t.TempDir()
	db := newTestDB(t, Options{Dir: dir, SegmentPoints: 4})
	writeN(t, db, "m", baseTs, 6, func(i int64) float64 { return float64(i) })
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// 向每个段追加残缺尾巴（模拟断电半条记录）
	files, _ := filepath.Glob(filepath.Join(dir, "series", "*", "seg-*.dat"))
	if len(files) == 0 {
		t.Fatal("未找到段文件")
	}
	for _, f := range files {
		fh, err := os.OpenFile(f, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fh.Write([]byte{1, 2, 3, 4, 5}); err != nil {
			t.Fatal(err)
		}
		_ = fh.Close()
	}
	db2 := newTestDB(t, Options{Dir: dir, SegmentPoints: 4})
	if n := queryLen(t, db2, "m"); n != 6 {
		t.Fatalf("残尾应被忽略，条数 %d != 6", n)
	}
}

func TestCorruptSegmentSkipped(t *testing.T) {
	dir := t.TempDir()
	db := newTestDB(t, Options{Dir: dir, SegmentPoints: 4})
	if err := db.WriteOne("m", baseTs, 42); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// 伪造一个损坏段（magic 不符）
	seriesDirs, _ := filepath.Glob(filepath.Join(dir, "series", "*"))
	if len(seriesDirs) != 1 {
		t.Fatalf("序列目录数 %d", len(seriesDirs))
	}
	bad := filepath.Join(seriesDirs[0], "seg-999999-999.dat")
	if err := os.WriteFile(bad, []byte("not-a-tsdb-segment-file"), 0o644); err != nil {
		t.Fatal(err)
	}
	db2 := newTestDB(t, Options{Dir: dir, SegmentPoints: 4})
	if n := queryLen(t, db2, "m"); n != 1 {
		t.Fatalf("坏段应跳过，正常数据可查: %d", n)
	}
}

func TestRetentionDeletesExpired(t *testing.T) {
	now := time.Now().UnixMilli()
	db := newTestDB(t, Options{Retention: time.Hour, DownsampleAfter: 0})
	writeN(t, db, "m", now-2*3_600_000, 9, func(i int64) float64 { return float64(i) })
	res, err := db.RunRetention(now)
	if err != nil {
		t.Fatalf("RunRetention 失败: %v", err)
	}
	if res.Deleted != 2 {
		t.Fatalf("过期删除段数 %d != 2", res.Deleted)
	}
	if n := queryLen(t, db, "m"); n != 1 {
		t.Fatalf("活动段应保留: %d", n)
	}
}

func TestRetentionNoopWithoutPolicy(t *testing.T) {
	now := time.Now().UnixMilli()
	db := newTestDB(t, Options{})
	writeN(t, db, "m", now-48*3_600_000, 9, func(i int64) float64 { return float64(i) })
	res, err := db.RunRetention(now)
	if err != nil {
		t.Fatalf("RunRetention 失败: %v", err)
	}
	if res.Deleted != 0 || res.Downsampled != 0 {
		t.Fatalf("无策略应 no-op: %+v", res)
	}
	if n := queryLen(t, db, "m"); n != 9 {
		t.Fatalf("无策略不应丢数据: %d", n)
	}
}

func TestDownsampleRewritesSegment(t *testing.T) {
	now := time.Now().UnixMilli()
	dir := t.TempDir()
	db := newTestDB(t, Options{Dir: dir, Retention: 6 * time.Hour, DownsampleAfter: time.Hour, DownsampleEvery: time.Minute})
	start := now - 2*3_600_000
	writeN(t, db, "m", start, 5, func(i int64) float64 { return float64((i + 1) * 10) }) // 10,20,30,40 封段；50 活动
	res, err := db.RunRetention(now)
	if err != nil {
		t.Fatalf("RunRetention 失败: %v", err)
	}
	if res.Downsampled != 1 {
		t.Fatalf("降采样段数 %d != 1", res.Downsampled)
	}
	// 段文件有一个已变为 rollup
	files, _ := filepath.Glob(filepath.Join(dir, "series", "*", "seg-*.dat"))
	rollups := 0
	for _, f := range files {
		seg, err := loadSegment(f)
		if err != nil {
			t.Fatalf("载荷段失败 %s: %v", f, err)
		}
		if seg.kind == kindRollup {
			rollups++
		}
	}
	if rollups != 1 {
		t.Fatalf("rollup 段数 %d != 1", rollups)
	}
	// 查询：rollup 代表点 avg=25 + 活动段 50
	pts, err := db.Query("m", nil, 0, 0)
	if err != nil {
		t.Fatalf("Query 失败: %v", err)
	}
	if len(pts) != 2 {
		t.Fatalf("查询条数 %d != 2: %+v", len(pts), pts)
	}
	if pts[0].Value != 25 {
		t.Fatalf("rollup 代表点 %v != 25", pts[0].Value)
	}
	if pts[0].Ts%60_000 != 0 {
		t.Fatalf("桶 Ts 未对齐分钟网格: %d", pts[0].Ts)
	}
	if pts[1].Value != 50 {
		t.Fatalf("活动段值 %v != 50", pts[1].Value)
	}
}

func TestDownsampleThenRetention(t *testing.T) {
	now := time.Now().UnixMilli()
	db := newTestDB(t, Options{Retention: 3 * time.Hour, DownsampleAfter: time.Hour, DownsampleEvery: time.Minute})
	writeN(t, db, "m", now-2*3_600_000, 9, func(i int64) float64 { return float64(i) })
	res1, err := db.RunRetention(now)
	if err != nil {
		t.Fatalf("第一轮失败: %v", err)
	}
	if res1.Downsampled != 2 || res1.Deleted != 0 {
		t.Fatalf("第一轮应降采样 2 段: %+v", res1)
	}
	res2, err := db.RunRetention(now + 2*3_600_000) // 时间前进 2h → rollup 超 3h 保留
	if err != nil {
		t.Fatalf("第二轮失败: %v", err)
	}
	if res2.Deleted != 2 {
		t.Fatalf("第二轮应删除 2 个 rollup 段: %+v", res2)
	}
	if n := queryLen(t, db, "m"); n != 1 {
		t.Fatalf("活动段保留: %d", n)
	}
}

func TestAggregateRollupWeighted(t *testing.T) {
	now := time.Now().UnixMilli()
	db := newTestDB(t, Options{Retention: 24 * time.Hour, DownsampleAfter: time.Hour, DownsampleEvery: time.Minute})
	// 9 条：段1（1..4 封）、段2（5..8 封）、段3（9 活动）
	writeN(t, db, "m", now-2*3_600_000, 9, func(i int64) float64 { return float64(i + 1) })
	if _, err := db.RunRetention(now); err != nil {
		t.Fatalf("RunRetention 失败: %v", err)
	}
	// 加权均值 = (1+..+9)/9 = 5（简单平均会得 2.5、6.5、9 → 6）
	bs, err := db.Aggregate("m", nil, 0, 0, 1_000_000_000, AggAvg)
	if err != nil {
		t.Fatalf("Aggregate 失败: %v", err)
	}
	if len(bs) != 1 {
		t.Fatalf("桶数 %d != 1", len(bs))
	}
	if bs[0].Value != 5 {
		t.Fatalf("加权均值 %v != 5", bs[0].Value)
	}
	if bs[0].Count != 9 {
		t.Fatalf("计数 %d != 9", bs[0].Count)
	}
}

func TestShedEvictsOldest(t *testing.T) {
	dir := t.TempDir()
	db := newTestDB(t, Options{Dir: dir, SegmentPoints: 4, MaxBytes: 100})
	// 一次写 9 条：段1/2 封，段3 活动 1 条；bytes=32*3+9*16=240
	writeN(t, db, "m", baseTs, 9, func(i int64) float64 { return float64(i) })
	// 再写 1 条触发泄洪：删段1、段2（-192）→ 48 < 100 后继续写
	if err := db.WriteOne("m", baseTs+100, 99); err != nil {
		t.Fatalf("泄洪后写入应成功: %v", err)
	}
	st := db.Stats()
	if st.SealedSegments != 0 {
		t.Fatalf("泄洪后封段数 %d != 0", st.SealedSegments)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "series", "*", "seg-*.dat"))
	if len(files) != 1 {
		t.Fatalf("泄洪后应剩 1 个段文件: %d", len(files))
	}
	if n := queryLen(t, db, "m"); n != 2 {
		t.Fatalf("泄洪后保留活动段数据: %d != 2", n)
	}
}

func TestBackpressureRejects(t *testing.T) {
	db := newTestDB(t, Options{MaxBytes: 40})
	if err := db.WriteOne("m", baseTs, 1); err != nil {
		t.Fatalf("首条应可写: %v", err)
	}
	err := db.WriteOne("m", baseTs+1, 2)
	if !errors.Is(err, ErrBackpressure) {
		t.Fatalf("超水位应背压拒绝: %v", err)
	}
	if st := db.Stats(); st.DroppedWrites != 1 {
		t.Fatalf("丢弃计数 %d != 1", st.DroppedWrites)
	}
	if n := queryLen(t, db, "m"); n != 1 {
		t.Fatalf("背压不应破坏已有数据: %d", n)
	}
}

func TestWritesRejectedAfterClose(t *testing.T) {
	db := newTestDB(t, Options{})
	if err := db.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	if err := db.WriteOne("m", baseTs, 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("关闭后写入应 ErrClosed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("重复 Close 应幂等: %v", err)
	}
}

func TestQueryAfterClose(t *testing.T) {
	db := newTestDB(t, Options{})
	if err := db.WriteOne("m", baseTs, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Query("m", nil, 0, 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("关闭后查询应 ErrClosed: %v", err)
	}
}

func TestConcurrentWrites(t *testing.T) {
	db := newTestDB(t, Options{SegmentPoints: 64})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			metric := fmt.Sprintf("m/%d", g)
			for i := 0; i < 50; i++ {
				if err := db.WriteOne(metric, baseTs+int64(i), float64(i)); err != nil {
					t.Errorf("并发写失败: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	st := db.Stats()
	if st.Points != 400 {
		t.Fatalf("并发写点数 %d != 400", st.Points)
	}
	for g := 0; g < 8; g++ {
		if n := queryLen(t, db, fmt.Sprintf("m/%d", g)); n != 50 {
			t.Fatalf("序列 %d 条数 %d != 50", g, n)
		}
	}
}

func TestConcurrentWriteQuery(t *testing.T) {
	db := newTestDB(t, Options{SegmentPoints: 64})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			_ = db.WriteOne("m", baseTs+int64(i), float64(i))
		}
	}()
	for i := 0; i < 50; i++ {
		if _, err := db.Query("m", nil, 0, 0); err != nil {
			t.Fatalf("并发查询失败: %v", err)
		}
	}
	<-done
	// Close 后点数一致
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if st := db.Stats(); st.Points != 200 {
		t.Fatalf("最终点数 %d != 200", st.Points)
	}
}

func TestRunRetentionLoopStops(t *testing.T) {
	db := newTestDB(t, Options{Retention: time.Hour})
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		db.RunRetentionLoop(10*time.Millisecond, stop)
		close(done)
	}()
	time.Sleep(40 * time.Millisecond)
	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("保留循环未退出")
	}
}

func TestSeriesKeySorting(t *testing.T) {
	if got := seriesKey("m", map[string]string{"b": "2", "a": "1"}); got != "m{a=1,b=2}" {
		t.Fatalf("序列键排序不符: %s", got)
	}
	if got := seriesKey("m", nil); got != "m" {
		t.Fatalf("无 tags 键不符: %s", got)
	}
	// 同 tags 不同声明顺序 → 同 ID
	if seriesID("m", map[string]string{"a": "1", "b": "2"}) != seriesID("m", map[string]string{"b": "2", "a": "1"}) {
		t.Fatal("tags 顺序不应影响序列 ID")
	}
}

func TestEmptyWriteAndNilQuery(t *testing.T) {
	db := newTestDB(t, Options{})
	if err := db.Write(); err != nil {
		t.Fatalf("空写入应成功: %v", err)
	}
	if err := db.WriteOne("", baseTs, 1); err == nil {
		t.Fatal("空 metric 应报错")
	}
}

// —— 基准（720 条/s 场景：200 序列 × 36 点/批 ≈ 7200 条/批）——

func newBenchDB(b *testing.B, opts Options) *DB {
	b.Helper()
	if opts.Dir == "" {
		opts.Dir = b.TempDir()
	}
	if opts.SegmentPoints <= 0 {
		opts.SegmentPoints = 65536
	}
	db, err := Open(opts)
	if err != nil {
		b.Fatalf("Open 失败: %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })
	return db
}

func BenchmarkWriteSingleSeries(b *testing.B) {
	db := newBenchDB(b, Options{})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := db.WriteOne("bench/single/temp", baseTs+int64(i), float64(i%100)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWriteParallelSeries(b *testing.B) {
	db := newBenchDB(b, Options{})
	b.SetParallelism(8)
	b.RunParallel(func(pb *testing.PB) {
		var i int64
		for pb.Next() {
			if err := db.WriteOne(fmt.Sprintf("bench/par/%d", i%16), baseTs+i, float64(i%100)); err != nil {
				b.Fatal(err)
			}
			i++
		}
	})
}

// BenchmarkWrite720Scenario 模拟"720 条/s"采集负载：每批 200 序列 × 36 点
// （=7200 条 ≈ 10s 数据），批内逐条写入。
func BenchmarkWrite720Scenario(b *testing.B) {
	db := newBenchDB(b, Options{})
	pts := make([]Point, 0, 7200)
	for s := 0; s < 200; s++ {
		metric := fmt.Sprintf("bench/dev-%03d/ns/prop", s)
		for j := 0; j < 36; j++ {
			pts = append(pts, Point{Metric: metric, Ts: baseTs + int64(j)*250, Value: float64(j)})
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := db.Write(pts...); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQueryWindow(b *testing.B) {
	db := newBenchDB(b, Options{SegmentPoints: 4096})
	for i := 0; i < 100_000; i++ {
		if err := db.WriteOne("bench/query/x", baseTs+int64(i)*100, float64(i%50)); err != nil {
			b.Fatal(err)
		}
	}
	if err := db.Flush(); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.Query("bench/query/x", nil, baseTs, baseTs+1_000_000); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAggregateWindow(b *testing.B) {
	db := newBenchDB(b, Options{SegmentPoints: 4096})
	for i := 0; i < 50_000; i++ {
		if err := db.WriteOne("bench/agg/x", baseTs+int64(i)*100, float64(i%50)); err != nil {
			b.Fatal(err)
		}
	}
	if err := db.Flush(); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.Aggregate("bench/agg/x", nil, baseTs, baseTs+5_000_000, 60_000, AggAvg); err != nil {
			b.Fatal(err)
		}
	}
}

var _ = sort.Ints // 保留 sort 引用（部分构建标签下可能未用）
