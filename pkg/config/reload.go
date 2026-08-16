// 配置热重载基础设施（WBS 2.7 动态配置、热重载）。
//
// Reloader 是一个泛型热重载器：持有配置的原子快照（atomic.Pointer），
// 支持两种触发方式：
//
//  1. SIGHUP 强制重载（WatchSIGHUP）：操作员显式触发，不检查 mtime；
//  2. 定时轮询（Start/Poll）：每 60s 检查配置文件 mtime/size，变化才重载。
//
// 安全语义（fail-safe）：
//   - 重载失败（JSON 解析错误、字段非法、文件被删、apply 钩子拒绝）时
//     保持旧配置继续运行，错误经 LastError 可查、由调用方记录日志，
//     绝不崩溃、绝不回退默认值；
//   - 读取方通过 Get() 拿到不可变快照，与重载写入（原子指针交换）
//     无锁无竞态；重载本身由互斥锁串行化，多个触发源并发安全。
//
// 优先级保留：reloadFn 每次重载重新执行完整优先级链
// （命令行 > 环境变量 > 配置文件 > 默认值），env/flag 覆盖在重载后依然生效。
package config

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"edgeflow/pkg/log"
)

// DefaultWatchInterval 是定时检查配置文件 mtime 的默认周期。
const DefaultWatchInterval = 60 * time.Second

// Reloader 是泛型配置热重载器（T 为具体配置类型，如 Config / EdgeCoreConfig）。
//
// 并发模型：
//   - 快照：atomic.Pointer[T]，Get() 无锁读，Reload 成功后原子交换；
//   - 重载串行化：mu 保证 Reload/Poll/Stop 互斥，多触发源（SIGHUP 与
//     定时轮询并发）不会交错；
//   - 快照不可变：交换后旧快照不再被修改，读取方可安全持有。
type Reloader[T any] struct {
	mu       sync.Mutex
	filePath string
	// reloadFn 重新加载并解析配置（每次重载完整执行优先级链）。
	reloadFn func() (*T, error)
	// applyFn 是提交前钩子（可选）：对新旧配置做"热生效策略"——
	// 可热生效的字段直接接受，需重启的字段记录警告并回写旧值，
	// 返回错误则本次重载被整体拒绝（快照保持旧配置）。
	applyFn func(old, next *T) error

	snap     atomic.Pointer[T]
	lastMod  time.Time // 上次成功 stat 的 mtime（mu 保护）
	lastSize int64     // 上次成功 stat 的 size（mu 保护）
	lastErr  error     // 最近一次重载失败原因（mu 保护）

	stopCh  chan struct{}
	doneCh  chan struct{}
	started bool // 轮询循环是否已启动（mu 保护）
}

// NewReloader 创建热重载器。initial 为启动时已加载的配置（立即成为当前快照）。
func NewReloader[T any](filePath string, initial *T, reloadFn func() (*T, error), applyFn func(old, next *T) error) *Reloader[T] {
	r := &Reloader[T]{filePath: filePath, reloadFn: reloadFn, applyFn: applyFn}
	r.snap.Store(initial)
	return r
}

// Get 返回当前配置的不可变快照（并发安全，重载期间始终拿到完整旧值或完整新值）。
func (r *Reloader[T]) Get() *T {
	return r.snap.Load()
}

// Reload 无条件执行一次重载（SIGHUP 触发路径）：解析新配置 → apply 钩子
// → 成功则原子交换快照。任何失败保持旧配置并记录 LastError。
func (r *Reloader[T]) Reload() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.doReload()
}

// Poll 是定时路径：仅当配置文件 mtime/size 相对上次发生变化时才重载；
// 未变化时返回 nil（不触发 reloadFn）。文件缺失时：从未见过该文件
// （启动即缺省）→ 静默 no-op；文件曾存在后被删除 → 记录错误并保持旧配置。
func (r *Reloader[T]) Poll() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	fi, err := os.Stat(r.filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && r.lastMod.IsZero() {
			return nil // 从未见过配置文件（启动即缺省）：静默等待文件出现
		}
		err = fmt.Errorf("配置文件 %s 不可用: %w", r.filePath, err)
		r.lastErr = err
		return err
	}
	if fi.ModTime().Equal(r.lastMod) && fi.Size() == r.lastSize {
		return nil // mtime/size 未变化：不重载
	}
	// 先记录本次 stat 再尝试重载：失败后不重复报错，直到文件再次变化
	// （操作员修复文件会更新 mtime，下一轮轮询即重试；SIGHUP 随时可强制重试）。
	r.lastMod, r.lastSize = fi.ModTime(), fi.Size()
	return r.doReload()
}

// LastError 返回最近一次重载失败的原因（成功重载后为 nil）。
func (r *Reloader[T]) LastError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastErr
}

// Start 启动定时轮询 goroutine（每 interval 调用一次 Poll）。
// 重复调用无副作用；Stop 停止轮询并等待 goroutine 退出。
func (r *Reloader[T]) Start(interval time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return
	}
	r.started = true
	r.stopCh = make(chan struct{})
	r.doneCh = make(chan struct{})
	// 通道以局部变量传入，轮询 goroutine 不再触碰共享字段（避免与 Stop 的竞态）
	go r.pollLoop(interval, r.stopCh, r.doneCh)
}

// Stop 停止定时轮询并等待轮询 goroutine 退出（幂等）。
func (r *Reloader[T]) Stop() {
	r.mu.Lock()
	if !r.started {
		r.mu.Unlock()
		return
	}
	r.started = false
	close(r.stopCh)
	doneCh := r.doneCh
	r.mu.Unlock()
	<-doneCh
}

func (r *Reloader[T]) pollLoop(interval time.Duration, stopCh, doneCh chan struct{}) {
	defer close(doneCh)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := r.Poll(); err != nil {
				log.Warnf("配置热重载检查失败（保持当前配置运行）: %v", err)
			}
		case <-stopCh:
			return
		}
	}
}

// doReload 执行重载（调用方必须持有 mu）。
func (r *Reloader[T]) doReload() error {
	next, err := r.reloadFn()
	if err != nil {
		r.lastErr = fmt.Errorf("解析配置失败: %w", err)
		return r.lastErr
	}
	if r.applyFn != nil {
		if err := r.applyFn(r.Get(), next); err != nil {
			r.lastErr = fmt.Errorf("应用配置失败: %w", err)
			return r.lastErr
		}
	}
	r.snap.Store(next)
	r.lastErr = nil
	// 成功后刷新 mtime 记录：刚重载过的文件不会被下一轮 Poll 误判为"变化"
	if fi, err := os.Stat(r.filePath); err == nil {
		r.lastMod, r.lastSize = fi.ModTime(), fi.Size()
	}
	return nil
}

// WatchSIGHUP 注册 SIGHUP 信号监听：每次收到信号调用 reloadFn 并记录错误
// （fail-safe：重载失败只记日志，进程继续用旧配置运行）。
// 返回的 stop 函数注销信号监听并结束后台 goroutine，应在进程退出路径上
// defer 调用（run 返回前保证信号不再投递）。
func WatchSIGHUP(reloadFn func() error) (stop func()) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-hup:
				if err := reloadFn(); err != nil {
					log.Errorf("SIGHUP 配置重载失败（保持旧配置运行）: %v", err)
				}
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(hup)
		close(done)
	}
}
