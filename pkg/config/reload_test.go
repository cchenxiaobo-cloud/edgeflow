// 配置热重载器测试（WBS 2.7）：正常重载、fail-safe 回退、mtime 守卫、
// 并发读安全、SIGHUP 触发。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// reloadTestConfig 是测试用配置类型（避免测试与生产类型耦合）。
type reloadTestConfig struct {
	Port int
	Tag  string
}

// newReloadTestLoader 构造一个基于 LoadReload 的 Reloader（与 cloudcore 装配同构）。
func newReloadTestLoader(t *testing.T, path string, initial *reloadTestConfig) *Reloader[reloadTestConfig] {
	t.Helper()
	t.Setenv(PortEnvVar, "")
	t.Setenv(HubPortEnvVar, "")
	// 测试用 reloadFn：复用生产 LoadReload 后做类型映射（字段含义独立于生产 Config）
	reloadFn := func() (*reloadTestConfig, error) {
		cfg, err := LoadReload(path, 0, false)
		if err != nil {
			return nil, err
		}
		return &reloadTestConfig{Port: cfg.Port, Tag: fmt.Sprintf("v%d", cfg.Port)}, nil
	}
	return NewReloader(path, initial, reloadFn, nil)
}

// writeReloadFile 写入测试配置文件（content 为合法 cloudcore.json）。
func writeReloadFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reload.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入配置文件失败: %v", err)
	}
	return path
}

// TestReloaderNormalReload 验证正常重载生效：文件变更 → Reload → 快照更新。
func TestReloaderNormalReload(t *testing.T) {
	path := writeReloadFile(t, `{"port": 8080}`)
	initial, err := Load(path, 0, false)
	if err != nil {
		t.Fatalf("Load 不应报错: %v", err)
	}
	rel := newReloadTestLoader(t, path, &reloadTestConfig{Port: initial.Port})

	if got := rel.Get().Port; got != 8080 {
		t.Fatalf("初始快照端口 = %d，期望 8080", got)
	}

	if err := os.WriteFile(path, []byte(`{"port": 9090}`), 0o644); err != nil {
		t.Fatalf("改写配置文件失败: %v", err)
	}
	if err := rel.Reload(); err != nil {
		t.Fatalf("Reload 不应报错: %v", err)
	}
	if got := rel.Get().Port; got != 9090 {
		t.Errorf("重载后端口 = %d，期望 9090", got)
	}
	if rel.LastError() != nil {
		t.Errorf("成功重载后 LastError 应为 nil: %v", rel.LastError())
	}
}

// TestReloaderInvalidKeepsOld 验证非法配置回退：解析失败保持旧值并记录错误。
func TestReloaderInvalidKeepsOld(t *testing.T) {
	path := writeReloadFile(t, `{"port": 8080}`)
	initial, err := Load(path, 0, false)
	if err != nil {
		t.Fatalf("Load 不应报错: %v", err)
	}
	rel := newReloadTestLoader(t, path, &reloadTestConfig{Port: initial.Port})

	for _, bad := range []string{`{"port": 9090`, `{"port": 70000}`, `port=8080`} {
		if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
			t.Fatalf("写入坏配置失败: %v", err)
		}
		if err := rel.Reload(); err == nil {
			t.Fatalf("坏配置 %q 应重载失败", bad)
		}
		if got := rel.Get().Port; got != 8080 {
			t.Errorf("重载失败后端口 = %d，应保持旧值 8080（坏配置 %q）", got, bad)
		}
		if rel.LastError() == nil {
			t.Error("重载失败后 LastError 不应为 nil")
		}
	}
}

// TestReloaderApplyRejectedKeepsOld 验证 apply 钩子拒绝时整体回退
// （如新端口绑定失败：拒绝重载，快照保持旧配置）。
func TestReloaderApplyRejectedKeepsOld(t *testing.T) {
	path := writeReloadFile(t, `{"port": 8080}`)
	initial, err := Load(path, 0, false)
	if err != nil {
		t.Fatalf("Load 不应报错: %v", err)
	}
	rel := NewReloader(path, &reloadTestConfig{Port: initial.Port},
		func() (*reloadTestConfig, error) {
			cfg, err := LoadReload(path, 0, false)
			if err != nil {
				return nil, err
			}
			return &reloadTestConfig{Port: cfg.Port}, nil
		},
		func(old, next *reloadTestConfig) error {
			return fmt.Errorf("新端口 %d 绑定失败", next.Port)
		})

	if err := os.WriteFile(path, []byte(`{"port": 9090}`), 0o644); err != nil {
		t.Fatalf("改写配置文件失败: %v", err)
	}
	if err := rel.Reload(); err == nil {
		t.Fatal("apply 拒绝时 Reload 应报错")
	}
	if got := rel.Get().Port; got != 8080 {
		t.Errorf("apply 拒绝后端口 = %d，应保持旧值 8080", got)
	}
	if !contains(rel.LastError().Error(), "绑定失败") {
		t.Errorf("LastError 应包含 apply 失败原因: %v", rel.LastError())
	}
}

// TestReloaderPollNoChangeNoReload 验证 mtime 未变化不重载：
// Poll 不触发 reloadFn（用调用计数断言）。
func TestReloaderPollNoChangeNoReload(t *testing.T) {
	path := writeReloadFile(t, `{"port": 8080}`)
	initial, err := Load(path, 0, false)
	if err != nil {
		t.Fatalf("Load 不应报错: %v", err)
	}
	var calls atomic.Int64
	rel := NewReloader(path, &reloadTestConfig{Port: initial.Port},
		func() (*reloadTestConfig, error) {
			calls.Add(1)
			cfg, err := LoadReload(path, 0, false)
			if err != nil {
				return nil, err
			}
			return &reloadTestConfig{Port: cfg.Port}, nil
		}, nil)

	// 首次 Reload 成功（快照与文件一致），随后 Poll 不应再触发 reloadFn
	if err := rel.Reload(); err != nil {
		t.Fatalf("Reload 不应报错: %v", err)
	}
	before := calls.Load()
	for i := 0; i < 3; i++ {
		if err := rel.Poll(); err != nil {
			t.Fatalf("Poll 不应报错: %v", err)
		}
	}
	if after := calls.Load(); after != before {
		t.Errorf("mtime 未变化时 Poll 仍触发了 %d 次 reloadFn（期望 0）", after-before)
	}
}

// TestReloaderPollDetectsChange 验证 Poll 检测到文件变化后重载。
func TestReloaderPollDetectsChange(t *testing.T) {
	path := writeReloadFile(t, `{"port": 8080}`)
	initial, err := Load(path, 0, false)
	if err != nil {
		t.Fatalf("Load 不应报错: %v", err)
	}
	rel := newReloadTestLoader(t, path, &reloadTestConfig{Port: initial.Port})
	if err := rel.Reload(); err != nil {
		t.Fatalf("Reload 不应报错: %v", err)
	}

	// 换一个不同长度/内容的文件（mtime 可能同秒，size 兜底）
	if err := os.WriteFile(path, []byte(`{"port": 10000, "hubPort": 10001}`), 0o644); err != nil {
		t.Fatalf("改写配置文件失败: %v", err)
	}
	if err := rel.Poll(); err != nil {
		t.Fatalf("Poll 应检测到变化并重载: %v", err)
	}
	if got := rel.Get().Port; got != 10000 {
		t.Errorf("Poll 重载后端口 = %d，期望 10000", got)
	}
}

// TestReloaderFileDeletedKeepsOld 验证文件被删：Reload/Poll 报错并保持旧配置
// （fail-safe，不回退默认值）。
func TestReloaderFileDeletedKeepsOld(t *testing.T) {
	path := writeReloadFile(t, `{"port": 8080}`)
	initial, err := Load(path, 0, false)
	if err != nil {
		t.Fatalf("Load 不应报错: %v", err)
	}
	rel := newReloadTestLoader(t, path, &reloadTestConfig{Port: initial.Port})
	if err := rel.Reload(); err != nil {
		t.Fatalf("Reload 不应报错: %v", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("删除配置文件失败: %v", err)
	}
	if err := rel.Reload(); err == nil {
		t.Fatal("文件被删后 Reload 应报错")
	}
	if got := rel.Get().Port; got != 8080 {
		t.Errorf("文件被删后端口 = %d，应保持旧值 8080", got)
	}
	if err := rel.Poll(); err == nil {
		t.Fatal("文件被删后 Poll 应报错")
	}
	if got := rel.Get().Port; got != 8080 {
		t.Errorf("文件被删后（Poll）端口 = %d，应保持旧值 8080", got)
	}
}

// TestReloaderMissingFileSilent 验证启动即无配置文件：Poll 静默 no-op，
// 文件出现后下一轮 Poll 自动生效。
func TestReloaderMissingFileSilent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "later.json")
	rel := newReloadTestLoader(t, path, &reloadTestConfig{Port: 8080})

	// 文件从未存在：Poll 静默（不报错、不重载）
	if err := rel.Poll(); err != nil {
		t.Fatalf("文件从未存在时 Poll 应静默: %v", err)
	}
	if got := rel.Get().Port; got != 8080 {
		t.Errorf("快照端口 = %d，应保持 8080", got)
	}

	// 文件随后出现：Poll 检测到并加载
	if err := os.WriteFile(path, []byte(`{"port": 10010}`), 0o644); err != nil {
		t.Fatalf("创建配置文件失败: %v", err)
	}
	if err := rel.Poll(); err != nil {
		t.Fatalf("文件出现后 Poll 应重载: %v", err)
	}
	if got := rel.Get().Port; got != 10010 {
		t.Errorf("文件出现后端口 = %d，期望 10010", got)
	}
}

// TestReloaderStartStop 验证定时轮询循环：文件变化后自动重载，Stop 正常退出。
func TestReloaderStartStop(t *testing.T) {
	path := writeReloadFile(t, `{"port": 8080}`)
	initial, err := Load(path, 0, false)
	if err != nil {
		t.Fatalf("Load 不应报错: %v", err)
	}
	rel := newReloadTestLoader(t, path, &reloadTestConfig{Port: initial.Port})
	rel.Start(10 * time.Millisecond)
	defer rel.Stop()

	if err := os.WriteFile(path, []byte(`{"port": 10020, "hubPort": 10021}`), 0o644); err != nil {
		t.Fatalf("改写配置文件失败: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rel.Get().Port == 10020 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := rel.Get().Port; got != 10020 {
		t.Errorf("定时轮询未在超时时间内生效，端口 = %d", got)
	}
}

// TestReloaderConcurrentGetDuringReload 验证并发读不 panic、无数据竞争：
// 多 goroutine 高频 Get 的同时持续 Reload（配合 go test -race 运行）。
func TestReloaderConcurrentGetDuringReload(t *testing.T) {
	path := writeReloadFile(t, `{"port": 8080}`)
	initial, err := Load(path, 0, false)
	if err != nil {
		t.Fatalf("Load 不应报错: %v", err)
	}
	rel := newReloadTestLoader(t, path, &reloadTestConfig{Port: initial.Port})

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = rel.Get() // 读侧拿不可变快照
				}
			}
		}()
	}
	for i := 0; i < 50; i++ {
		_ = rel.Reload()
		_ = rel.Poll()
	}
	close(stop)
	wg.Wait()
}

// TestWatchSIGHUP 验证 SIGHUP 触发重载回调，stop 注销监听。
func TestWatchSIGHUP(t *testing.T) {
	called := make(chan struct{}, 4)
	stop := WatchSIGHUP(func() error {
		called <- struct{}{}
		return nil
	})
	defer stop()

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("发送 SIGHUP 失败: %v", err)
	}
	select {
	case <-called:
		// 信号已触发重载回调
	case <-time.After(2 * time.Second):
		t.Fatal("SIGHUP 未在超时时间内触发重载回调")
	}
}

// TestWatchSIGHUPReloadError 验证重载失败时 WatchSIGHUP 不崩溃（回调返回错误仅记日志）。
func TestWatchSIGHUPReloadError(t *testing.T) {
	stop := WatchSIGHUP(func() error { return fmt.Errorf("模拟解析失败") })
	defer stop()
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("发送 SIGHUP 失败: %v", err)
	}
	// 无断言点：能走到这里（不 panic、不退出）即通过
	time.Sleep(100 * time.Millisecond)
}

// contains 是 strings.Contains 的测试辅助（避免测试文件引入额外 import 噪声）。
func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
