// 设备状态查询与指令下发 API（WBS 5.3/2.2 简化版，云端侧装配见 main.go）。
//
// 路由（与 Pod 状态 API 同构）：
//
//	GET  /api/v1/devices                        → 全部设备状态
//	GET  /api/v1/nodes/{nodeID}/devices         → 单节点设备状态
//	POST /api/v1/nodes/{nodeID}/device-command  → 向边缘下发设备指令
package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"edgeflow/cloud/pkg/cloudhub"
	"edgeflow/cloud/pkg/devicestatus"
	"edgeflow/pkg/protocol"
)

// deviceStatusList 是设备状态查询 API 的响应形态。
//
// 选择 K8s List 风格（kind/apiVersion + items 数组），与 podStatusList
// 同构；items 恒为非 nil（空数据编码为 [] 而非 null）。
type deviceStatusList struct {
	Kind       string                      `json:"kind"`
	APIVersion string                      `json:"apiVersion"`
	Items      []devicestatus.DeviceStatus `json:"items"`
}

// listDevices 处理 GET /api/v1/devices：返回全部节点的设备状态
// （按 nodeID/namespace/deviceName 排序）；无数据时返回空数组而非 null。
func (a *nodeAPI) listDevices(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// 存储未注入（测试等场景）时按空列表处理，避免 nil 编码为 null
	items := make([]devicestatus.DeviceStatus, 0)
	if a.devices != nil {
		items = a.devices.ListAll()
	}
	if err := json.NewEncoder(w).Encode(deviceStatusList{
		Kind:       "DeviceStatusList",
		APIVersion: "v1",
		Items:      items,
	}); err != nil {
		logEncodeError("listDevices", err)
	}
}

// listNodeDevices 处理 GET /api/v1/nodes/{nodeID}/devices：返回单节点的设备状态。
//
// 语义约定（与 listNodePods 一致）：
//   - 节点不存在（从未注册）→ 404（与 /api/v1/nodes/{nodeID} 的 404 语义一致）
//   - 节点存在但无设备 → 200 + 空数组（"节点健康、只是还没设备" 与
//     "节点未知" 是两种语义，空数组让客户端可以无分支地遍历）
func (a *nodeAPI) listNodeDevices(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("nodeID")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if a.reg == nil {
		w.WriteHeader(http.StatusNotFound)
		if err := json.NewEncoder(w).Encode(map[string]string{"error": "node not found", "nodeID": nodeID}); err != nil {
			logEncodeError("listNodeDevices", err)
		}
		return
	}
	if _, ok := a.reg.Get(nodeID); !ok {
		w.WriteHeader(http.StatusNotFound)
		if err := json.NewEncoder(w).Encode(map[string]string{"error": "node not found", "nodeID": nodeID}); err != nil {
			logEncodeError("listNodeDevices", err)
		}
		return
	}
	items := make([]devicestatus.DeviceStatus, 0)
	if a.devices != nil {
		items = a.devices.ListByNode(nodeID)
	}
	if err := json.NewEncoder(w).Encode(deviceStatusList{
		Kind:       "DeviceStatusList",
		APIVersion: "v1",
		Items:      items,
	}); err != nil {
		logEncodeError("listNodeDevices", err)
	}
}

// deviceCommandRequest 是设备指令下发 API 的请求体（WBS 5.3 契约）。
// 字段与 DeviceCommand 消息负载（devicetwin.DeviceCommandPayload）一致，
// 勿单独修改。
type deviceCommandRequest struct {
	DeviceName string  `json:"deviceName"` // 目标设备名称（必填）
	Namespace  string  `json:"namespace"`  // 命名空间（缺省 "default"）
	Property   string  `json:"property"`   // 目标属性名（必填）
	Value      float64 `json:"value"`      // 期望值
}

// sendDeviceCommand 处理 POST /api/v1/nodes/{nodeID}/device-command：
// 通过可靠投递向指定边缘节点下发 DeviceCommand 消息（WBS 5.3 端到端入口）。
//
// 响应语义（与 syncPod 对齐）：
//   - 200=边缘已确认（Ack ok），且期望值已写入云端设备状态存储；
//   - 400=请求非法（JSON 解析失败/缺 deviceName/property）；
//   - 404=节点未注册/离线；
//   - 502=边缘回 error Ack（指令已送达但执行失败）；
//   - 504=确认超时重试耗尽；
//   - 500=其他发送失败（兜底）。
func (a *nodeAPI) sendDeviceCommand(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("nodeID")

	var req deviceCommandRequest
	if err := decodeWriteBody(w, r, &req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			http.Error(w, `{"error":"request body too large (limit 1MiB)"}`, http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, `{"error":"invalid json body"}`, http.StatusBadRequest)
		return
	}
	if req.DeviceName == "" || req.Property == "" {
		http.Error(w, `{"error":"deviceName and property are required"}`, http.StatusBadRequest)
		return
	}

	msg, err := protocol.NewMessage(protocol.TypeDeviceCommand, "cloud", nodeID, req)
	if err != nil {
		http.Error(w, `{"error":"build message failed"}`, http.StatusInternalServerError)
		return
	}

	if err := a.reliableSend(r.Context(), nodeID, msg, cloudhub.ReliableOptions{}); err != nil {
		if errors.Is(err, cloudhub.ErrNodeOffline) {
			http.Error(w, `{"error":"node offline or not registered"}`, http.StatusNotFound)
			return
		}
		if errors.Is(err, cloudhub.ErrAckTimeout) {
			http.Error(w, `{"error":"ack timeout after retries"}`, http.StatusGatewayTimeout)
			return
		}
		if errors.Is(err, cloudhub.ErrAckFailed) {
			// 边缘明确回 error Ack：指令已送达但处理失败，
			// 与「没送达」（404/504）语义不同，映射 502 Bad Gateway。
			http.Error(w, `{"error":"edge rejected ack"}`, http.StatusBadGateway)
			return
		}
		http.Error(w, `{"error":"send failed"}`, http.StatusInternalServerError)
		return
	}

	// 下发成功（边缘已确认）：把期望值写入云端设备状态存储，与边缘
	// Twin.Desired 构成云端视角的期望态；设备上报（Upsert）不会覆盖它
	// （见 devicestatus.Upsert 的字段级合并语义）。
	if a.devices != nil {
		a.devices.SetDesired(nodeID, req.Namespace, req.DeviceName, req.Property, req.Value)
	}

	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write([]byte(`{"status":"ok","acked":true}`)); err != nil {
		logEncodeError("sendDeviceCommand", err)
	}
}
