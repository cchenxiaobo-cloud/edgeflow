package video

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"edgeflow/edge/pkg/mapper"
	"edgeflow/edge/pkg/metamanager"
	pkgvideo "edgeflow/pkg/video"
)

// stubSource 顺序产出 n 帧（seq 1..n），随后阻塞（模拟持续流）。
type stubSource struct {
	n    int
	ch   chan struct{}
	once sync.Once
	fast bool // true=不出帧间隔直接连发（触发背压）
}

func newStubSource(n int, fast bool) *stubSource {
	return &stubSource{n: n, ch: make(chan struct{}), fast: fast}
}

func (s *stubSource) Next(ctx context.Context) (*pkgvideo.Frame, error) {
	s.once.Do(func() { close(s.ch) })
	if s.n <= 0 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	s.n--
	if !s.fast {
		time.Sleep(20 * time.Millisecond)
	}
	return &pkgvideo.Frame{Seq: uint64(1000 - s.n), TsMs: time.Now().UnixMilli(), Width: 32, Height: 24,
		JPEG: []byte{0xFF, 0xD8, 0xFF, 0xD9}}, nil
}

// stubInfer 可编程推理器：延迟 + 错误注入 + 固定结果。
type stubInfer struct {
	delay  time.Duration
	fail   error
	result *pkgvideo.InferenceResult

	mu    sync.Mutex
	calls []uint64
}

func (s *stubInfer) Infer(_ context.Context, f *pkgvideo.Frame) (*pkgvideo.InferenceResult, error) {
	s.mu.Lock()
	s.calls = append(s.calls, f.Seq)
	s.mu.Unlock()
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	if s.fail != nil {
		return nil, s.fail
	}
	return s.result, nil
}

func (s *stubInfer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// stubPublisher 收集发布的事件。
type stubPublisher struct {
	mu     sync.Mutex
	topics []string
	bodies [][]byte
}

func (p *stubPublisher) Publish(topic string, payload []byte) error {
	p.mu.Lock()
	p.topics = append(p.topics, topic)
	p.bodies = append(p.bodies, payload)
	p.mu.Unlock()
	return nil
}

func (p *stubPublisher) snapshot() ([]string, [][]byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.topics, p.bodies
}

func testConfig(url string) *Config {
	return &Config{
		DeviceName: "cam-01",
		Namespace:  "default",
		Source:     SourceConfig{Type: "synthetic"},
		Inference:  InferConfig{URL: url, TimeoutMs: 1000},
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", msg)
}

// writeFile 写文件辅助（配置用例）。
func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}

// TestV0310MapperMetricsAndEvents 指标面累计 + 事件上行 + 台账留痕（US-3/US-4）。
func TestV0310MapperMetricsAndEvents(t *testing.T) {
	inf := &stubInfer{result: &pkgvideo.InferenceResult{
		Detections: []pkgvideo.Detection{
			{Label: "person", Score: 0.8, BBox: [4]float64{1, 1, 2, 2}},
			{Label: "car", Score: 0.6, BBox: [4]float64{3, 3, 4, 4}},
		},
	}}
	pub := &stubPublisher{}
	store, err := metamanager.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ledger, err := metamanager.NewLedger(store)
	if err != nil {
		t.Fatalf("NewLedger: %v", err)
	}
	m, err := NewMapper(testConfig("http://unused"),
		WithSource(func(*Config) (pkgvideo.FrameSource, error) { return newStubSource(3, false), nil }),
		WithInferencer(func(*Config) pkgvideo.Inferencer { return inf }),
		WithLedger(ledger), WithEventPublisher(pub))
	if err != nil {
		t.Fatalf("NewMapper: %v", err)
	}
	if m.Name() != DefaultName || m.DeviceNames()[0] != "cam-01" || m.DeviceNamespace() != "default" {
		t.Fatalf("Mapper 元信息错误")
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Start 幂等。
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start 幂等: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool { return inf.callCount() >= 3 }, "3 帧推理未完成")
	// 留痕/事件到达（异步于推理返回）。
	waitFor(t, 2*time.Second, func() bool {
		ops, _ := ledger.ListOps(metamanager.OpFilter{DeviceID: "cam-01", Direction: metamanager.DirUp})
		return len(ops) >= 3
	}, "台账留痕不足 3 条")
	topics, bodies := pub.snapshot()
	if len(topics) < 3 {
		t.Fatalf("事件不足 3 条: %d", len(topics))
	}
	wantTopic := fmt.Sprintf(InferenceTopicTpl, "cam-01")
	for i, tp := range topics {
		if tp != wantTopic {
			t.Fatalf("主题错误: %s", tp)
		}
		var res pkgvideo.InferenceResult
		if err := json.Unmarshal(bodies[i], &res); err != nil {
			t.Fatalf("事件 payload 非法 JSON: %v", err)
		}
		if len(res.Detections) != 2 || res.Detections[0].Label != "person" {
			t.Fatalf("事件结果不保真: %+v", res)
		}
	}
	props, err := m.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if props["framesTotal"] < 3 || props["inferTotal"] < 3 || props["inferFailTotal"] != 0 {
		t.Fatalf("指标面异常: %v", props)
	}
	if props["detectionsLast"] != 2 || props["avgScoreLast"] != 0.7 || props["frameSeqLast"] < 3 {
		t.Fatalf("结果指标异常: %v", props)
	}
	if props["streamOn"] != 1 {
		t.Fatalf("streamOn 应为 1: %v", props)
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := m.Stop(); err != nil { // 幂等
		t.Fatalf("Stop 幂等: %v", err)
	}
}

// TestV0310MapperDropAndFail 背压丢帧 + 推理失败计数（US-3）。
func TestV0310MapperDropAndFail(t *testing.T) {
	inf := &stubInfer{delay: 80 * time.Millisecond, fail: errors.New("infer down")}
	m, err := NewMapper(testConfig("http://unused"),
		WithSource(func(*Config) (pkgvideo.FrameSource, error) { return newStubSource(20, true), nil }),
		WithInferencer(func(*Config) pkgvideo.Inferencer { return inf }))
	if err != nil {
		t.Fatalf("NewMapper: %v", err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool {
		props, _ := m.Collect()
		return props["framesDropped"] > 0 && props["inferFailTotal"] > 0
	}, "未观察到丢帧与失败计数")
	props, _ := m.Collect()
	if props["inferTotal"] != 0 {
		t.Fatalf("全失败时 inferTotal 应为 0: %v", props)
	}
	_ = m.Stop()
}

// TestV0310MapperStreamCommand stream 指令启停（US-3）。
func TestV0310MapperStreamCommand(t *testing.T) {
	inf := &stubInfer{result: &pkgvideo.InferenceResult{}}
	m, err := NewMapper(testConfig("http://unused"),
		WithSource(func(*Config) (pkgvideo.FrameSource, error) { return newStubSource(0, false), nil }),
		WithInferencer(func(*Config) pkgvideo.Inferencer { return inf }))
	if err != nil {
		t.Fatalf("NewMapper: %v", err)
	}
	// 暂停（未启动时 Stop 幂等）。
	rep, err := m.HandleCommand(mapper.DeviceCommand{DeviceName: "cam-01", Property: "stream", Value: 0})
	if err != nil {
		t.Fatalf("stream=0: %v", err)
	}
	if rep.Properties["streamOn"] != 0 {
		t.Fatalf("暂停后 streamOn 应为 0: %v", rep.Properties)
	}
	// 启动。
	rep, err = m.HandleCommand(mapper.DeviceCommand{DeviceName: "cam-01", Property: "stream", Value: 1})
	if err != nil {
		t.Fatalf("stream=1: %v", err)
	}
	if rep.Properties["streamOn"] != 1 {
		t.Fatalf("启动后 streamOn 应为 1: %v", rep.Properties)
	}
	// 未知属性拒绝。
	if _, err := m.HandleCommand(mapper.DeviceCommand{Property: "zoom", Value: 2}); err == nil {
		t.Fatalf("未知属性未拒绝")
	}
	_ = m.Stop()
}

// TestV0310MapperConfigValidate 配置校验（US-1 边界 + US-5 装配前置）。
func TestV0310MapperConfigValidate(t *testing.T) {
	if err := (&Config{DeviceName: "c", Source: SourceConfig{Type: "rtsp"}, Inference: InferConfig{URL: "x"}}).validate(); err == nil {
		t.Fatalf("rtsp 类型阶段一应拒绝")
	}
	if err := (&Config{Source: SourceConfig{Type: "synthetic"}, Inference: InferConfig{URL: "x"}}).validate(); err == nil {
		t.Fatalf("空设备名应拒绝")
	}
	if err := (&Config{DeviceName: "c", Source: SourceConfig{Type: "synthetic"}}).validate(); err == nil {
		t.Fatalf("空推理 URL 应拒绝")
	}
	// LoadConfig：文件不存在报错。
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatalf("缺失配置文件应报错")
	}
	// LoadConfig：合法文件。
	dir := t.TempDir()
	path := filepath.Join(dir, "video.json")
	good := []byte(`{"deviceName":"cam-9","source":{"type":"synthetic","synthetic":{"fps":2}},"inference":{"url":"http://x/infer"}}`)
	if werr := writeFile(path, good); werr != nil {
		t.Fatalf("写配置: %v", werr)
	}
	cfg, err := LoadConfig(path)
	if err != nil || cfg.DeviceName != "cam-9" || cfg.Source.Synth.FPS != 2 {
		t.Fatalf("LoadConfig: cfg=%+v err=%v", cfg, err)
	}
}

// TestV0310MapperE2E 装配级全链路：配置文件 → LoadConfig → NewMapper
// （真实 HTTPInferencer → httptest stub 推理服务）→ Start → 指标/事件 → Stop（US-5）。
func TestV0310MapperE2E(t *testing.T) {
	var seen sync.Map
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			FrameSeq uint64 `json:"frameSeq"`
			Image    string `json:"image"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Image == "" {
			t.Errorf("帧图为空")
			http.Error(w, "empty image", http.StatusBadRequest)
			return
		}
		seen.Store(req.FrameSeq, true)
		_ = json.NewEncoder(w).Encode(pkgvideo.InferenceResult{
			Detections: []pkgvideo.Detection{{Label: "motion", Score: 0.5, BBox: [4]float64{0, 0, 8, 8}}},
		})
	}))
	t.Cleanup(stub.Close)

	cfgPath := filepath.Join(t.TempDir(), "video.json")
	if err := writeFile(cfgPath, []byte(fmt.Sprintf(
		`{"deviceName":"cam-e2e","source":{"type":"synthetic","synthetic":{"fps":20,"width":32,"height":24}},"inference":{"url":%q,"timeoutMs":2000}}`,
		stub.URL))); err != nil {
		t.Fatalf("写配置: %v", err)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	pub := &stubPublisher{}
	m, err := NewMapper(cfg, WithEventPublisher(pub))
	if err != nil {
		t.Fatalf("NewMapper: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		props, _ := m.Collect()
		return props["inferTotal"] >= 3
	}, "e2e 推理不足 3 帧")
	props, _ := m.Collect()
	if props["detectionsLast"] != 1 || props["framesDropped"] > 3 {
		t.Fatalf("e2e 指标异常: %v", props)
	}
	topics, _ := pub.snapshot()
	if len(topics) == 0 {
		t.Fatalf("e2e 事件未发布")
	}
	_ = m.Stop()
}

// TestV0310MapperRestartSequence stream 快速启停序列（复核 P1-1 回归锚）：
// 修复前 stopOnce 一次性 vs Start 重建 stopCh 组合使第二次 Stop 永久阻塞
// （HandleCommand 同步内联会卡死 edgecore 全部设备指令）。修复后每次
// Stop 必须在时限内返回，且重启后循环真实重新拉流（新帧继续推理）。
func TestV0310MapperRestartSequence(t *testing.T) {
	inf := &stubInfer{result: &pkgvideo.InferenceResult{}}
	// 工厂每次返回新 stub 源：验证重启后确实重新拉流（callCount 继续增长）。
	m, err := NewMapper(testConfig("http://unused"),
		WithSource(func(*Config) (pkgvideo.FrameSource, error) { return newStubSource(3, false), nil }),
		WithInferencer(func(*Config) pkgvideo.Inferencer { return inf }))
	if err != nil {
		t.Fatalf("NewMapper: %v", err)
	}
	withTimeout := func(op func() error, name string) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- op() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s 卡死（5s 超时）——Stop 重启序列死锁回归", name)
		}
	}
	for round := 1; round <= 3; round++ {
		rep, err := m.HandleCommand(mapper.DeviceCommand{Property: "stream", Value: 1})
		if err != nil || rep.Properties["streamOn"] != 1 {
			t.Fatalf("第 %d 轮启动失败: err=%v props=%v", round, err, rep.Properties)
		}
		waitFor(t, 3*time.Second, func() bool { return inf.callCount() >= round*3 },
			fmt.Sprintf("第 %d 轮推理未达 3 帧", round))
		stopErr := error(nil)
		withTimeout(func() error {
			_, err := m.HandleCommand(mapper.DeviceCommand{Property: "stream", Value: 0})
			stopErr = err
			return err
		}, fmt.Sprintf("第 %d 次 stream=0（Stop）", round))
		if stopErr != nil {
			t.Fatalf("第 %d 次 Stop 报错: %v", round, stopErr)
		}
		props, _ := m.Collect()
		if props["streamOn"] != 0 {
			t.Fatalf("第 %d 轮停止后 streamOn 应为 0: %v", round, props)
		}
		// 循环真停：callCount 稳定（100ms 窗口内不增长）。
		n1 := inf.callCount()
		time.Sleep(100 * time.Millisecond)
		if n2 := inf.callCount(); n2 != n1 {
			t.Fatalf("第 %d 轮停止后循环仍在跑: %d → %d", round, n1, n2)
		}
	}
	if err := m.Stop(); err != nil { // 未运行时 Stop 幂等
		t.Fatalf("终态 Stop 幂等: %v", err)
	}
}

// TestV0310SlotConsumeThenOverwrite 复核 P1-2 语义锚：Take 消费清槽后，
// 后续 Put 覆盖空槽不计丢弃；只有覆盖未消费帧才计。
func TestV0310SlotConsumeThenOverwrite(t *testing.T) {
	slot := pkgvideo.NewLatestSlot()
	mk := func(seq uint64) *pkgvideo.Frame { return &pkgvideo.Frame{Seq: seq} }
	slot.Put(mk(10))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if f, ok := slot.Take(ctx, 0); !ok || f.Seq != 10 {
		t.Fatalf("首取失败: ok=%v", ok)
	}
	slot.Put(mk(11)) // 覆盖已清空槽 → 不计丢弃
	if got := slot.Dropped(); got != 0 {
		t.Fatalf("已消费帧的覆盖被误计丢弃: %d", got)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if f, ok := slot.Take(ctx2, 0); !ok || f.Seq != 11 {
		t.Fatalf("二次取帧失败: ok=%v seq=%d", ok, f.Seq)
	}
}
