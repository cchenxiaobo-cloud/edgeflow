// v0.6.0「真多活」扩展面：心跳租约（grant-per-heartbeat，主线裁决 D1）。
//
// 每次心跳 = Grant(ttl) + Put(hbKey, {lastSeen}, WithLease) 两条 RPC 单函数完成；
// 键永远绑定「最后一次 Put 携带的租约」→ 无租约 ID 状态、无恢复逻辑、
// 无跨副本身份协调（设计 §1.2 候选 B）。租约到期 etcd 自动删键 = 判离线。
//
// 本文件同时承载 EDGEFLOW_CLOUDCORE_NODE_LEASE_TTL 的解析（主线裁决 D2：
// 独立 env、默认 300s、与 NODE_TIMEOUT 解耦；仅外部模式消费）。
package etcdstore

import (
	"context"
	"fmt"
	"os"
	"time"

	"edgeflow/pkg/log"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// 心跳租约 TTL 相关环境变量与默认值（主线裁决 D2）：
//   - EDGEFLOW_CLOUDCORE_NODE_LEASE_TTL：判活租约 TTL（外部模式）；
//     未设置 → 300s（10× 心跳周期 30s）；显式 <90s → Warn（低于 3× 心跳周期的
//     护栏，抖动误判风险）；≤0 / 非法 → fail-fast；
//   - 仅外部模式（ENDPOINTS 非空）消费；embed/纯内存模式下显式设置 →
//     Warn 忽略（M2/M15，由装配层 warnLeaseTTLIgnored 处理）。
const (
	// EnvLeaseTTL 覆盖判活租约 TTL（Go duration 或秒数）。
	EnvLeaseTTL = "EDGEFLOW_CLOUDCORE_NODE_LEASE_TTL"
	// DefaultLeaseTTL 是默认判活租约 TTL（300s = 10× 心跳周期；vs v0.5.0
	// NODE_TIMEOUT=180s 是「检测延迟」而 TTL 是「故障免疫」，二者解耦）。
	DefaultLeaseTTL = 300 * time.Second
	// LeaseTTLWarnBelow 是低于即告警的阈值（<90s = 3× 心跳周期护栏，E12）。
	LeaseTTLWarnBelow = 90 * time.Second
)

// LeaseKV 是心跳租约扩展面（外部模式装配要求 kv 满足）。
type LeaseKV interface {
	// GrantHeartbeatLease 单函数完成 Grant(ttl) + Put(key, value, WithLease)。
	// 任一步失败 → error，不得产生「有租约无键/有键无租约」半状态：
	// Put 失败时**不** Revoke 新租约（不引入 Revoke 竞态面），孤儿租约 ≤TTL
	// 自灭（设计 §1.2）。
	GrantHeartbeatLease(ctx context.Context, key string, value []byte, ttl time.Duration) error
}

// 编译期断言：*kvStore 满足扩展面。
var _ LeaseKV = (*kvStore)(nil)

// GrantHeartbeatLease 实现 LeaseKV（见接口注释）。TTL 按秒取整（etcd 粒度，
// 最小 1s）。
func (s *kvStore) GrantHeartbeatLease(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	secs := int64(ttl / time.Second)
	if secs < 1 {
		secs = 1
	}
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	lresp, err := s.leaseClient().Grant(cctx, secs)
	if err != nil {
		return fmt.Errorf("etcdstore: GrantHeartbeatLease Grant(ttl=%ds) 失败: %w", secs, err)
	}
	if _, err := s.kv.Put(cctx, key, string(value), clientv3.WithLease(lresp.ID)); err != nil {
		// 不 Revoke（设计 §1.2）：孤儿租约 ≤TTL 自灭，无半状态。
		return fmt.Errorf("etcdstore: GrantHeartbeatLease Put(%s, lease=%d) 失败: %w", key, lresp.ID, err)
	}
	return nil
}

// LeaseTTLFromEnv 解析 EDGEFLOW_CLOUDCORE_NODE_LEASE_TTL（主线裁决 D2）：
//
//	未设置  → DefaultLeaseTTL（300s）；
//	显式    → Go duration/秒数校验（>0）；<90s → Warn（3× 心跳周期护栏）；
//	          ≤0 / 非法 → fail-fast error。
//
// 注意：本函数只在外部模式装配路径调用（embed/纯内存是死路径，见
// cmd/cloudcore 装配）；解析错误一律 fail-fast（对齐 nodeRetentionFromEnv
// 风格）。
func LeaseTTLFromEnv() (time.Duration, error) {
	v := os.Getenv(EnvLeaseTTL)
	if v == "" {
		return DefaultLeaseTTL, nil
	}
	d, err := parseDurationEnv(EnvLeaseTTL, v)
	if err != nil {
		return 0, err
	}
	if d < LeaseTTLWarnBelow {
		log.Warnf("[etcdstore] 环境变量 %s=%q 低于心率周期(30s)的 3 倍（<90s），存在抖动误判风险（离线检出时延上界 ≈ 2×TTL）", EnvLeaseTTL, v)
	}
	return d, nil
}