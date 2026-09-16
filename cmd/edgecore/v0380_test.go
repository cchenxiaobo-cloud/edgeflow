// v0.38.0 测试锚（边缘侧）：时序库装配（开关解析 / Options 默认与回退 /
// 采样管道 sink / 打开失败降级）。与 spec 0011 US-6 对应。
package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"edgeflow/pkg/rules"
	"edgeflow/pkg/tsdb"
)

func TestTSDBEnabledSwitch(t *testing.T) {
	t.Setenv("EDGEFLOW_EDGECORE_TSDB", "")
	if tsdbEnabled() {
		t.Fatal("缺省（空）应关闭")
	}
	t.Setenv("EDGEFLOW_EDGECORE_TSDB", "off")
	if tsdbEnabled() {
		t.Fatal("off 应关闭")
	}
	t.Setenv("EDGEFLOW_EDGECORE_TSDB", "on")
	if !tsdbEnabled() {
		t.Fatal("on 应启用")
	}
}

func TestTSDBOptionsFromEnvDefaults(t *testing.T) {
	t.Setenv("EDGEFLOW_EDGECORE_TSDB_DIR", "")
	t.Setenv("EDGEFLOW_EDGECORE_TSDB_RETENTION", "")
	t.Setenv("EDGEFLOW_EDGECORE_TSDB_MAX_MB", "")
	t.Setenv("EDGEFLOW_EDGECORE_TSDB_DOWNSAMPLE", "")
	opts := tsdbOptionsFromEnv("/data/edgeflow.db")
	if opts.Dir != filepath.Join("/data", "tsdb") {
		t.Fatalf("默认目录错误: %s", opts.Dir)
	}
	if opts.Retention != 72*time.Hour {
		t.Fatalf("默认保留错误: %v", opts.Retention)
	}
	if opts.MaxBytes != 256<<20 {
		t.Fatalf("默认水位错误: %d", opts.MaxBytes)
	}
	if opts.DownsampleAfter != time.Hour || opts.DownsampleEvery != time.Minute {
		t.Fatalf("默认降采样错误: %v/%v", opts.DownsampleAfter, opts.DownsampleEvery)
	}
}

func TestTSDBOptionsFromEnvOverrides(t *testing.T) {
	td := t.TempDir()
	t.Setenv("EDGEFLOW_EDGECORE_TSDB_DIR", td)
	t.Setenv("EDGEFLOW_EDGECORE_TSDB_RETENTION", "24h")
	t.Setenv("EDGEFLOW_EDGECORE_TSDB_MAX_MB", "64")
	t.Setenv("EDGEFLOW_EDGECORE_TSDB_DOWNSAMPLE", "off")
	opts := tsdbOptionsFromEnv("/data/edgeflow.db")
	if opts.Dir != td {
		t.Fatalf("目录覆盖失败: %s", opts.Dir)
	}
	if opts.Retention != 24*time.Hour {
		t.Fatalf("保留覆盖失败: %v", opts.Retention)
	}
	if opts.MaxBytes != 64<<20 {
		t.Fatalf("水位覆盖失败: %d", opts.MaxBytes)
	}
	if opts.DownsampleAfter != 0 {
		t.Fatalf("降采样关闭失败: %v", opts.DownsampleAfter)
	}
}

func TestTSDBOptionsFromEnvInvalidFallback(t *testing.T) {
	t.Setenv("EDGEFLOW_EDGECORE_TSDB_RETENTION", "abc")
	t.Setenv("EDGEFLOW_EDGECORE_TSDB_MAX_MB", "-5")
	opts := tsdbOptionsFromEnv("/data/edgeflow.db")
	if opts.Retention != 72*time.Hour {
		t.Fatalf("非法保留应回退默认: %v", opts.Retention)
	}
	if opts.MaxBytes != 256<<20 {
		t.Fatalf("非法水位应回退默认: %d", opts.MaxBytes)
	}
	// "0" = 显式禁用/不限
	t.Setenv("EDGEFLOW_EDGECORE_TSDB_RETENTION", "0")
	t.Setenv("EDGEFLOW_EDGECORE_TSDB_MAX_MB", "0")
	opts = tsdbOptionsFromEnv("/data/edgeflow.db")
	if opts.Retention != 0 || opts.MaxBytes != 0 {
		t.Fatalf("\"0\" 语义错误: %v/%d", opts.Retention, opts.MaxBytes)
	}
}

// openTestTSDB 打开临时时序库。
func openTestTSDB(t *testing.T) (*tsdb.DB, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := tsdb.Open(tsdb.Options{Dir: dir})
	if err != nil {
		t.Fatalf("打开时序库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, dir
}

func TestTSSinkWritesAcceptedValues(t *testing.T) {
	gov := rules.NewGovernor()
	eng := rules.NewEvaluator()
	pipe := newSamplePipeline(gov, eng, nil)
	db, _ := openTestTSDB(t)
	pipe.SetTSSink(newTSSink(db))

	out := pipe.process("sensor-01", "default", map[string]float64{"temperature": 42.5}, 1_700_000_000_000)
	if len(out) != 1 || out["temperature"] != 42.5 {
		t.Fatalf("直通返回值不符: %+v", out)
	}
	pts, err := db.Query("sensor-01/default/temperature", nil, 0, 0)
	if err != nil {
		t.Fatalf("Query 失败: %v", err)
	}
	if len(pts) != 1 || pts[0].Value != 42.5 || pts[0].Ts != 1_700_000_000_000 {
		t.Fatalf("sink 写入不符: %+v", pts)
	}
}

func TestTSSinkOnlyWritesAcceptedUnderGovernance(t *testing.T) {
	gov := rules.NewGovernor()
	eng := rules.NewEvaluator()
	pipe := newSamplePipeline(gov, eng, nil)
	// 治理：range [-50,150] + debounce 2
	pol := rules.GovernancePolicy{
		DeviceName: "sensor-01", Property: "temperature",
		Range:    &rules.Bounds{Min: -50, Max: 150},
		Debounce: 2,
	}
	if err := gov.ApplyPolicies([]rules.GovernancePolicy{pol}); err != nil {
		t.Fatalf("应用治理策略失败: %v", err)
	}
	db, _ := openTestTSDB(t)
	pipe.SetTSSink(newTSSink(db))

	// 999 越界（拦截）；10 首次（debounce 拦截）；10 连续第二次（accept）；
	// 999 再拦；20/20（先拦后接受）
	for i, v := range []float64{999, 10, 10, 999, 20, 20} {
		pipe.process("sensor-01", "default", map[string]float64{"temperature": v}, int64(i))
	}
	pts, err := db.Query("sensor-01/default/temperature", nil, 0, 0)
	if err != nil {
		t.Fatalf("Query 失败: %v", err)
	}
	if len(pts) != 2 || pts[0].Value != 10 || pts[1].Value != 20 {
		t.Fatalf("仅 accepted 值应落库: %+v", pts)
	}
}

func TestTSSinkWithoutRulesEngine(t *testing.T) {
	// 无规则/无治理（gov/eng 为 nil）：直通 + sink 记录
	pipe := &samplePipeline{}
	db, _ := openTestTSDB(t)
	pipe.SetTSSink(newTSSink(db))

	out := pipe.process("d1", "ns", map[string]float64{"p": 1.5, "q": 2.5}, 1234)
	if len(out) != 2 {
		t.Fatalf("直通应原样返回: %+v", out)
	}
	for prop, want := range map[string]float64{"p": 1.5, "q": 2.5} {
		pts, err := db.Query("d1/ns/"+prop, nil, 0, 0)
		if err != nil {
			t.Fatalf("Query 失败: %v", err)
		}
		if len(pts) != 1 || pts[0].Value != want || pts[0].Ts != 1234 {
			t.Fatalf("直通 sink 写入不符（%s）: %+v", prop, pts)
		}
	}
}

func TestSetupEdgeTSDBDisabled(t *testing.T) {
	t.Setenv("EDGEFLOW_EDGECORE_TSDB", "off")
	pipe := newSamplePipeline(rules.NewGovernor(), rules.NewEvaluator(), nil)
	cleanup, ok := setupEdgeTSDB(pipe)
	if ok || cleanup != nil {
		t.Fatalf("关闭时应 (nil,false): ok=%v", ok)
	}
}

func TestSetupEdgeTSDBEnabled(t *testing.T) {
	td := t.TempDir()
	dir := filepath.Join(td, "tsdb")
	t.Setenv("EDGEFLOW_EDGECORE_TSDB", "on")
	t.Setenv("EDGEFLOW_EDGECORE_TSDB_DIR", dir)
	t.Setenv("EDGEFLOW_EDGECORE_TSDB_RETENTION", "1h")
	pipe := newSamplePipeline(rules.NewGovernor(), rules.NewEvaluator(), nil)
	cleanup, ok := setupEdgeTSDB(pipe)
	if !ok || cleanup == nil {
		t.Fatal("启用时应成功装配")
	}
	// sink 生效：写一条；cleanup 关闭刷盘后由新实例读回
	pipe.process("d", "ns", map[string]float64{"p": 2}, 1000)
	cleanup() // 唯一一次关闭
	db2, err := tsdb.Open(tsdb.Options{Dir: dir})
	if err != nil {
		t.Fatalf("重开失败: %v", err)
	}
	defer db2.Close()
	pts, err := db2.Query("d/ns/p", nil, 0, 0)
	if err != nil || len(pts) != 1 || pts[0].Value != 2 {
		t.Fatalf("装配后的 sink 数据不符: n=%d err=%v", len(pts), err)
	}
}

func TestSetupEdgeTSDBOpenFailureDegrades(t *testing.T) {
	td := t.TempDir()
	blocker := filepath.Join(td, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 数据目录父路径是普通文件 → MkdirAll 失败
	t.Setenv("EDGEFLOW_EDGECORE_TSDB", "on")
	t.Setenv("EDGEFLOW_EDGECORE_TSDB_DIR", filepath.Join(blocker, "sub"))
	pipe := newSamplePipeline(rules.NewGovernor(), rules.NewEvaluator(), nil)
	cleanup, ok := setupEdgeTSDB(pipe)
	if ok || cleanup != nil {
		t.Fatalf("打开失败应降级为关闭: ok=%v", ok)
	}
	// 降级后管道行为不应受影响（仍直通）
	out := pipe.process("d", "ns", map[string]float64{"p": 1}, 1)
	if len(out) != 1 {
		t.Fatalf("降级后直通行为不符: %+v", out)
	}
}
