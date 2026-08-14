// ConfigSync 下发处理（M2：ConfigMap/Secret 下发链路，WBS 6.2）。
//
// 与 PodSync 同构：云端通过可靠投递下发 ConfigSync 消息，EdgeHub 读循环
// 收到后调用本文件的处理函数——DecodePayload 解析 → 按 operation 落盘/
// 删除 → 返回 nil/error，由 EdgeHub 自动回 Ack（成功 code=ok / 失败
// code=error，云端据此区分 200 / 502，超时未确认则重发同 ID，边缘幂等去重）。
package main

import (
	"encoding/json"
	"errors"
	"fmt"

	"edgeflow/edge/pkg/metamanager"
	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"
)

// ConfigSyncPayload 是 ConfigSync 消息的负载（与云端契约一致，字段不可改）：
// operation 取 add/update/delete，config 是配置的 JSON 对象（原样交给
// MetaManager 落盘，不做字段裁剪）。
type ConfigSyncPayload struct {
	Operation string          `json:"operation"` // add / update / delete
	Config    json.RawMessage `json:"config"`    // 配置的 JSON 表示（含 name/namespace/kind/data）
}

// handleConfigSync 处理一条 ConfigSync 下发消息（行为与 handlePodSync 对齐）：
//   - add/update → MetaManager.SaveConfig（config JSON 原样落盘，幂等覆盖）；
//   - delete → 从 config 对象提取 namespace/name 后 MetaManager.DeleteConfig；
//   - 未知 operation → 返回 error（与 handlePodSync 一致：EdgeHub 自动回
//     Ack code=error，云端不再重试同 ID）；
//   - 解析/存储失败返回 error，EdgeHub 自动回 Ack code=error。
func handleConfigSync(store *metamanager.Store, msg *protocol.Message) error {
	var payload ConfigSyncPayload
	if err := msg.DecodePayload(&payload); err != nil {
		return fmt.Errorf("解析 ConfigSync 负载失败: %w", err)
	}
	configJSON := string(payload.Config)
	switch payload.Operation {
	case "add", "update":
		if err := store.SaveConfig(configJSON); err != nil {
			return fmt.Errorf("保存配置元数据失败: %w", err)
		}
		log.Infof("MetaManager 已保存配置元数据（operation=%s, config=%s）",
			payload.Operation, configJSON)
		return nil
	case "delete":
		// delete 时 config 对象通常只携带 name/namespace；提取后按命名空间+名称删除
		var cfg struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		}
		if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
			return fmt.Errorf("解析 delete 操作的配置信息失败: %w", err)
		}
		if cfg.Name == "" {
			return errors.New("delete 操作的配置缺少 name 字段")
		}
		if err := store.DeleteConfig(cfg.Namespace, cfg.Name); err != nil {
			return fmt.Errorf("删除配置元数据失败: %w", err)
		}
		log.Infof("MetaManager 已删除配置元数据（namespace=%s, config=%s）", cfg.Namespace, cfg.Name)
		return nil
	default:
		return fmt.Errorf("未知的 ConfigSync operation: %q", payload.Operation)
	}
}
