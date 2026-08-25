package modelrelease

// 领跑锁能力面（设计 §5.4/WBS-6 + 主线裁决 D5）。
//
// 语义（仅在外部多副本模式有意义；embed/内存单实例 = NoopLockKV 恒成功，
// 逻辑空转无害，统一实现）：
//
//   - grant-per-claim：每次抢占/续约都是 Grant(ttl) + Put(lockKey, value,
//     WithLease) 重绑新租约（对齐 registry 的 GrantHeartbeatLease 原语，
//     不 KeepAlive 流——零续约 bug 族）；
//   - 独占判定：锁值内编码 expiresAt（= 授予时刻 + ttl）；键存在且
//     expiresAt 新鲜 → 他副本持有 → (false, nil)，本副本不推进该 release；
//     键缺失/值损坏/过期 → 接管（重新 Grant+Put 重绑租约）；
//   - 崩溃接管：领跑者崩溃 → 租约 ≤TTL 自散 + expiresAt 过期 → 他副本
//     重新 grant 接管（R-2 双执行者窗口：check-grant 非原子，极端并发下
//     双写者可能同时通过——部署幂等 + perNode CAS 收敛，设计 §13.2 R-2
//     登记窗口极小且正确性不受损）；
//   - D5：刷新周期 = max(5s, TTL/3)，刷新走独立定时器（release.go
//     refreshLoop），与批内节点部署解耦（防慢节点饿死刷新）；TTL ≥ 15s
//     时 TTL ≥ 3×refresh 恒成立（TTL=15s → refresh=5s 自洽，默认
//     TTL=60s → refresh=20s 保持原节奏）。

import (
	"context"
	"encoding/json"
	"time"
)

// LockKV 是发布领跑锁能力面（控制器依赖；生产装配 = NewEtcdLockKV 包
// 外部/embed kv，纯内存模式 = NoopLockKV）。
type LockKV interface {
	// TryAcquire 尝试以租约锁方式占用锁键（grant-per-claim + 值内过期
	// 编码）：成功获得（含接管）→ (true, nil)；键存在且持有者活跃 →
	// (false, nil)；后端错误 → (false, err)（调用方本轮跳过、下轮重试）。
	TryAcquire(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error)
}

// lockValue 是锁键的持久化值（值内编码过期时间，跨副本独占判定用）。
type lockValue struct {
	// ReleaseID 是持有者 release（人读诊断用）。
	ReleaseID string `json:"releaseID"`
	// ExpiresAt 是持有者视角的过期时刻（Unix 毫秒 = 授予时刻 + ttl）；
	// 刷新时重写。
	ExpiresAt int64 `json:"expiresAt"`
}

// LockBackend 是租约锁后端的最小原语面（外部/embed 的 etcdstore.KVStore
// 与 LeaseKV 满足；测试注入 fake）。
type LockBackend interface {
	// Get 读锁键（不存在 → (nil, nil)）。
	Get(ctx context.Context, key string) ([]byte, error)
	// GrantHeartbeatLease 单函数完成 Grant(ttl) + Put(key, value, WithLease)。
	GrantHeartbeatLease(ctx context.Context, key string, value []byte, ttl time.Duration) error
}

// EtcdLockKV 是 LockKV 的 etcd 实现（外部/embed 模式装配）：
// 值内编码 expiresAt 的 grant-per-claim（见文件头语义）。
type EtcdLockKV struct {
	backend LockBackend
	now     func() time.Time
}

// 编译期断言。
var _ LockKV = (*EtcdLockKV)(nil)

// NewEtcdLockKV 构造 etcd 租约锁（backend nil → 返回 nil 由调用方防御；
// now 可注入 fake 时钟测试过期判定，nil → time.Now）。
func NewEtcdLockKV(backend LockBackend, now func() time.Time) *EtcdLockKV {
	if backend == nil {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	return &EtcdLockKV{backend: backend, now: now}
}

// TryAcquire 实现 LockKV：读锁键 → 持有者活跃（expiresAt > now）→
// 他副本 false；**同持有者刷新 → 重绑租约 true**（D5/grant-per-claim：
// 值内 expiresAt 前移）；缺失/损坏/过期 → grant+put 重绑租约 → true。
func (l *EtcdLockKV) TryAcquire(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	cur, err := l.backend.Get(ctx, key)
	if err != nil {
		return false, err
	}
	if cur != nil {
		var lv lockValue
		if err := json.Unmarshal(cur, &lv); err == nil && lv.ExpiresAt > l.now().UnixMilli() && lv.ReleaseID != string(value) {
			// 他副本持有且活跃 → 不推进
			return false, nil
		}
		// 同持有者刷新 / 值损坏 / 已过期 / 缺失 → 继续重绑租约
	}
	data, err := json.Marshal(lockValue{
		ReleaseID: string(value),
		ExpiresAt: l.now().UnixMilli() + ttl.Milliseconds(),
	})
	if err != nil {
		return false, err
	}
	if err := l.backend.GrantHeartbeatLease(ctx, key, data, ttl); err != nil {
		return false, err
	}
	return true, nil
}

// NoopLockKV 是纯内存模式的锁实现（设计 §3.4/§5.4：单实例即天然领跑者，
// 锁逻辑空转——统一实现，锁获取恒成功）。embed 单副本模式亦可用本实现
// （无跨副本写者，租约无意义）。
type NoopLockKV struct{}

// 编译期断言。
var _ LockKV = (*NoopLockKV)(nil)

// TryAcquire 实现 LockKV：恒 (true, nil)。
func (*NoopLockKV) TryAcquire(context.Context, string, []byte, time.Duration) (bool, error) {
	return true, nil
}
