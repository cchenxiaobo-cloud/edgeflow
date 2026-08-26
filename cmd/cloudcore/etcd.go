package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"edgeflow/cloud/pkg/cloudhub"
	"edgeflow/cloud/pkg/devicestatus"
	"edgeflow/cloud/pkg/etcdstore"
	"edgeflow/cloud/pkg/modelrelease"
	"edgeflow/cloud/pkg/modelrepo"
	"edgeflow/cloud/pkg/registry"
	"edgeflow/pkg/httpx"
	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"
)

// nodeRetentionFromEnv 解析 EDGEFLOW_CLOUDCORE_NODE_RETENTION（默认
// registry.OfflineTTLDefault=24h）。支持 "24h" 或纯秒数；非法值 fail-fast
// 报错（与 nodecontroller.DurationsFromEnv 同约定）。该值同时喂给内存
// OfflineTTL 与 etcd GC 阈值，保证内存/etcd 同一口径（设计文档 §3）。
func nodeRetentionFromEnv() (time.Duration, error) {
	v := os.Getenv("EDGEFLOW_CLOUDCORE_NODE_RETENTION")
	if v == "" {
		return registry.OfflineTTLDefault, nil
	}
	if d, err := time.ParseDuration(v); err == nil {
		if d <= 0 {
			return 0, fmt.Errorf("EDGEFLOW_CLOUDCORE_NODE_RETENTION=%q 必须为正时长", v)
		}
		return d, nil
	}
	var secs int64
	if _, err := fmt.Sscanf(v, "%d", &secs); err != nil || secs <= 0 {
		return 0, fmt.Errorf("EDGEFLOW_CLOUDCORE_NODE_RETENTION=%q 无效（支持 \"24h\" 或正整数秒）", v)
	}
	return time.Duration(secs) * time.Second, nil
}

// warnEmbedFieldsIgnored 在外部 etcd 模式下检测用户显式设置了 embed 专用
// 环境变量（DATA_DIR/CLIENT_URL/PEER_URL/QUOTA/COMPACTION/STRICT），逐一
// 告警「仅 embed 模式生效，当前被忽略」（前置条件 #4）。显式设置 = 用户
// 可能在期待 embed 行为 → 明示忽略，防止"配了但没生效"的静默混淆。
func warnEmbedFieldsIgnored() {
	pairs := []struct {
		name, val string
	}{
		{etcdstore.EnvDataDir, os.Getenv(etcdstore.EnvDataDir)},
		{etcdstore.EnvClientURL, os.Getenv(etcdstore.EnvClientURL)},
		{etcdstore.EnvPeerURL, os.Getenv(etcdstore.EnvPeerURL)},
		{etcdstore.EnvQuotaBackendBytes, os.Getenv(etcdstore.EnvQuotaBackendBytes)},
		{etcdstore.EnvAutoCompactionMode, os.Getenv(etcdstore.EnvAutoCompactionMode)},
		{etcdstore.EnvAutoCompactionRetention, os.Getenv(etcdstore.EnvAutoCompactionRetention)},
		{etcdstore.EnvStrict, os.Getenv(etcdstore.EnvStrict)},
	}
	for _, p := range pairs {
		if p.val != "" {
			log.Warnf("[etcdstore] 外部模式：环境变量 %s 仅 embed 模式生效，当前被忽略（显式设置值 %q）", p.name, p.val)
		}
	}
}

// warnLeaseTTLIgnored 在 embed / 纯内存模式下检测用户显式设置了
// EDGEFLOW_CLOUDCORE_NODE_LEASE_TTL（仅外部模式生效的判活租约 TTL）→
// Warn 忽略（主线裁决 D2 + 兼容矩阵 M2/M15：不报错，对齐
// warnEmbedFieldsIgnored 同族风格）。
func warnLeaseTTLIgnored() {
	if v := os.Getenv(etcdstore.EnvLeaseTTL); v != "" {
		log.Warnf("[etcdstore] 当前运行模式不使用租约判活（embed/纯内存）：环境变量 %s 仅外部模式（EDGEFLOW_CLOUDCORE_ETCD_ENDPOINTS 非空）生效，当前被忽略（显式设置值 %q）", etcdstore.EnvLeaseTTL, v)
	}
}

// envMultiReplica 是多副本模式标识（主线裁决 D3）：External() && 本值为
// 1/true → /healthz 反映 etcd 连接（失联 >TTL → 503 → K8s liveness 重启
// 自愈）；其余形态保持 v0.5.0 语义（healthz 恒 200 进程存活语义）。
// 纯 env 派生（Chart 在 external.enabled && replicaCount>1 时自动注入）。
const envMultiReplica = "EDGEFLOW_CLOUDCORE_MULTI_REPLICA"

// multiReplicaFromEnv 解析 EDGEFLOW_CLOUDCORE_MULTI_REPLICA：
// "1"/"true" → 生效；空/"0"/"false" → 关闭；其他值 → fail-fast。
// 注意：仅外部模式装配路径调用（纯内存/embed 是死路径，不解析不报错）。
func multiReplicaFromEnv() (bool, error) {
	switch v := os.Getenv(envMultiReplica); v {
	case "", "0", "false":
		return false, nil
	case "1", "true":
		return true, nil
	default:
		return false, fmt.Errorf("%s=%q 只支持空/'0'/'false'（关）或 '1'/'true'（开）", envMultiReplica, v)
	}
}

// etcdHealthChecker 是 /healthz 的 etcd 连接健康检查抽象（外部多副本模式）。
// 实现：registry.LeaseEtcdRegistry（续约/重扫/watch 的成功接触时间戳）。
type etcdHealthChecker interface {
	EtcdHealthyWithin(d time.Duration) bool
}

// etcdAwareHealthz 构造多副本模式的 /healthz：底层 etcd 失联超过 staleAfter
// （= 判活租约 TTL）→ 503（K8s liveness 失败 → 重启副本自愈，主线裁决 D3）；
// 健康时输出与 httpx.Healthz 完全一致的 200 响应（进程存活语义不变）。
func etcdAwareHealthz(checker etcdHealthChecker, staleAfter time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checker.EtcdHealthyWithin(staleAfter) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status": "unhealthy",
				"reason": fmt.Sprintf("etcd 失联超过 %v（多副本判活依赖 etcd 租约），等待 K8s liveness 重启自愈", staleAfter),
			})
			return
		}
		httpx.Healthz()(w, r)
	}
}

// safeStartEmbeddedKV 兜住坏 WAL 的 panic：基础层实测（subagent_04 §6.1）
// 数据目录损坏时 etcd 在 raft 恢复阶段是进程内 panic 而非返回 error，
// 必须 recover 才能按 STRICT 决策（=1 拒绝启动 / 默认降级纯内存 + 告警）。
func safeStartEmbeddedKV(cfg etcdstore.Config) (et *etcdstore.EmbeddedEtcd, kv etcdstore.KVStore, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("嵌入式 etcd 启动 panic（数据目录可能损坏）: %v", r)
			if et != nil {
				_ = et.Close()
				et = nil
				kv = nil
			}
		}
	}()
	return etcdstore.NewEmbeddedKV(cfg)
}

// ── v0.7.0 模型仓库/灰度发布装配 ───────────────────────────────────────

// 环境变量（设计 §9.3 配置表；全部可选，非法值 fail-fast）。
const (
	// envReleaseScanInterval 是发布控制器扫描周期（默认 5s，>0）。
	envReleaseScanInterval = "EDGEFLOW_CLOUDCORE_RELEASE_SCAN_INTERVAL"
	// envReleaseLockTTL 是发布领跑锁租约 TTL（默认 60s，>=15s——D5 护栏：
	// TTL ≥ 3×refresh，refresh=max(5s,TTL/3)；仅外部模式消费，embed/纯内存
	// 显式设置 → Warn 忽略，并入 warnEmbedFieldsIgnored 同族）。
	envReleaseLockTTL = "EDGEFLOW_CLOUDCORE_RELEASE_LOCK_TTL"
	// envReleaseGCEnabled / envReleaseGCKeep 是终态发布 GC 开关与保留条数
	// （v0.8.0，L28）：默认关闭（L31 审计口径——终态 release 键永久保留）；
	// 开启后控制器在发布进入终态时按 keep 保留最近 N 条终态，旧终态及其
	// 逐节点结果被清理（内存与 etcd 键空间同步）。keep 默认 100，≥1；
	// 仅开启时校验（关闭时配错不阻断启动，对齐外部变量忽略惯例）。
	envReleaseGCEnabled = "EDGEFLOW_CLOUDCORE_RELEASE_GC_ENABLED"
	envReleaseGCKeep    = "EDGEFLOW_CLOUDCORE_RELEASE_GC_KEEP"
	// envMirrorCheck / envMirrorCheckTimeout / envRegistryToken 是发布前
	// 镜像探活配置（v0.9.0，R-1）：mode 空/off=不检查（零行为变化）、
	// warn=失败仅告警、fail=失败阻断发布（422）；timeout 默认 5s（>0）；
	// token 为私有 registry 的 Bearer token（可选）。
	envBatchParallel      = "EDGEFLOW_CLOUDCORE_RELEASE_BATCH_PARALLEL"
	envMirrorCheck        = "EDGEFLOW_CLOUDCORE_RELEASE_MIRROR_CHECK"
	envMirrorCheckTimeout = "EDGEFLOW_CLOUDCORE_RELEASE_MIRROR_CHECK_TIMEOUT"
	envRegistryToken      = "EDGEFLOW_CLOUDCORE_REGISTRY_TOKEN"
)

// releaseScanIntervalFromEnv 解析发布控制器扫描周期（默认 5s；支持
// "5s" 或正整数秒；非法值 fail-fast，对齐 nodeRetentionFromEnv 风格）。
func releaseScanIntervalFromEnv() (time.Duration, error) {
	v := os.Getenv(envReleaseScanInterval)
	if v == "" {
		return modelrelease.DefaultScanInterval, nil
	}
	if d, err := time.ParseDuration(v); err == nil {
		if d <= 0 {
			return 0, fmt.Errorf("%s=%q 必须为正时长（默认 5s）", envReleaseScanInterval, v)
		}
		return d, nil
	}
	var secs int64
	if _, err := fmt.Sscanf(v, "%d", &secs); err != nil || secs <= 0 {
		return 0, fmt.Errorf("%s=%q 无效（支持 \"5s\" 或正整数秒）", envReleaseScanInterval, v)
	}
	return time.Duration(secs) * time.Second, nil
}

// releaseLockTTLFromEnv 解析发布领跑锁 TTL（默认 60s；>=15s 下限，D5；
// 非法值 fail-fast）。
func releaseLockTTLFromEnv() (time.Duration, error) {
	v := os.Getenv(envReleaseLockTTL)
	if v == "" {
		return modelrelease.DefaultLockTTL, nil
	}
	d := time.Duration(0)
	if pd, err := time.ParseDuration(v); err == nil {
		d = pd
	} else {
		var secs int64
		if _, err := fmt.Sscanf(v, "%d", &secs); err != nil || secs <= 0 {
			return 0, fmt.Errorf("%s=%q 无效（支持 \"60s\" 或正整数秒）", envReleaseLockTTL, v)
		}
		d = time.Duration(secs) * time.Second
	}
	if d < modelrelease.MinLockTTL {
		return 0, fmt.Errorf("%s=%q 必须 >= %v（D5：TTL ≥ 3×refresh 护栏，refresh=max(5s,TTL/3)）",
			envReleaseLockTTL, v, modelrelease.MinLockTTL)
	}
	return d, nil
}

// releaseGCFromEnv 解析终态发布 GC 配置（v0.8.0，L28）：返回 (enabled, keep)。
// enabled 默认 false（L31 审计口径）；keep 默认 100（仅开启时校验 ≥1）。
func releaseGCFromEnv() (bool, int, error) {
	enabled := false
	switch v := os.Getenv(envReleaseGCEnabled); v {
	case "":
	case "1", "true":
		enabled = true
	default:
		return false, 0, fmt.Errorf("%s=%q 只支持空/'1'（开）", envReleaseGCEnabled, v)
	}
	keep := modelrelease.DefaultGCKeep
	if v := os.Getenv(envReleaseGCKeep); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return false, 0, fmt.Errorf("%s=%q 必须为 ≥1 的整数（默认 %d）", envReleaseGCKeep, v, modelrelease.DefaultGCKeep)
		}
		keep = n
	}
	return enabled, keep, nil
}

// releaseBatchParallelFromEnv 解析批内并行部署度（v0.10.0，D6）：默认 1
// （串行，零行为变化）；≥1 非法 fail-fast。
func releaseBatchParallelFromEnv() (int, error) {
	v := os.Getenv(envBatchParallel)
	if v == "" {
		return 1, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s=%q 必须为 ≥1 的整数（默认 1=串行）", envBatchParallel, v)
	}
	return n, nil
}

// mirrorCheckFromEnv 解析发布前镜像探活配置（v0.9.0，R-1）：返回 nil = off
// （默认，零行为变化）。非法 mode fail-fast；timeout 缺省 5s（>0）。
func mirrorCheckFromEnv() (*mirrorCheckConfig, error) {
	mode, err := modelrelease.ParseMirrorCheckMode(os.Getenv(envMirrorCheck))
	if err != nil {
		return nil, fmt.Errorf("%v", err)
	}
	if mode == modelrelease.MirrorCheckOff {
		return nil, nil
	}
	timeout := modelrelease.DefaultMirrorCheckTimeout
	if v := os.Getenv(envMirrorCheckTimeout); v != "" {
		d, derr := time.ParseDuration(v)
		if derr != nil || d <= 0 {
			return nil, fmt.Errorf("%s=%q 无效（支持 \"5s\" 等正时长，默认 %v）", envMirrorCheckTimeout, v, modelrelease.DefaultMirrorCheckTimeout)
		}
		timeout = d
	}
	return &mirrorCheckConfig{
		mode:    mode,
		timeout: timeout,
		token:   os.Getenv(envRegistryToken),
	}, nil
}

// warnReleaseLockTTLIgnored 在 embed/纯内存模式检测用户显式设置了
// EDGEFLOW_CLOUDCORE_RELEASE_LOCK_TTL（仅外部模式消费）→ Warn 忽略
// （对齐 warnLeaseTTLIgnored 同族风格，设计 §9.3）。
func warnReleaseLockTTLIgnored() {
	if v := os.Getenv(envReleaseLockTTL); v != "" {
		log.Warnf("[modelrelease] 当前运行模式不使用领跑锁租约（embed/纯内存单实例恒成功）：环境变量 %s 仅外部模式（EDGEFLOW_CLOUDCORE_ETCD_ENDPOINTS 非空）生效，当前被忽略（显式设置值 %q）", envReleaseLockTTL, v)
	}
}

// assembleModelStores 装配模型仓库存储 + 灰度发布控制器（v0.7.0，
// WBS-8；embed/外部两条 etcd 路径末尾调用）：
//
//   - EtcdModelStore（写穿 + CAS + guard 自愈 D3 + meta 复查 D7；
//     fakeKV 单测见 modelrepo/etcd_store_test.go）；
//   - Deployer（ReliableSend 注入 = hub.ReliableSendContext，Run 前
//     由调用方装配）；
//   - 领跑锁：external=true → EtcdLockKV（租约锁 grant-per-claim，
//     TTL/刷新绑定 D5）；embed/内存 → NoopLockKV（单实例恒成功，
//     逻辑空转无害，设计 §3.4/§5.4）；
//   - 加载：embed → Load（全量）；external → LoadAnchored + StartWatch
//     （锚定 + 增量应用器，断线全量重放）；加载失败以空库继续（与
//     registry Load 同口径）；
//   - 返回 (store, controller, closeModel, error)；closeModel 停 watch
//     （不关底层 kv——kv 生命周期归调用方 closeEtcd 链）。
func assembleModelStores(kv etcdstore.KVStore,
	send func(ctx context.Context, nodeID string, msg *protocol.Message, opts cloudhub.ReliableOptions) error,
	external bool, modeLabel string, sigCtx context.Context, scanInterval, lockTTL time.Duration,
	gcEnabled bool, gcKeep int, batchParallel int) (modelrepo.ModelStore, *modelrelease.Controller, func(), error) {

	closeModel := func() {} // 失败路径兜底：无可关（成功路径会覆盖）

	store, err := modelrepo.NewEtcdModelStore(kv)
	if err != nil {
		return nil, nil, closeModel, err
	}
	closeModel = func() { _ = store.Close() }

	deploy, err := modelrelease.NewDeployer(store, send)
	if err != nil {
		return nil, nil, closeModel, err
	}
	var locks modelrelease.LockKV = &modelrelease.NoopLockKV{}
	if external {
		// 外部模式：租约锁（grant-per-claim，值内编码过期；kv 恒为
		// ExtendedKV 满足 Get+GrantHeartbeatLease 能力面）
		lb, ok := kv.(modelrelease.LockBackend)
		if !ok {
			return nil, nil, closeModel, fmt.Errorf("modelrelease: 外部模式要求 LockBackend（Get + GrantHeartbeatLease），当前 KV 不满足")
		}
		locks = modelrelease.NewEtcdLockKV(lb, nil)
	}
	ctrl, err := modelrelease.NewController(store, deploy, locks, modelrelease.Options{
		ScanInterval:  scanInterval,
		LockTTL:       lockTTL,
		GCEnabled:     gcEnabled,
		GCKeep:        gcKeep,
		BatchParallel: batchParallel,
	})
	if err != nil {
		return nil, nil, closeModel, err
	}

	if external {
		rev, err := store.LoadAnchored(sigCtx)
		if err != nil {
			log.Errorf("[modelrepo] 模型仓库锚定加载失败（以空库继续，watch/重扫会收敛）: %v", err)
		}
		store.StartWatch(sigCtx)
		log.Infof("[modelrepo] 模型仓库装配完成（%s）：锚定 rev=%d", modeLabel, rev)
	} else {
		if err := store.Load(sigCtx); err != nil {
			log.Errorf("[modelrepo] 模型仓库加载失败（以空库继续）: %v", err)
		}
		models, _ := store.ListModels(sigCtx)
		releases, _ := store.ListReleases(sigCtx, "")
		deploys := 0
		for i := range models {
			if ds, err := store.ListDeployments(sigCtx, models[i].Name); err == nil {
				deploys += len(ds)
			}
		}
		log.Infof("[modelrepo] 模型仓库装配完成（%s）：恢复模型 %d / 发布 %d / 部署影子 %d（版本与 perNode 随模型/发布加载）",
			modeLabel, len(models), len(releases), deploys)
	}
	return store, ctrl, closeModel, nil
}

// 把被移除节点的设备子树（/edgeflow/devicestatus/<nodeID>/）从 etcd 级联
// 删除——"节点生命周期是设备数据唯一正确清理信号"（设计文档 §3.1）。
// 清理失败只记日志不阻断（等下一轮，与 ledger.RunCleanupLoop 同模式）。
// gcCascadeLoop 轮询注册表 GC 事件（惰性 GC + 启动清理 + CleanupLoop 产生），
// 把被移除节点的设备子树（/edgeflow/devicestatus/<nodeID>/）从 etcd 级联
// 删除——"节点生命周期是设备数据唯一正确清理信号"（设计文档 §3.1）。
// 清理失败只记日志不阻断（等下一轮，与 ledger.RunCleanupLoop 同模式）。
func gcCascadeLoop(ctx context.Context, nodeReg registry.Store, deviceStore devicestatus.Store) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, nodeID := range nodeReg.TakeGCEvents() {
				if err := deviceStore.DeleteByNode(nodeID); err != nil {
					log.Errorf("[etcdstore] 节点 %s 设备子树级联删除失败: %v", nodeID, err)
				}
			}
		}
	}
}

// assembleEtcdStores 是**嵌入式** etcd 模式的装配逻辑（v0.5.0 抽取，embed
// 路径回归锚点）：创建写穿包装 → schema 版本钩子 → Load 恢复 →
// CleanupLoop + gcCascadeLoop。外部模式 v0.6.0 起走 assembleExternalEtcdStores
// （Lease 注册表 + watch 同步，见下）。
//
//   - closeBackend 关闭底层连接：embed 模式 = et.Close()；外部模式 = kv.Close()；
//   - 任一包装创建失败 → 调用 downgrade（降级纯内存 + 告警），
//     返回 closeEtcd = 仅关底层（与 v0.4.0 embed 失败路径语义一致），
//     并以 error 返回原因（调用方只读不阻断，downgrade 已处理）；
//   - 成功路径的 closeEtcd 关停顺序（§6.1）：注册表循环 → 设备存储
//     （内部关 kv，kvStore.Close 幂等）→ closeBackend。
func assembleEtcdStores(kv etcdstore.KVStore, closeBackend func(), modeLabel string,
	retention time.Duration, sigCtx context.Context, downgrade func(string)) (registry.Store, devicestatus.Store, func(), error) {

	closeEtcd := func() { closeBackend() }

	nodeReg, err := registry.NewEtcdRegistry(kv, registry.WithOfflineTTL(retention))
	if err != nil {
		downgrade("创建 etcd 注册表包装失败")
		return nil, nil, closeEtcd, err
	}
	deviceStore, err := devicestatus.NewEtcdDeviceStore(kv)
	if err != nil {
		downgrade("创建 etcd 设备存储包装失败")
		_ = nodeReg.Close()
		return nil, nil, closeEtcd, err
	}
	closeEtcd = func() {
		if err := nodeReg.Close(); err != nil {
			log.Errorf("[etcdstore] 注册表包装关停失败: %v", err)
		}
		if err := deviceStore.Close(); err != nil {
			log.Errorf("[etcdstore] 设备存储包装关停失败（kv Close）: %v", err)
		}
		closeBackend()
	}

	// schema 版本迁移钩子（v0.4.0 预留项）：新库写入当前版本；
	// 版本不匹配 → 告警不阻断（主动迁移由运维按 runbook 处理）。
	if err := etcdstore.EnsureSchemaVersion(sigCtx, kv, etcdstore.DefaultSchemaVersion); err != nil {
		log.Warnf("[etcdstore] schema 版本检查: %v", err)
	}

	// 启动加载：先 Load+Seed 再对外服务（加载失败/坏键跳过+告警，
	// 不阻断启动——空库继续，等心跳/指令重建）。
	if err := nodeReg.Load(sigCtx); err != nil {
		log.Errorf("[etcdstore] 节点台账加载失败（以空库继续，等心跳重注册）: %v", err)
	}
	if err := deviceStore.Load(sigCtx); err != nil {
		log.Errorf("[etcdstore] 设备影子加载失败（以空库继续）: %v", err)
	}
	log.Infof("[etcdstore] %s 装配完成：恢复节点 %d 台账 + 设备影子（Desired），retention=%v",
		modeLabel, nodeReg.Count(), retention)
	// 定期 GC（每小时，与 ledger.RunCleanupLoop 同模式）+ 设备子树级联。
	nodeReg.CleanupLoop(sigCtx, time.Hour)
	go gcCascadeLoop(sigCtx, nodeReg, deviceStore)
	return nodeReg, deviceStore, closeEtcd, nil
}

// assembleExternalEtcdStores 是 v0.6.0 外部模式（真多活）装配逻辑（设计
// §12.5 + 主线裁决 D1/D2/D3）：
//
//  1. LeaseEtcdRegistry（判活 = etcd 租约视角）+ EtcdDeviceStore（CAS + watch）
//  2. schema 钩子（同值 Put 幂等，多副本首启无竞态——v0.5.0 D8 不变）
//  3. LoadAnchored 双存储锚定加载（失败以空库继续 + 日志，不阻断启动——
//     与 v0.5.0「加载失败以空库继续」同口径；rev=0 时 watch 只收新事件，
//     周期重扫 30s 内收敛）
//  4. StartWatch 双存储（watch 增量 + 续约 worker + 周期重扫）
//  5. CleanupLoop（两阶段 GC，周期 = max(30s, NODE_SCAN_INTERVAL)）+
//     gcCascadeLoop（级联删设备子树，严格排在守卫删除确认之后——R4-2）
//
// 启动失败一律 fail-fast（不降级纯内存——外部 etcd 是显式部署依赖，v0.5.0
// 决策延续）；closeEtcd 关停顺序：注册表循环 → 设备存储（内部关 kv，
// kvStore.Close 幂等）→ closeBackend。
func assembleExternalEtcdStores(kv etcdstore.ExtendedKV, closeBackend func(), modeLabel string,
	retention, leaseTTL, scanInterval time.Duration, sigCtx context.Context) (registry.Store, devicestatus.Store, func(), error) {

	closeEtcd := func() { closeBackend() }

	nodeReg, err := registry.NewLeaseEtcdRegistry(kv, registry.LeaseRegOptions{
		OfflineTTL:   retention,
		LeaseTTL:     leaseTTL,
		ScanInterval: scanInterval,
		RenewWorkers: 4,
	})
	if err != nil {
		return nil, nil, closeEtcd, err
	}
	deviceStore, err := devicestatus.NewEtcdDeviceStore(kv)
	if err != nil {
		_ = nodeReg.Close()
		return nil, nil, closeEtcd, err
	}
	closeEtcd = func() {
		if err := nodeReg.Close(); err != nil {
			log.Errorf("[etcdstore] 注册表包装关停失败: %v", err)
		}
		if err := deviceStore.Close(); err != nil {
			log.Errorf("[etcdstore] 设备存储包装关停失败（kv Close）: %v", err)
		}
		closeBackend()
	}

	// schema 版本钩子（v0.4.0 预留项，v0.5.0 随外部模式落地；v0.6.0 不 bump——
	// 新增心跳键空间属向后兼容扩展，既有键/JSON 逐字不变）。
	if err := etcdstore.EnsureSchemaVersion(sigCtx, kv, etcdstore.DefaultSchemaVersion); err != nil {
		log.Warnf("[etcdstore] schema 版本检查: %v", err)
	}

	// 锚定加载（失败以空库继续，等心跳/指令重建——与 v0.5.0 同口径）。
	rRev, err := nodeReg.LoadAnchored(sigCtx)
	if err != nil {
		log.Errorf("[etcdstore] 节点台账锚定加载失败（以空库继续，等心跳重注册）: %v", err)
	}
	dRev, err := deviceStore.LoadAnchored(sigCtx)
	if err != nil {
		log.Errorf("[etcdstore] 设备影子锚定加载失败（以空库继续）: %v", err)
	}
	log.Infof("[etcdstore] %s 装配完成：恢复节点 %d 台账 + 设备影子（Desired），retention=%v leaseTTL=%v（锚点 registry rev=%d / devicestatus rev=%d）",
		modeLabel, nodeReg.Count(), retention, leaseTTL, rRev, dRev)

	// watch 应用器 + 续约 worker（grant-per-heartbeat）+ 周期重扫 + 两阶段 GC。
	nodeReg.StartWatch(sigCtx)
	deviceStore.StartWatch(sigCtx)
	nodeReg.CleanupLoop(sigCtx, scanInterval)
	go gcCascadeLoop(sigCtx, nodeReg, deviceStore)
	return nodeReg, deviceStore, closeEtcd, nil
}
