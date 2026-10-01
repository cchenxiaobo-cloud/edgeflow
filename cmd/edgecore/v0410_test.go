// v0410_test.go：波形装配单测（spec 0014 US-3）。
package main

import (
	"testing"
	"time"

	"edgeflow/edge/pkg/devicetwin"
	"edgeflow/pkg/rules"
)

// TestParseWaveformOptionsFromEnv 覆盖默认值/覆盖/非法回退。
func TestParseWaveformOptionsFromEnv(t *testing.T) {
	// 默认（未开启）。
	o := parseWaveformOptionsFromEnv()
	if o.enabled {
		t.Fatalf("默认应关闭")
	}
	if o.rateHz != defaultWaveformRateHz || o.block != defaultWaveformBlock || o.intervalMs != defaultWaveformInterval {
		t.Fatalf("默认参数不符: %+v", o)
	}
	if o.device != defaultWaveformDevice || o.ns != defaultWaveformNS {
		t.Fatalf("默认设备/命名空间不符: %+v", o)
	}
	// 合法覆盖。
	t.Setenv(envWaveformEnabled, "on")
	t.Setenv(envWaveformRateHz, "8000")
	t.Setenv(envWaveformBlock, "1024")
	t.Setenv(envWaveformInterval, "200")
	t.Setenv(envWaveformDevice, "vib-x")
	t.Setenv(envWaveformNS, "plant")
	o = parseWaveformOptionsFromEnv()
	if !o.enabled || o.rateHz != 8000 || o.block != 1024 || o.intervalMs != 200 || o.device != "vib-x" || o.ns != "plant" {
		t.Fatalf("覆盖参数不符: %+v", o)
	}
	// 非法回退。
	t.Setenv(envWaveformRateHz, "abc")
	t.Setenv(envWaveformBlock, "0")
	t.Setenv(envWaveformInterval, "-1")
	o = parseWaveformOptionsFromEnv()
	if o.rateHz != defaultWaveformRateHz || o.block != defaultWaveformBlock || o.intervalMs != defaultWaveformInterval {
		t.Fatalf("非法值应回退默认: %+v", o)
	}
}

// TestWaveformLoopDisabled 覆盖关闭态（默认零行为——不启动循环）。
func TestWaveformLoopDisabled(t *testing.T) {
	if stop := setupWaveformLoop(nil, nil); stop != nil {
		t.Fatalf("未开启时应返回 nil")
	}
}

// TestWaveformLoopWritesShadow 覆盖启用态：特征经管道写入影子（可关停）。
func TestWaveformLoopWritesShadow(t *testing.T) {
	tw := devicetwin.NewStore()
	pipe := newSamplePipeline(rules.NewGovernor(), rules.NewEvaluator(), nil)

	t.Setenv(envWaveformEnabled, "on")
	t.Setenv(envWaveformInterval, "100")
	t.Setenv(envWaveformRateHz, "10000")
	t.Setenv(envWaveformBlock, "1024")

	stop := setupWaveformLoop(pipe, tw)
	if stop == nil {
		t.Fatalf("开启后应返回停止函数")
	}
	deadline := time.Now().Add(3 * time.Second)
	var props map[string]float64
	for time.Now().Before(deadline) {
		for _, twin := range tw.SnapshotAll() {
			if twin.DeviceName == defaultWaveformDevice {
				props = twin.Reported
				break
			}
		}
		if props != nil && props["rms"] > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	stop()
	stop() // 幂等
	if props == nil || props["rms"] <= 0 {
		t.Fatalf("影子应含波形特征（rms > 0）: %v", props)
	}
	for _, k := range []string{"peak", "domFreq", "envFreq"} {
		if _, ok := props[k]; !ok {
			t.Fatalf("特征缺键 %s: %v", k, props)
		}
	}
}
