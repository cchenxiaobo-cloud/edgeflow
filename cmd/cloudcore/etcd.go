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
