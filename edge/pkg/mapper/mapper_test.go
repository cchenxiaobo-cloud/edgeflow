package mapper

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// fakeMapper 是可编程的测试 Mapper（实现 DeviceMapper + DeviceNameResolver）。
type fakeMapper struct {
	name       string
	deviceName string
	startErr   error
	stopErr    error
	mu         sync.Mutex
	started    bool
}

func (f *fakeMapper) Name() string          { return f.name }
func (f *fakeMapper) DeviceNames() []string { return []string{f.deviceName} }
func (f *fakeMapper) Start(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = true
	err := f.startErr
	f.startErr = nil // 一次性错误：只失败首次，验证重复调用幂等
	return err
}
func (f *fakeMapper) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = false
	err := f.stopErr
	f.stopErr = nil // 一次性错误：只失败首次，验证重复调用幂等
	return err
}
func (f *fakeMapper) HandleCommand(cmd DeviceCommand) (DeviceReport, error) {
	if cmd.DeviceName != f.deviceName {
		return DeviceReport{}, fmt.Errorf("设备名不匹配: %s", cmd.DeviceName)
	}
	return DeviceReport{DeviceName: f.deviceName}, nil
}
func (f *fakeMapper) Collect() (map[string]float64, error) {
	return map[string]float64{"v": 1}, nil
}
func (f *fakeMapper) isStarted() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started
}

// plainMapper 不实现 DeviceNameResolver（无 DeviceNames 方法），
// 验证"注册名即设备名"退化路径。
type plainMapper struct {
	mu      sync.Mutex
	name    string
	started bool
}

func (p *plainMapper) Name() string { return p.name }
func (p *plainMapper) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.started = true
	return nil
}
func (p *plainMapper) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.started = false
	return nil
}
func (p *plainMapper) HandleCommand(cmd DeviceCommand) (DeviceReport, error) {
	return DeviceReport{}, nil
}
func (p *plainMapper) Collect() (map[string]float64, error) {
	return map[string]float64{}, nil
}

func TestRegistryRegisterGetList(t *testing.T) {
	r := NewRegistry()
	for _, name := range []string{"sensor-b", "sensor-a", "sensor-c"} {
		if err := r.Register(&fakeMapper{name: name, deviceName: "dev-" + name}); err != nil {
			t.Fatalf("注册 %s 失败: %v", name, err)
		}
	}
	// 注册名查询
	for _, name := range []string{"sensor-a", "sensor-b", "sensor-c"} {
		if _, ok := r.Get(name); !ok {
			t.Errorf("Get(%q) 应命中", name)
		}
	}
	if _, ok := r.Get("sensor-none"); ok {
		t.Errorf("Get 未注册的 Mapper 不应命中")
	}
	// List 按注册名排序
	list := r.List()
	if len(list) != 3 {
		t.Fatalf("List 数量 = %d, want 3", len(list))
	}
	for i, want := range []string{"sensor-a", "sensor-b", "sensor-c"} {
		if list[i].Name() != want {
			t.Errorf("List[%d] = %q, want %q（应按注册名排序）", i, list[i].Name(), want)
		}
	}
}

func TestRegistryRegisterNilAndEmptyName(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(nil); err == nil {
		t.Errorf("注册 nil Mapper 应报错")
	}
	if err := r.Register(&fakeMapper{name: "", deviceName: "d"}); err == nil {
		t.Errorf("注册空名 Mapper 应报错")
	}
}

func TestRegistryDuplicateRegister(t *testing.T) {
	r := NewRegistry()
	a := &fakeMapper{name: "dup", deviceName: "dev-a"}
	b := &fakeMapper{name: "dup", deviceName: "dev-b"}
	if err := r.Register(a); err != nil {
		t.Fatalf("首次注册失败: %v", err)
	}
	err := r.Register(b)
	if err == nil {
		t.Fatalf("同名重复注册应报错")
	}
	if _, ok := r.Get("dup"); !ok {
		t.Errorf("首次注册的 Mapper 应保留")
	}
	// 设备名冲突：整个注册回滚，不留半注册状态
	c := &fakeMapper{name: "c", deviceName: "dev-a"} // 设备名 dev-a 已被 a 占用
	if err := r.Register(c); err == nil {
		t.Fatalf("设备名冲突应报错")
	}
	if _, ok := r.Get("c"); ok {
		t.Errorf("设备名冲突后 Mapper c 不应留在注册表（回滚）")
	}
	if _, ok := r.Route("dev-a"); !ok {
		t.Errorf("原有设备名 dev-a 的路由应保留")
	}
}

func TestRegistryRouteByDeviceName(t *testing.T) {
	r := NewRegistry()
	s := &fakeMapper{name: "mock-sensor", deviceName: "sensor-01"}
	if err := r.Register(s); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	// 按设备名路由（DeviceNameResolver 索引）
	m, ok := r.Route("sensor-01")
	if !ok || m != s {
		t.Errorf("Route(sensor-01) 应命中 mock-sensor Mapper")
	}
	// 按注册名路由（回退路径）
	if m, ok := r.Route("mock-sensor"); !ok || m != s {
		t.Errorf("Route(mock-sensor) 回退到注册名查找应命中")
	}
	// 未注册设备
	if _, ok := r.Route("sensor-99"); ok {
		t.Errorf("Route(未注册设备) 不应命中")
	}
	// 未实现 DeviceNameResolver 的 Mapper：注册名即设备名
	p := &plainMapper{name: "plain"}
	if err := r.Register(p); err != nil {
		t.Fatalf("注册 plain Mapper 失败: %v", err)
	}
	if m, ok := r.Route("plain"); !ok || m != p {
		t.Errorf("Route(plain) 应按注册名命中 plain Mapper")
	}
	if _, ok := r.Route("plain-dev"); ok {
		t.Errorf("plain Mapper 未声明设备名，Route(plain-dev) 不应命中")
	}
}

func TestRegistryStartAllStopAll(t *testing.T) {
	r := NewRegistry()
	a := &fakeMapper{name: "a", deviceName: "dev-a"}
	b := &fakeMapper{name: "b", deviceName: "dev-b", startErr: errors.New("b 启动失败")}
	c := &fakeMapper{name: "c", deviceName: "dev-c", stopErr: errors.New("c 停止失败")}
	for _, m := range []DeviceMapper{a, b, c} {
		if err := r.Register(m); err != nil {
			t.Fatalf("注册失败: %v", err)
		}
	}
	// StartAll：单台失败不影响其余，错误聚合
	err := r.StartAll(context.Background())
	if err == nil {
		t.Fatalf("StartAll 应返回聚合错误")
	}
	if !a.isStarted() || !b.isStarted() || !c.isStarted() {
		t.Errorf("StartAll 应启动全部 Mapper（含失败的）")
	}
	// 幂等：再次 StartAll 不报错
	if err := r.StartAll(context.Background()); err != nil {
		t.Errorf("重复 StartAll 不应报错: %v", err)
	}
	// StopAll：单台失败不影响其余，错误聚合
	err = r.StopAll()
	if err == nil {
		t.Fatalf("StopAll 应返回聚合错误")
	}
	if a.isStarted() || b.isStarted() || c.isStarted() {
		t.Errorf("StopAll 应停止全部 Mapper（含失败的）")
	}
	// 幂等：再次 StopAll 不报错
	if err := r.StopAll(); err != nil {
		t.Errorf("重复 StopAll 不应报错: %v", err)
	}
}

func TestRegistryDispatch(t *testing.T) {
	r := NewRegistry()
	s := &fakeMapper{name: "mock-sensor", deviceName: "sensor-01"}
	if err := r.Register(s); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	report, err := r.Dispatch(DeviceCommand{DeviceName: "sensor-01", Property: "x", Value: 1})
	if err != nil {
		t.Fatalf("Dispatch 应路由成功: %v", err)
	}
	if report.DeviceName != "sensor-01" {
		t.Errorf("Dispatch 返回的 DeviceReport 设备名 = %q", report.DeviceName)
	}
	// 未注册设备 → 错误
	if _, err := r.Dispatch(DeviceCommand{DeviceName: "sensor-99"}); err == nil {
		t.Errorf("Dispatch 未注册设备应报错")
	}
}

// TestRegistryConcurrentAccess 验证并发安全（配合 -race 运行）：
// 并发 Register + Get/List/Route/Dispatch 混跑不应有数据竞争。
func TestRegistryConcurrentAccess(t *testing.T) {
	r := NewRegistry()
	const n = 32
	// 预注册一批
	for i := 0; i < n; i++ {
		f := &fakeMapper{name: fmt.Sprintf("m-%02d", i), deviceName: fmt.Sprintf("dev-%02d", i)}
		if err := r.Register(f); err != nil {
			t.Fatalf("预注册失败: %v", err)
		}
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				idx := (i + g) % n
				name := fmt.Sprintf("m-%02d", idx)
				dev := fmt.Sprintf("dev-%02d", idx)
				if _, ok := r.Get(name); !ok {
					t.Errorf("并发 Get(%q) 应命中", name)
				}
				if _, ok := r.Route(dev); !ok {
					t.Errorf("并发 Route(%q) 应命中", dev)
				}
				_ = r.List()
				if _, err := r.Dispatch(DeviceCommand{DeviceName: dev, Property: "x"}); err != nil {
					t.Errorf("并发 Dispatch(%q) 应成功: %v", dev, err)
				}
			}
		}(g)
	}
	// 并发注册新 Mapper（互不冲突的名字）
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f := &fakeMapper{name: fmt.Sprintf("extra-%d", i), deviceName: fmt.Sprintf("extra-dev-%d", i)}
			if err := r.Register(f); err != nil {
				t.Errorf("并发注册失败: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if got := len(r.List()); got != n+8 {
		t.Errorf("并发后注册表大小 = %d, want %d", got, n+8)
	}
}
