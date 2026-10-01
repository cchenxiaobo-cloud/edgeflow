// waveform_test.go：高频波形通道单测（spec 0014 US-3）。
package waveform

import (
	"math"
	"testing"
)

// freqTolerance 是频率特征断言容差（bin 分辨率 = rateHz/N，10kHz/4096 ≈ 2.44Hz；
// 相邻 bin 泄漏下取 1 个 bin 为界）。
const freqTolerance = 3.0

// TestFeaturesKnownSignal 覆盖已知信号特征：50Hz 正弦（幅值 1.0）→
// domFreq ≈ 50Hz、domAmp ≈ 1.0、rms ≈ 1/√2、peak ≈ 1.0、crest ≈ √2。
// 采样率取 10240Hz（bin 宽 2.5Hz，50Hz 恰为 bin20 中心）——矩形窗下非整
// 周期采样会有谱泄漏（主 bin 幅值低于真实值，登记 KI §42），对齐后断言精确。
func TestFeaturesKnownSignal(t *testing.T) {
	const rate = 10240.0
	const n = 4096
	samples := make([]float64, n)
	for i := range samples {
		samples[i] = math.Sin(2 * math.Pi * 50.0 * float64(i) / rate)
	}
	f := Features(samples, rate)
	if df := f["domFreq"]; math.Abs(df-50.0) > freqTolerance {
		t.Fatalf("domFreq = %v, want ≈50（±%v）", df, freqTolerance)
	}
	if da := f["domAmp"]; math.Abs(da-1.0) > 0.05 {
		t.Fatalf("domAmp = %v, want ≈1.0", da)
	}
	wantRMS := 1.0 / math.Sqrt2
	if r := f["rms"]; math.Abs(r-wantRMS) > 0.02 {
		t.Fatalf("rms = %v, want ≈%v", r, wantRMS)
	}
	if p := f["peak"]; math.Abs(p-1.0) > 0.02 {
		t.Fatalf("peak = %v, want ≈1.0", p)
	}
	if c := f["crest"]; math.Abs(c-math.Sqrt2) > 0.05 {
		t.Fatalf("crest = %v, want ≈%v", c, math.Sqrt2)
	}
}

// TestFeaturesHarmonicDominant 覆盖基频+谐波混合：基频幅值高于谐波 →
// 主频仍为基频（50Hz 而非 150Hz）。
func TestFeaturesHarmonicDominant(t *testing.T) {
	const rate = 10000.0
	const n = 4096
	samples := make([]float64, n)
	for i := range samples {
		t := float64(i) / rate
		samples[i] = 1.0*math.Sin(2*math.Pi*50*t) + 0.4*math.Sin(2*math.Pi*150*t)
	}
	f := Features(samples, rate)
	if df := f["domFreq"]; math.Abs(df-50.0) > freqTolerance {
		t.Fatalf("domFreq = %v, want ≈50（基频主导）", df)
	}
}

// TestFeaturesEnvelope 覆盖包络谱主频：AM 调制信号
// x(t) = (1+0.8·sin(2π·10t))·sin(2π·500t) @4kHz → 包络谱主频 ≈ 10Hz。
func TestFeaturesEnvelope(t *testing.T) {
	const rate = 4000.0
	const n = 4096
	samples := make([]float64, n)
	for i := range samples {
		t := float64(i) / rate
		samples[i] = (1 + 0.8*math.Sin(2*math.Pi*10*t)) * math.Sin(2*math.Pi*500*t)
	}
	f := Features(samples, rate)
	if ef := f["envFreq"]; math.Abs(ef-10.0) > freqTolerance {
		t.Fatalf("envFreq = %v, want ≈10（调制频率）", ef)
	}
}

// TestFeaturesEmpty 覆盖空输入（全零特征，不 panic）。
func TestFeaturesEmpty(t *testing.T) {
	f := Features(nil, 10000)
	if len(f) != 6 {
		t.Fatalf("空输入应返回全键特征: %v", f)
	}
}

// TestFFTAgainstDFT 覆盖 radix-2 FFT 与朴素 DFT 一致性（8 点随机序列）。
func TestFFTAgainstDFT(t *testing.T) {
	const n = 8
	re := make([]float64, n)
	for i := range re {
		re[i] = float64(i*i%7) - 3
	}
	im := make([]float64, n)
	refRe := append([]float64(nil), re...)
	refIm := append([]float64(nil), im...)
	fft(re, im)
	for k := 0; k < n; k++ {
		wantRe, wantIm := 0.0, 0.0
		for t2 := 0; t2 < n; t2++ {
			ang := -2 * math.Pi * float64(k*t2) / float64(n)
			wantRe += refRe[t2]*math.Cos(ang) - refIm[t2]*math.Sin(ang)
			wantIm += refRe[t2]*math.Sin(ang) + refIm[t2]*math.Cos(ang)
		}
		if math.Abs(re[k]-wantRe) > 1e-9 || math.Abs(im[k]-wantIm) > 1e-9 {
			t.Fatalf("FFT(%d) = (%v,%v), want (%v,%v)", k, re[k], im[k], wantRe, wantIm)
		}
	}
}

// TestRingOverwrite 覆盖环形缓冲覆盖语义与时间序快照。
func TestRingOverwrite(t *testing.T) {
	r := NewRing(4)
	r.Push([]float64{1, 2})
	if r.Len() != 2 {
		t.Fatalf("Len = %d, want 2", r.Len())
	}
	r.Push([]float64{3, 4, 5, 6, 7})
	if r.Len() != 4 {
		t.Fatalf("满容后 Len = %d, want 4", r.Len())
	}
	snap := r.Snapshot()
	want := []float64{4, 5, 6, 7} // 最旧被覆盖，保留最新 4 个（时间序）
	for i := range want {
		if snap[i] != want[i] {
			t.Fatalf("Snapshot[%d] = %v, want %v", i, snap[i], want[i])
		}
	}
}

// TestRingEmpty 覆盖空缓冲。
func TestRingEmpty(t *testing.T) {
	r := NewRing(8)
	if r.Len() != 0 || len(r.Snapshot()) != 0 {
		t.Fatalf("空缓冲应 Len=0/Snapshot 空")
	}
}

// TestSourceDeterministicAndContinuous 覆盖模拟源确定性（同 seed 同输出）与
// 仿真时间轴连续推进（块间无重叠/无缝隙）。
func TestSourceDeterministicAndContinuous(t *testing.T) {
	s1 := NewVibrationSource(10000, 512, 42)
	s2 := NewVibrationSource(10000, 512, 42)
	b1 := s1.NextBlock()
	b2 := s2.NextBlock()
	if len(b1.Samples) != 512 || b1.RateHz != 10000 {
		t.Fatalf("块规格不符: %s", b1)
	}
	for i := range b1.Samples {
		if b1.Samples[i] != b2.Samples[i] {
			t.Fatalf("同 seed 应确定性: [%d] %v vs %v", i, b1.Samples[i], b2.Samples[i])
		}
	}
	// 时间轴推进：下一块起始 = 上一块起始 + 块长/rate（512/10000 = 51.2ms → 51）。
	nb := s1.NextBlock()
	wantTs := b1.Ts + int64(math.Round(512.0/10000.0*1000))
	if nb.Ts != wantTs {
		t.Fatalf("时间轴推进不符: %d vs %d", nb.Ts, wantTs)
	}
	// 不同 seed 输出不同。
	s3 := NewVibrationSource(10000, 512, 43)
	b3 := s3.NextBlock()
	diff := false
	for i := range b1.Samples {
		if b1.Samples[i] != b3.Samples[i] {
			diff = true
			break
		}
	}
	if !diff {
		t.Fatalf("不同 seed 应产出不同波形")
	}
	// 幅值有界（基波+谐波+噪声 ≈ |1.0|+|0.4|+3σ）。
	for _, v := range b1.Samples {
		if math.Abs(v) > 2.0 {
			t.Fatalf("幅值越界: %v", v)
		}
	}
}
