// edgecore 配置（WBS 2.7 动态配置、热重载）。
//
// 加载优先级（从高到低）：环境变量 EDGEFLOW_EDGECORE_* > 配置文件
// config/edgecore.json > 默认值。
//
// 配置文件为 JSON 格式，默认路径 config/edgecore.json（可用 --config 覆盖），
// 所有字段可选（缺省回落默认值），例如：
//
//	{
//	  "cloudAddr": "ws://127.0.0.1:10000",
//	  "nodeID": "edge-node-1",
//	  "podReportInterval": "30s",
//	  "deviceReportInterval": "30s",
//	  "reconcileInterval": "5s"
//	}
//
// 敏感配置（接入 Token 等）不入文件（见 docs/ARCHITECTURE.md §5.2），
// 仍走环境变量注入。环境变量在每次加载（含热重载）时重新读取，
// 因此 env 覆盖在重载后依然保留。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"edgeflow/pkg/log"
)

// edgecore 默认值与环境变量名。
const (
	// EdgeCoreDefaultPath 是 edgecore 默认配置文件路径，可用 --config 覆盖。
	EdgeCoreDefaultPath = "config/edgecore.json"

	// 环境变量名与 edge/pkg/edgehub 及 cmd/edgecore 中的同名常量同值
	// （跨包字符串常量，避免 pkg 反向依赖 cloud/edge 包，修改需两边同步）。
	// EdgeCoreCloudAddrEnv 覆盖云端 CloudHub 地址。
	EdgeCoreCloudAddrEnv = "EDGEFLOW_EDGECORE_CLOUD_ADDR"
	// EdgeCoreNodeIDEnv 覆盖边缘节点 ID。
	EdgeCoreNodeIDEnv = "EDGEFLOW_EDGECORE_NODE_ID"
	// EdgeCoreReportIntervalEnv 覆盖 Pod 状态上报周期。
	EdgeCoreReportIntervalEnv = "EDGEFLOW_EDGECORE_REPORT_INTERVAL"
	// EdgeCoreDeviceReportIntervalEnv 覆盖设备数据上报周期。
	EdgeCoreDeviceReportIntervalEnv = "EDGEFLOW_EDGECORE_DEVICE_REPORT_INTERVAL"
	// EdgeCoreReconcileIntervalEnv 覆盖 Edged 调谐周期。
	EdgeCoreReconcileIntervalEnv = "EDGEFLOW_EDGECORE_RECONCILE_INTERVAL"

	// EdgeCoreDefaultCloudAddr 是默认云端地址（与 edgehub.DefaultCloudAddr 同值）。
	EdgeCoreDefaultCloudAddr = "ws://127.0.0.1:10000"
	// EdgeCoreDefaultReportInterval 是默认上报周期（Pod 与设备一致）。
	EdgeCoreDefaultReportInterval = 30 * time.Second
	// EdgeCoreDefaultReconcileInterval 是 Edged 默认调谐周期。
	EdgeCoreDefaultReconcileInterval = 5 * time.Second

	// minEdgeCoreInterval / maxEdgeCoreInterval 是周期类配置的合法范围，
	// 与 cmd/edgecore 的 minReportInterval/maxReportInterval 同值
	// （周期过短打爆云边通道，过长导致状态陈旧）。
	minEdgeCoreInterval = time.Second
	maxEdgeCoreInterval = 10 * time.Minute
)

// EdgeCoreConfig 是 edgecore 的运行时配置。
type EdgeCoreConfig struct {
	// CloudAddr 是云端 CloudHub 地址（如 ws://127.0.0.1:10000）。
	CloudAddr string
	// NodeID 是边缘节点 ID（注册与消息 Source 使用）。
	NodeID string
	// PodReportInterval 是 Pod 状态上报周期。
	PodReportInterval time.Duration
	// DeviceReportInterval 是设备数据上报周期。
	DeviceReportInterval time.Duration
	// ReconcileInterval 是 Edged 调谐周期（装配期生效，运行期不可热改）。
	ReconcileInterval time.Duration
}

// edgeCoreFileConfig 对应配置文件（config/edgecore.json）的磁盘格式。
// 所有字段可选：缺省字段回落默认值（nodeID 缺省回落 "edge-"+主机名）。
type edgeCoreFileConfig struct {
	CloudAddr            string `json:"cloudAddr"`
	NodeID               string `json:"nodeID"`
	PodReportInterval    string `json:"podReportInterval"`
	DeviceReportInterval string `json:"deviceReportInterval"`
	ReconcileInterval    string `json:"reconcileInterval"`
}

// LoadEdgeCore 加载 edgecore 配置：环境变量 > 配置文件 > 默认值。
// 配置文件不存在时静默使用默认值（启动语义，与 cloudcore 一致）。
func LoadEdgeCore(filePath string) (*EdgeCoreConfig, error) {
	return loadEdgeCore(filePath, false)
}

// LoadEdgeCoreReload 是热重载专用的加载入口：优先级与 LoadEdgeCore 完全一致，
// 唯一区别是配置文件不存在时返回错误（重载语义：文件缺失视为失败，
// 保持旧配置继续运行，而不是静默回落到默认值）。
func LoadEdgeCoreReload(filePath string) (*EdgeCoreConfig, error) {
	return loadEdgeCore(filePath, true)
}

func loadEdgeCore(filePath string, missingIsError bool) (*EdgeCoreConfig, error) {
	cfg := &EdgeCoreConfig{
		CloudAddr:            EdgeCoreDefaultCloudAddr,
		NodeID:               defaultEdgeCoreNodeID(),
		PodReportInterval:    EdgeCoreDefaultReportInterval,
		DeviceReportInterval: EdgeCoreDefaultReportInterval,
		ReconcileInterval:    EdgeCoreDefaultReconcileInterval,
	}

	// 第 2 优先级：配置文件（不存在时保持默认值；重载语义下缺失即错误）
	if filePath == "" {
		filePath = EdgeCoreDefaultPath
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if missingIsError {
				return nil, fmt.Errorf("配置文件 %s 不存在（热重载失败，保持旧配置）", filePath)
			}
		} else {
			return nil, fmt.Errorf("读取配置文件 %s 失败: %w", filePath, err)
		}
	} else {
		var fc edgeCoreFileConfig
		if err := json.Unmarshal(data, &fc); err != nil {
			return nil, fmt.Errorf("解析配置文件 %s 失败（请检查 JSON 格式与字段类型）: %w", filePath, err)
		}
		if fc.CloudAddr != "" {
			cfg.CloudAddr = fc.CloudAddr
		}
		if fc.NodeID != "" {
			cfg.NodeID = fc.NodeID
		}
		if fc.PodReportInterval != "" {
			if cfg.PodReportInterval, err = parseEdgeCoreInterval(filePath, "podReportInterval", fc.PodReportInterval); err != nil {
				return nil, err
			}
		}
		if fc.DeviceReportInterval != "" {
			if cfg.DeviceReportInterval, err = parseEdgeCoreInterval(filePath, "deviceReportInterval", fc.DeviceReportInterval); err != nil {
				return nil, err
			}
		}
		if fc.ReconcileInterval != "" {
			if cfg.ReconcileInterval, err = parseEdgeCoreInterval(filePath, "reconcileInterval", fc.ReconcileInterval); err != nil {
				return nil, err
			}
		}
	}

	// 第 1 优先级：环境变量（每次加载——含热重载——重新读取，覆盖保持）
	if v := os.Getenv(EdgeCoreCloudAddrEnv); v != "" {
		cfg.CloudAddr = v
	}
	if v := os.Getenv(EdgeCoreNodeIDEnv); v != "" {
		cfg.NodeID = v
	}
	cfg.PodReportInterval = edgeCoreIntervalFromEnv(EdgeCoreReportIntervalEnv, cfg.PodReportInterval)
	cfg.DeviceReportInterval = edgeCoreIntervalFromEnv(EdgeCoreDeviceReportIntervalEnv, cfg.DeviceReportInterval)
	cfg.ReconcileInterval = edgeCoreIntervalFromEnv(EdgeCoreReconcileIntervalEnv, cfg.ReconcileInterval)
	return cfg, nil
}

// defaultEdgeCoreNodeID 生成默认节点 ID：优先环境变量 EDGEFLOW_EDGECORE_NODE_ID，
// 其次 "edge-"+主机名，兜底 "edge-local"（与 edgehub.DefaultNodeID 同策略）。
func defaultEdgeCoreNodeID() string {
	if v := os.Getenv(EdgeCoreNodeIDEnv); v != "" {
		return v
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return "edge-" + h
	}
	return "edge-local"
}

// parseEdgeCoreInterval 解析配置文件中的时长字段：非法格式或超出
// 1s~10min 合法范围时返回错误（配置错误必须显式暴露，不静默回落）。
func parseEdgeCoreInterval(filePath, field, raw string) (time.Duration, error) {
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("配置文件 %s 的 %s 非法（期望时长字符串如 \"30s\"）: %w", filePath, field, err)
	}
	if d < minEdgeCoreInterval || d > maxEdgeCoreInterval {
		return 0, fmt.Errorf("配置文件 %s 的 %s 超出合法范围（%v~%v）: %q", filePath, field, minEdgeCoreInterval, maxEdgeCoreInterval, raw)
	}
	return d, nil
}

// edgeCoreIntervalFromEnv 解析环境变量为时长：未设置时保持当前值（文件值或默认值）；
// 非法/越界时记录警告并回落当前值（与 cmd/edgecore 的 durationFromEnv
// 语义一致：非法 env 视为未配置，按优先级链取下一级——文件值，无文件值时默认值）。
func edgeCoreIntervalFromEnv(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= minEdgeCoreInterval && d <= maxEdgeCoreInterval {
			return d
		}
		log.Warnf("%s 非法（%q），回落 %v", key, v, def)
	}
	return def
}
