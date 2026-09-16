// 时序存储装配（v0.38.0，spec 0011 US-6）：环境开关解析 + 采样管道 sink。
//
// 全部 opt-in：EDGEFLOW_EDGECORE_TSDB=on 时才加载；默认关闭时启动链路
// 零新日志、数据路径零变化（与 v0.37.0 逐字节等价）。
package main

import (
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"edgeflow/edge/pkg/metamanager"
	"edgeflow/pkg/log"
	"edgeflow/pkg/tsdb"
)

// tsdbEnabled 报告是否启用边缘时序库（opt-in：EDGEFLOW_EDGECORE_TSDB=on）。
func tsdbEnabled() bool {
	return os.Getenv("EDGEFLOW_EDGECORE_TSDB") == "on"
}

// tsdbDirFromEnv 返回时序库数据目录：EDGEFLOW_EDGECORE_TSDB_DIR 覆盖，
// 默认 <edgecore 数据库文件同目录>/tsdb（如 data/tsdb）。
func tsdbDirFromEnv(dbPath string) string {
	if v := os.Getenv("EDGEFLOW_EDGECORE_TSDB_DIR"); v != "" {
		return v
	}
	return filepath.Join(filepath.Dir(dbPath), "tsdb")
}

// tsdbOptionsFromEnv 构造时序库配置（spec 0011 US-6 默认值）：
//   - EDGEFLOW_EDGECORE_TSDB_RETENTION：保留时长（默认 72h；"0" 禁用）；
//   - EDGEFLOW_EDGECORE_TSDB_MAX_MB：容量水位 MB（默认 256；"0" 不限）；
//   - EDGEFLOW_EDGECORE_TSDB_DOWNSAMPLE=off：关闭降采样（默认 on：after=1h/every=1m）。
//
// 非法值告警后回退默认（不阻断启动）。
func tsdbOptionsFromEnv(dbPath string) tsdb.Options {
	opts := tsdb.Options{
		Dir:             tsdbDirFromEnv(dbPath),
		Retention:       72 * time.Hour,
		DownsampleAfter: time.Hour,
		DownsampleEvery: time.Minute,
		MaxBytes:        256 << 20,
	}
	if v := os.Getenv("EDGEFLOW_EDGECORE_TSDB_RETENTION"); v != "" {
		if v == "0" {
			opts.Retention = 0
		} else if d, err := time.ParseDuration(v); err == nil && d > 0 {
			opts.Retention = d
		} else {
			log.Warnf("TSDB_RETENTION 非法（%q），回退默认 %v", v, opts.Retention)
		}
	}
	if v := os.Getenv("EDGEFLOW_EDGECORE_TSDB_MAX_MB"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			opts.MaxBytes = n << 20
		} else {
			log.Warnf("TSDB_MAX_MB 非法（%q），回退默认 %d MB", v, opts.MaxBytes>>20)
		}
	}
	if os.Getenv("EDGEFLOW_EDGECORE_TSDB_DOWNSAMPLE") == "off" {
		opts.DownsampleAfter = 0
	}
	return opts
}

// tsSinkWriter 把采样值写入时序库；对连续失败告警做限频（背压时丢弃
// 计数在库内统计，这里避免日志刷屏；恢复时提示一次）。
type tsSinkWriter struct {
	db   *tsdb.DB
	fail int64 // 连续失败计数（atomic）
}

// newTSSink 构造 samplePipeline 的时序写入出口：
// metric = device/namespace/property（Time 戳沿用采样时刻）。
func newTSSink(db *tsdb.DB) func(device, ns, prop string, value float64, ts int64) {
	w := &tsSinkWriter{db: db}
	return func(device, ns, prop string, value float64, ts int64) {
		if err := w.db.WriteOne(device+"/"+ns+"/"+prop, ts, value); err != nil {
			n := atomic.AddInt64(&w.fail, 1)
			if n == 1 || n%256 == 0 {
				log.Warnf("时序写入失败（连续第 %d 次，如容量背压）: %v", n, err)
			}
			return
		}
		if n := atomic.SwapInt64(&w.fail, 0); n > 0 {
			log.Infof("时序写入已恢复（此前连续失败 %d 次）", n)
		}
	}
}

// setupEdgeTSDB 装配时序库（spec 0011 US-6）：
// 开关关闭 → (nil, false) 静默返回（零行为）；打开失败 → 告警 + (nil, false)
// （不阻断启动，采集不受影响）。成功时：设置采样管道 sink → 启动保留轮 →
// 周期保留循环；返回清理函数（随 edgecore 关闭刷盘）与启用标志。
func setupEdgeTSDB(pipe *samplePipeline) (func(), bool) {
	if !tsdbEnabled() {
		return nil, false
	}
	opts := tsdbOptionsFromEnv(metamanager.DefaultDBPathFromEnv())
	tdb, err := tsdb.Open(opts)
	if err != nil {
		log.Warnf("时序库启用失败（继续运行，不落时序数据）: %v", err)
		return nil, false
	}
	if res, err := tdb.RunRetention(time.Now().UnixMilli()); err != nil {
		log.Warnf("时序库启动保留轮失败: %v", err)
	} else if res.Deleted > 0 || res.Downsampled > 0 {
		log.Infof("时序库启动保留轮：降采样 %d 段，删除 %d 段（释放 %d 字节）",
			res.Downsampled, res.Deleted, res.BytesFreed)
	}
	stopCh := make(chan struct{})
	go tdb.RunRetentionLoop(time.Minute, stopCh)
	pipe.SetTSSink(newTSSink(tdb))
	st := tdb.Stats()
	log.Infof("时序库已启用（dir=%s；序列 %d，点 %d；保留 %v，降采样 after=%v/every=%v，水位 %dMB）",
		opts.Dir, st.Series, st.Points, opts.Retention, opts.DownsampleAfter, opts.DownsampleEvery, opts.MaxBytes>>20)
	return func() {
		close(stopCh)
		if err := tdb.Close(); err != nil {
			log.Warnf("时序库关闭失败: %v", err)
		} else {
			log.Infof("时序库已关闭（刷盘完成）")
		}
	}, true
}
