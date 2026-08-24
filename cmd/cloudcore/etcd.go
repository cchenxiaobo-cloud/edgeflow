package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"edgeflow/cloud/pkg/devicestatus"
	"edgeflow/cloud/pkg/etcdstore"
	"edgeflow/cloud/pkg/registry"
	"edgeflow/pkg/log"
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

// assembleEtcdStores 是嵌入式/外部两种 etcd 模式共用的装配逻辑（v0.5.0
// 抽取，避免复制）：创建写穿包装 → schema 版本钩子 → Load 恢复 →
// CleanupLoop + gcCascadeLoop。
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
