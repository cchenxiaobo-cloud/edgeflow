package etcdstore

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SchemaVersionKey 是云端存储 schema 版本钩子的元数据键（v0.4.0 预留项，
// v0.5.0 随外部 etcd 模式一并落地）。键空间固定不变（/edgeflow/_meta/ 前缀），
// 与业务键（registry/devicestatus）隔离。
const SchemaVersionKey = "/edgeflow/_meta/schemaVersion"

// DefaultSchemaVersion 是当前存储 schema 版本（v0.5.0 外部 etcd 共享键空间
// 后的第一个显式版本；embed → 外部迁移不改变键空间与 JSON，仅需初始化本键）。
const DefaultSchemaVersion = "1"

// SchemaVersionMismatchError 表示存储中已存在其他 schema 版本（旧版本写入 /
// 其他副本迁移过 / 手工改写过）。装配层按"告警不阻断"处理（主动迁移由运维
// 按迁移 runbook 执行，见 .cluster/edgeflow-v050/subagent_01.md）。
type SchemaVersionMismatchError struct {
	Got  string
	Want string
}

func (e *SchemaVersionMismatchError) Error() string {
	return fmt.Sprintf("etcdstore: 存储 schema 版本不匹配: 现有 %q，期望 %q（可能来自其他版本部署，按迁移 runbook 处理）", e.Got, e.Want)
}

// EnsureSchemaVersion 确保存储 schema 版本与 want 一致（迁移钩子，v0.4.0 预留项）：
//   - 键不存在 → Put want（新库初始化，含首次接入外部集群的旧 embed 数据）；
//   - 存在且匹配 → nil；
//   - 存在但不匹配 → 返回 *SchemaVersionMismatchError（调用方记录告警，不阻断启动）。
func EnsureSchemaVersion(ctx context.Context, kv KVStore, want string) error {
	got, err := kv.Get(ctx, SchemaVersionKey)
	if err != nil {
		return fmt.Errorf("etcdstore: 读取 %s 失败: %w", SchemaVersionKey, err)
	}
	if got == nil {
		if err := kv.Put(ctx, SchemaVersionKey, []byte(want)); err != nil {
			return fmt.Errorf("etcdstore: 初始化 %s=%q 失败: %w", SchemaVersionKey, want, err)
		}
		return nil
	}
	if string(got) != want {
		return &SchemaVersionMismatchError{Got: string(got), Want: want}
	}
	return nil
}

// ProbeAlive 是外部 etcd 模式的启动连通性检查（设计 §1.3 R3）：对
// SchemaVersionKey 做线性一致 Get 探针——clientv3.New 是懒连接（不可达也构造
// 成功），"连接失败 = 拒绝启动"必须由显式探活落实。Get 是线性一致读（要求
// quorum），比 client.Status（单成员本地视图）更能反映集群可用性。
// 重试策略：单次 ctx 超时 5s（opTimeout 口径），失败睡 1s 重试，至多 3 次尝试，
// 最坏 ≈ 17s（预算 ≤20s）。键不存在返回 nil,nil 同样是成功 RPC，不影响判定。
func ProbeAlive(kv KVStore) error {
	const (
		probeTimeout  = 5 * time.Second
		probeRetryGap = time.Second
		maxAttempts   = 3
	)
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		_, err := kv.Get(ctx, SchemaVersionKey)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		// 鉴权被拒（etcd 启用 auth/角色未授权 /edgeflow/ 前缀）：错误含
		// PermissionDenied 码，附加专属引导文案（KNOWN-ISSUES ⑤ 契约）——
		// v0.5.0 不支持鉴权参数透传，必须到 etcd 侧授权。
		if status.Code(err) == codes.PermissionDenied {
			return fmt.Errorf("etcdstore: 启动探活被拒绝（PermissionDenied）——请检查：① 外部 etcd 侧已为连接用户授予 /edgeflow/ 键前缀读写权限（etcdctl role grant-permission）；② RBAC 用户名密码已通过 %s/%s 成对配置（v0.8.0 起支持，与 TLS/mTLS 正交）；③ 若用 mTLS，证书 CN 已在 etcd 侧映射角色（--client-cert-auth）: %w", EnvUsername, EnvPassword, err)
		}
		if attempt < maxAttempts {
			time.Sleep(probeRetryGap)
		}
	}
	return fmt.Errorf("etcdstore: 启动探活失败（%d 次尝试）: %w", maxAttempts, lastErr)
}
