// Package devicetwin 实现边缘侧设备数字孪生（设备影子，WBS 3.5 简化版）。
//
// 对标 KubeEdge 的 DeviceTwin 模块：为每台设备维护一份影子状态——
// Desired（云端期望值，由 DeviceCommand 下发写入）与 Reported（设备
// 实际上报值，由 Mapper 采样写入）。影子是边缘侧设备数据的汇聚点：
// 周期上报循环（cmd/edgecore/device_handlers.go）从影子快照生成
// DeviceReport 消息发往云端，实现"设备数据 → 云端"的闭环。
//
// 与 Mapper（edge/pkg/mapper，另一 Agent 并行开发）的关系：
// 本包不依赖 Mapper。云端指令的设备执行能力经 DeviceCommandExecutor
// 接口注入（见该接口注释），由 cmd/edgecore 装配层把 Mapper 注册表
// 适配成接口后接入；当前装配层以 nil 占位（骨架路径）。
package devicetwin

import (
	"sort"
	"sync"
)

// DefaultNamespace 是契约中 namespace 缺省时的取值（与 K8s 默认命名空间一致）。
const DefaultNamespace = "default"

// Twin 是单台设备的数字孪生（设备影子，WBS 3.5 简化版）。
//
// Desired 是云端期望值（DeviceCommand 下发写入）；Reported 是设备实际
// 上报值（Mapper 采样写入）；LastReportedAt 是最近一次设备数据写入时间。
// 时间戳统一为 Unix 毫秒（与协议 payload 的毫秒约定一致）。
type Twin struct {
	DeviceName     string             `json:"deviceName"`     // 设备名称
	Namespace      string             `json:"namespace"`      // 命名空间（缺省 "default"）
	Desired        map[string]float64 `json:"desired"`        // 云端期望值：属性名 → 期望值
	Reported       map[string]float64 `json:"reported"`       // 设备上报值：属性名 → 实际值
	LastReportedAt int64              `json:"lastReportedAt"` // 最近一次设备数据写入时间（毫秒）
}

// twinKey 是设备影子的唯一键：namespace + "/" + deviceName。
// 设备可能跨 namespace 重名（契约允许以 namespace 区分），仅用 deviceName
// 作 key 会互相覆盖，因此键必须带 namespace（与云端 podstatus 的 podKey 同构）。
type twinKey string

// twinKeyOf 构造影子键（namespace 缺省补 "default"）。
func twinKeyOf(namespace, deviceName string) twinKey {
	if namespace == "" {
		namespace = DefaultNamespace
	}
	return twinKey(namespace + "/" + deviceName)
}

// TwinStore 是设备影子的内存态存储。
//
// 并发安全：所有方法内部加锁，可在任意 goroutine（EdgeHub 消息回调、
// 上报循环、Mapper 采样）中并发调用。Get/SnapshotAll 返回深拷贝，
// 调用方修改不会污染存储内部状态。
//
// 持久化决策（选型说明）：当前为纯内存态。理由：
//   - 影子是设备的易变运行态（与节点元数据不同，无"重启后必须仍在"的
//     验收语义），丢失后可由设备下一轮上报/云端下次指令自动重建；
//   - 对标 KubeEdge：DeviceTwin 同样以内存缓存为主，DB 持久化是可选项；
//   - 扩展点：后续如需持久化（如复用 metamanager 的 SQLite KV），可在
//     SetDesired/UpsertReported 的写路径追加落盘，存储层 API 不变，
//     不影响本包之外的任何代码（见文件末尾"持久化扩展点"注释）。
type TwinStore struct {
	mu    sync.RWMutex
	twins map[twinKey]*Twin
}

// NewStore 创建空影子存储。
func NewStore() *TwinStore {
	return &TwinStore{twins: make(map[twinKey]*Twin)}
}

// SetDesired 写入（或更新）一台设备的期望值（云端 DeviceCommand 的落点）。
//
// 语义：
//   - 设备影子不存在时自动创建——指令即声明：云端指令本身足以让设备
//     进入云端视野，无需等待 Mapper 首次采样；
//   - 仅更新指定属性的期望值，已上报属性（Reported）不受影响；
//   - namespace 缺省补 "default"；
//   - deviceName/property 为空时忽略（无法定位写入位置）。
func (s *TwinStore) SetDesired(deviceName, namespace, property string, value float64) {
	if deviceName == "" || property == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := twinKeyOf(namespace, deviceName)
	t, ok := s.twins[key]
	if !ok {
		t = &Twin{
			DeviceName: deviceName,
			Namespace:  normNamespace(namespace),
			Desired:    make(map[string]float64),
			Reported:   make(map[string]float64),
		}
		s.twins[key] = t
	}
	t.Desired[property] = value
}

// UpsertReported 写入（或更新）一台设备的实际上报值（Mapper 采样的落点）。
//
// 语义：
//   - 设备影子不存在时自动创建；
//   - properties 按属性名合并（本次未上报的属性保留原值）；
//   - reportedAt 是本次数据的采样时间（毫秒），同时刷新 LastReportedAt；
//   - namespace 缺省补 "default"；
//   - deviceName 为空或 properties 为空时忽略。
func (s *TwinStore) UpsertReported(deviceName, namespace string, properties map[string]float64, reportedAt int64) {
	if deviceName == "" || len(properties) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := twinKeyOf(namespace, deviceName)
	t, ok := s.twins[key]
	if !ok {
		t = &Twin{
			DeviceName: deviceName,
			Namespace:  normNamespace(namespace),
			Desired:    make(map[string]float64),
			Reported:   make(map[string]float64),
		}
		s.twins[key] = t
	}
	for k, v := range properties {
		t.Reported[k] = v
	}
	t.LastReportedAt = reportedAt
}

// Get 查询一台设备的影子。
// 返回值是深拷贝，调用方修改不会污染存储；namespace 缺省补 "default"；
// 不存在时返回 false。
func (s *TwinStore) Get(deviceName, namespace string) (Twin, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.twins[twinKeyOf(namespace, deviceName)]
	if !ok {
		return Twin{}, false
	}
	return cloneTwin(t), true
}

// SnapshotAll 返回全部设备影子的快照（按 namespace/deviceName 排序，
// 保证输出确定性——上报循环按此顺序生成消息，便于云端对账）。
// 无数据时返回空切片（非 nil，JSON 编码为 []）。
func (s *TwinStore) SnapshotAll() []Twin {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Twin, 0, len(s.twins))
	for _, t := range s.twins {
		out = append(out, cloneTwin(t))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].DeviceName < out[j].DeviceName
	})
	return out
}

// normNamespace 把空命名空间归一为默认值。
func normNamespace(namespace string) string {
	if namespace == "" {
		return DefaultNamespace
	}
	return namespace
}

// cloneTwin 深拷贝影子（map 必须复制，防止调用方经快照修改存储内部状态；
// 拷贝后 map 恒为非 nil，JSON 编码为 {} 而非 null）。
func cloneTwin(t *Twin) Twin {
	c := *t
	c.Desired = make(map[string]float64, len(t.Desired))
	for k, v := range t.Desired {
		c.Desired[k] = v
	}
	c.Reported = make(map[string]float64, len(t.Reported))
	for k, v := range t.Reported {
		c.Reported[k] = v
	}
	return c
}

// 持久化扩展点（选型说明见 TwinStore 注释）：
//   - 若需复用 metamanager 的 SQLite KV 持久化影子：在 SetDesired /
//     UpsertReported 写路径末尾追加一次落盘（键可用 twinKey，值为 Twin
//     的 JSON 序列化），并在 NewStore 时回放历史键重建内存表；
//   - 存储层 API 不变，调用方（装配层/上报循环/Mapper）无需感知；
//   - 落盘失败策略建议：只记 Warn 不阻断写路径——内存态始终是权威，
//     磁盘只是加速重启重建的缓存，影子数据可由上报/指令自动重建。
