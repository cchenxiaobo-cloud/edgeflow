// 波形采集循环（v0.41.0，spec 0014 US-3）：模拟振动源按仿真速率连续出块 →
// 特征前置（RMS/Peak/Crest/FFT 主频/包络谱主频）→ 经既有采样管道
// （治理→规则→tsSink）进影子 → 随 DeviceReport 上云；原始波形落 tsdb
// 为子开关（_RAW_TSDB=on，默认 off——10k 点/s 量级，登记 KI §42）。
//
// opt-in：EDGEFLOW_EDGECORE_WAVEFORM=on（默认 off 零行为——循环不启动，
// 影子/规则/告警/tsdb 全链零变化）。
package main

import (
	"math"
	"os"
	"strconv"
	"sync"
	"time"

	"edgeflow/edge/pkg/devicetwin"
	"edgeflow/pkg/log"
	"edgeflow/pkg/waveform"
)

// 波形环境变量（spec 0014 US-3）。
const (
	envWaveformEnabled  = "EDGEFLOW_EDGECORE_WAVEFORM"
	envWaveformRateHz   = "EDGEFLOW_EDGECORE_WAVEFORM_RATE_HZ"
	envWaveformBlock    = "EDGEFLOW_EDGECORE_WAVEFORM_BLOCK"
	envWaveformInterval = "EDGEFLOW_EDGECORE_WAVEFORM_INTERVAL_MS"
	envWaveformDevice   = "EDGEFLOW_EDGECORE_WAVEFORM_DEVICE"
	envWaveformNS       = "EDGEFLOW_EDGECORE_WAVEFORM_NS"
	envWaveformRawTSDB  = "EDGEFLOW_EDGECORE_WAVEFORM_RAW_TSDB"

	defaultWaveformRateHz   = 10000.0
	defaultWaveformBlock    = 4096
	defaultWaveformInterval = 500
	defaultWaveformDevice   = "vibration-01"
	defaultWaveformNS       = "waveform"

	// waveformLogEveryBlocks 是特征日志限频（每 N 块一条，避免刷屏）。
	waveformLogEveryBlocks = 20
)

// waveformOptions 是波形装配参数（解析自环境变量，非法回退默认）。
type waveformOptions struct {
	enabled    bool
	rateHz     float64
	block      int
	intervalMs int
	device     string
	ns         string
	rawTSDB    bool
}

// parseWaveformOptionsFromEnv 解析波形装配参数。
func parseWaveformOptionsFromEnv() waveformOptions {
	o := waveformOptions{
		enabled:    os.Getenv(envWaveformEnabled) == "on",
		rateHz:     defaultWaveformRateHz,
		block:      defaultWaveformBlock,
		intervalMs: defaultWaveformInterval,
		device:     defaultWaveformDevice,
		ns:         defaultWaveformNS,
		rawTSDB:    os.Getenv(envWaveformRawTSDB) == "on",
	}
	if v := os.Getenv(envWaveformRateHz); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 1 {
			o.rateHz = f
		} else {
			log.Warnf("%s 非法（%q），回退默认 %.0f", envWaveformRateHz, v, defaultWaveformRateHz)
		}
	}
	if v := os.Getenv(envWaveformBlock); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 16 {
			o.block = n
		} else {
			log.Warnf("%s 非法（%q），回退默认 %d", envWaveformBlock, v, defaultWaveformBlock)
		}
	}
	if v := os.Getenv(envWaveformInterval); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 10 {
			o.intervalMs = n
		} else {
			log.Warnf("%s 非法（%q），回退默认 %d", envWaveformInterval, v, defaultWaveformInterval)
		}
	}
	if v := os.Getenv(envWaveformDevice); v != "" {
		o.device = v
	}
	if v := os.Getenv(envWaveformNS); v != "" {
		o.ns = v
	}
	return o
}

// setupWaveformLoop 装配波形特征循环（opt-in；返回停止函数，nil = 未启用）。
// 特征经 pipe.process（治理→规则→tsSink）后写影子——随 DeviceReport 上云；
// 原始波形（_RAW_TSDB=on）直写 tsSink（绕过治理/规则——原始数据不做规则评估，
// 特征值才进评估链）。循环退出由返回的停止函数驱动（优雅关闭）。
func setupWaveformLoop(pipe *samplePipeline, twins *devicetwin.TwinStore) func() {
	opts := parseWaveformOptionsFromEnv()
	if !opts.enabled {
		return nil
	}
	src := waveform.NewVibrationSource(opts.rateHz, opts.block, time.Now().UnixNano())
	var rawSink func(device, ns, prop string, value float64, ts int64)
	if opts.rawTSDB && pipe != nil {
		rawSink = pipe.TSSink()
	}
	done := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(done) }) }

	go func() {
		ticker := time.NewTicker(time.Duration(opts.intervalMs) * time.Millisecond)
		defer ticker.Stop()
		blocks := 0
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				blk := src.NextBlock()
				feats := waveform.Features(blk.Samples, blk.RateHz)
				now := time.Now().UnixMilli()
				out := feats
				if pipe != nil {
					out = pipe.process(opts.device, opts.ns, feats, now)
				}
				if twins != nil {
					twins.UpsertReported(opts.device, opts.ns, out, now)
				}
				if rawSink != nil {
					for i, v := range blk.Samples {
						// 仿真时间轴（1970 起）——按墙钟的保留/窗口查询不可见，
						// 登记 KI §42；Round 消除逐样本截断漂移。
						ts := blk.Ts + int64(math.Round(float64(i)/blk.RateHz*1000))
						rawSink(opts.device, opts.ns, "waveform", v, ts)
					}
				}
				blocks++
				if blocks%waveformLogEveryBlocks == 0 {
					log.Infof("波形采集：已出 %d 块（块长 %d @%.0fHz，特征 rms=%v domFreq=%v）",
						blocks, opts.block, opts.rateHz, feats["rms"], feats["domFreq"])
				}
			}
		}
	}()
	log.Infof("波形采集循环已启用（rate=%.0fHz，block=%d，interval=%dms，设备 %s/%s，原始落库=%v）",
		opts.rateHz, opts.block, opts.intervalMs, opts.device, opts.ns, opts.rawTSDB)
	return stop
}
