// Package waveform 提供高频波形通道抽象（v0.41.0，spec 0014 US-3）：
// 块采集（模拟振动源按 rateHz 连续出块）、环形缓冲、特征前置
// （RMS/Peak/Crest + FFT 主频 + 包络谱主频——最小实现，零依赖）。
//
// 定位（发展规划 v0.41 · G19）：维护场景高频通道（振动 10kHz）——波形块
// 经特征提取把「特征值」喂入既有采样管道（治理→规则→告警→tsdb，复用
// v0.37–v0.39 设施），原始波形可选落 v0.38 时序库（_RAW_TSDB=on，量级登记）。
//
// 边界（登记 KI §42）：FFT 为 radix-2（2 的幂，非 2 幂补零）；矩形窗
// （无加窗，谱泄漏登记）；包络谱为最小实现（|x| 去均值 → FFT，非完整
// Hilbert 解调）；模拟源按仿真速率出块（墙钟块间隔与仿真块时长解耦）。
package waveform

import (
	"fmt"
	"math"
	"math/rand"
	"sync"
)

// Block 是一个波形块（块采集语义：rateHz 采样率下的连续样本段）。
type Block struct {
	Samples []float64 // 采样值（物理单位）
	Ts      int64     // 块起始时间（毫秒，仿真时间轴——随块长推进）
	RateHz  float64   // 采样率（Hz）
}

// Source 是模拟振动源：基频+谐波+高斯噪声，确定性（seed）。
// 仿真速率连续出块——NextBlock 每次返回 [ts, ts+block/rate) 的信号段，
// 仿真时间轴随块长推进（墙钟间隔由装配层周期控制，二者解耦）。
type Source struct {
	rateHz  float64    // 采样率（Hz）
	block   int        // 块长（样本数）
	f1, a1  float64    // 基频/幅值（默认 50Hz / 1.0）
	f2, a2  float64    // 谐波/幅值（默认 150Hz / 0.4）
	noise   float64    // 高斯噪声 σ（默认 0.05）
	phase   float64    // 仿真时间游标（秒）
	phaseMs float64    // 仿真时间戳游标（毫秒；float 累加 + Round 输出，消除逐块截断漂移）
	rng     *rand.Rand // 噪声源（确定性）
}

// NewVibrationSource 创建模拟振动源（rateHz 采样率、block 块长、seed 确定性；
// 默认注入 50Hz 基波（幅值 1.0）+ 150Hz 谐波（幅值 0.4）+ σ=0.05 噪声）。
func NewVibrationSource(rateHz float64, block int, seed int64) *Source {
	return &Source{
		rateHz: rateHz,
		block:  block,
		f1:     50.0,
		a1:     1.0,
		f2:     150.0,
		a2:     0.4,
		noise:  0.05,
		rng:    rand.New(rand.NewSource(seed)),
	}
}

// NextBlock 产出下一个波形块（仿真时间轴连续推进）。
func (s *Source) NextBlock() Block {
	samples := make([]float64, s.block)
	dt := 1.0 / s.rateHz
	for i := range samples {
		t := s.phase + float64(i)*dt
		v := s.a1*math.Sin(2*math.Pi*s.f1*t) + s.a2*math.Sin(2*math.Pi*s.f2*t) + s.rng.NormFloat64()*s.noise
		samples[i] = v
	}
	blk := Block{
		Samples: samples,
		Ts:      int64(math.Round(s.phaseMs)),
		RateHz:  s.rateHz,
	}
	s.phase += float64(s.block) * dt
	s.phaseMs += float64(s.block) / s.rateHz * 1000
	return blk
}

// Ring 是定容环形缓冲（覆盖最旧；并发安全；count 语义——满容判定不依赖
// head 回绕，任意批长 Push 后 Snapshot 均返回按时间序的最新 min(count,cap) 样本）。
type Ring struct {
	mu    sync.Mutex
	buf   []float64
	cap   int
	head  int // 下一写入位置
	count int // 已缓存样本数（≤ cap）
}

// NewRing 创建定容环形缓冲。
func NewRing(capacity int) *Ring {
	if capacity < 1 {
		capacity = 1
	}
	return &Ring{buf: make([]float64, capacity), cap: capacity}
}

// Push 写入一批样本（超出容量覆盖最旧）。
func (r *Ring) Push(samples []float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range samples {
		r.buf[r.head] = v
		r.head = (r.head + 1) % r.cap
		if r.count < r.cap {
			r.count++
		}
	}
}

// Snapshot 返回按时间序（旧→新）的快照副本。
func (r *Ring) Snapshot() []float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.count < r.cap {
		out := make([]float64, r.count)
		copy(out, r.buf[:r.head])
		return out
	}
	out := make([]float64, 0, r.cap)
	out = append(out, r.buf[r.head:]...)
	out = append(out, r.buf[:r.head]...)
	return out
}

// Len 返回当前缓存样本数。
func (r *Ring) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

// nextPow2 返回 ≥ n 的最小 2 的幂。
func nextPow2(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

// fft 原地 radix-2 迭代 FFT（n 为 2 的幂；调用方保证）。
func fft(re, im []float64) {
	n := len(re)
	// 位反转重排
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j |= bit
		if i < j {
			re[i], re[j] = re[j], re[i]
			im[i], im[j] = im[j], im[i]
		}
	}
	// 蝶形运算
	for length := 2; length <= n; length <<= 1 {
		ang := -2 * math.Pi / float64(length)
		c, s := math.Cos(ang), math.Sin(ang)
		for i := 0; i < n; i += length {
			wr, wi := 1.0, 0.0
			half := length / 2
			for j := 0; j < half; j++ {
				ur, ui := re[i+j], im[i+j]
				vr := re[i+j+half]*wr - im[i+j+half]*wi
				vi := re[i+j+half]*wi + im[i+j+half]*wr
				re[i+j] = ur + vr
				im[i+j] = ui + vi
				re[i+j+half] = ur - vr
				im[i+j+half] = ui - vi
				wr, wi = wr*c-wi*s, wr*s+wi*c
			}
		}
	}
}

// magnitudeSpectrum 计算单边幅值谱（去 DC；矩形窗）。
// 返回长度 n/2 的幅值数组（bin k 对应频率 k*rateHz/n）。
func magnitudeSpectrum(samples []float64, rateHz float64) (mags []float64) {
	n := nextPow2(len(samples))
	re := make([]float64, n)
	copy(re, samples)
	im := make([]float64, n)
	fft(re, im)
	half := n / 2
	mags = make([]float64, half)
	for k := 1; k < half; k++ { // k=0 为 DC，跳过
		mags[k] = math.Sqrt(re[k]*re[k]+im[k]*im[k]) * 2 / float64(n)
	}
	return mags
}

// dominantOf 返回幅值谱中最大 bin 对应的频率与幅值（minHz/maxHz 为搜索频段）。
func dominantOf(mags []float64, rateHz float64, minHz, maxHz float64) (freq, amp float64) {
	n := len(mags) * 2 // 与 magnitudeSpectrum 的 n 一致（单边数组长度 = n/2）
	best := -1
	bestAmp := -1.0
	for k := 1; k < len(mags); k++ {
		f := float64(k) * rateHz / float64(n)
		if f < minHz || f > maxHz {
			continue
		}
		if mags[k] > bestAmp {
			bestAmp = mags[k]
			best = k
		}
	}
	if best < 0 {
		return 0, 0
	}
	return float64(best) * rateHz / float64(n), bestAmp
}

// Features 从波形块提取特征（特征前置——特征值经采样管道进影子/规则/告警）：
//   - rms：均方根；peak：最大绝对值；crest：peak/rms（波峰因子）；
//   - domFreq/domAmp：FFT 幅值谱主频（排除 DC 与 <1Hz 分量）与幅值；
//   - envFreq：包络谱主频（|x| 去均值 → FFT，搜索 [1Hz, rateHz/8]——
//     包络谱最小实现，非完整 Hilbert 解调）。
//
// 特征频率分辨率 = rateHz/nextPow2(N)（10kHz/4096 ≈ 2.44Hz，登记 KI §42）。
func Features(samples []float64, rateHz float64) map[string]float64 {
	if len(samples) == 0 {
		return map[string]float64{"rms": 0, "peak": 0, "crest": 0, "domFreq": 0, "domAmp": 0, "envFreq": 0}
	}
	var sumSq, peak float64
	env := make([]float64, len(samples))
	var envSum float64
	for i, v := range samples {
		sumSq += v * v
		av := math.Abs(v)
		if av > peak {
			peak = av
		}
		env[i] = av
		envSum += av
	}
	rms := math.Sqrt(sumSq / float64(len(samples)))
	crest := 0.0
	if rms > 0 {
		crest = peak / rms
	}
	// 主频（排除 <1Hz 与 DC）。
	mags := magnitudeSpectrum(samples, rateHz)
	domFreq, domAmp := dominantOf(mags, rateHz, 1.0, rateHz/2)
	// 包络谱主频：|x| 去均值 → FFT，搜索 [1Hz, rateHz/8]（低频调制段）。
	envMean := envSum / float64(len(env))
	for i := range env {
		env[i] -= envMean
	}
	envMags := magnitudeSpectrum(env, rateHz)
	envFreq, _ := dominantOf(envMags, rateHz, 1.0, rateHz/8)
	return map[string]float64{
		"rms":     round3(rms),
		"peak":    round3(peak),
		"crest":   round3(crest),
		"domFreq": round3(domFreq),
		"domAmp":  round3(domAmp),
		"envFreq": round3(envFreq),
	}
}

// FeatureNames 返回特征键集合（装配层/文档口径一致用）。
func FeatureNames() []string {
	return []string{"rms", "peak", "crest", "domFreq", "domAmp", "envFreq"}
}

// round3 保留 3 位小数（特征值上报粒度；避免浮点尾巴进影子/告警）。
func round3(v float64) float64 {
	return math.Round(v*1000) / 1000
}

// String 辅助（诊断日志用）。
func (b Block) String() string {
	return fmt.Sprintf("Block{ts=%d, n=%d, rate=%.0fHz}", b.Ts, len(b.Samples), b.RateHz)
}
