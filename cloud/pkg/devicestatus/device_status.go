// Package devicestatus 实现云端设备状态存储（内存态，WBS 5.3 云端侧）。
//
// 这是云边设备数据回传链路的云端落点：边缘侧周期性上报 DeviceReport
// 消息，CloudHub 收到后经回调注入本存储，再由 cloudcore 的 HTTP API
// （/api/v1/devices、/api/v1/nodes/{nodeID}/devices）对外提供查询；
// 云端下发的设备指令（POST .../device-command）成功后也会把期望值写入
// 本存储的 Desired 字段，构成完整的云端设备影子视图（期望态 + 上报态）。
//
// 与 podstatus 包同级的定位：当前为内存 Map 形态，待集群环境就绪后，
// 本包将被 K8s apiserver 对接（Device CRD 对象 + Informer/Watch）取代，
// 具体迁移点见文件末尾"后续对接 K8s apiserver"注释。
package devicestatus

import (
	"sort"
	"sync"
)

// DeviceStatus 是单台设备的云端状态快照，字段与边→云 DeviceReport
// 消息契约一致，同时也是 HTTP API 的 JSON 形态。
//
// 时间戳统一为 Unix 毫秒（与协议 payload 的毫秒约定一致），
// JSON 字段名与协议/API 约定保持一致（小驼峰）。
type DeviceStatus struct {
	NodeID         string             `json:"nodeID"`         // 节点唯一 ID（来自消息 Source）
	DeviceName     string             `json:"deviceName"`     // 设备名称
	Namespace      string             `json:"namespace"`      // 命名空间（缺省按 "default" 处理）
	Properties     map[string]float64 `json:"properties"`     // 设备实际上报值：属性名 → 值
	Desired        map[string]float64 `json:"desired"`        // 云端期望值：属性名 → 期望值（指令下发成功时写入）
	LastReportedAt int64              `json:"lastReportedAt"` // 最近一次上报时间（毫秒时间戳）
}

// DefaultNamespace 是契约中 namespace 缺省时的取值（与 K8s 默认命名空间一致）。
const DefaultNamespace = "default"

// deviceKey 是节点内设备的唯一键：namespace + "/" + deviceName。
// 设备可能跨 namespace 重名，仅用 deviceName 作 key 会互相覆盖，
// 因此键必须带 namespace（与 podstatus 的 podKey 同构）。
type deviceKey string

// deviceKeyOf 构造设备键（namespace 缺省补 "default"）。
func deviceKeyOf(namespace, deviceName string) deviceKey {
	if namespace == "" {
		namespace = DefaultNamespace
	}
	return deviceKey(namespace + "/" + deviceName)
}

// DeviceStatusStore 是内存态设备状态存储。
//
// 并发安全：所有方法内部加锁，可在任意 goroutine（CloudHub 连接处理、
// HTTP 请求处理）中并发调用。Get/List 返回值的拷贝，调用方修改不会
// 污染存储内部状态。
//
// 数据保留策略：节点断开时不清空其设备状态（与 podstatus 一致），
// 便于查询 API 展示节点离线前的最后状态；待后续接入 apiserver 后由
// TTL/GC 策略取代。
//
// 注意（CODE-REVIEW-M1B P2-3 联动结论）：本存储与 registry 的节点
// 保留策略不对称——registry 的 Offline 节点超过保留时长（默认 24h）会被
// 惰性 GC 清理，而本存储的设备记录没有对等的自动清理，节点长期离线后
// 会留下孤儿设备数据。这里刻意不加"最后上报时间（LastReportedAt）超时"
// 的 TTL：设备缺席上报不等于节点离线（采集失败只跳过、仅指令未上报的
// 记录 LastReportedAt=0），按设备时间戳清理会误删在线节点的状态与期望值。
// 正确的清理信号是节点生命周期（registry 的 MarkOffline），但该信号有两
// 个来源（断开事件与 nodecontroller 心跳停滞扫描），且均不在本包内；
// 待 apiserver 迁移后由节点对象的 TTL/驱逐统一收敛（见文末迁移点）。
type DeviceStatusStore struct {
	mu      sync.RWMutex
	devices map[string]map[deviceKey]DeviceStatus // nodeID → (deviceKey → DeviceStatus)
}

// NewStore 创建空存储。
func NewStore() *DeviceStatusStore {
	return &DeviceStatusStore{devices: make(map[string]map[deviceKey]DeviceStatus)}
}

// Upsert 写入（或更新）一台设备的已上报状态（边→云 DeviceReport 的落点）。
//
// 语义：
//   - nodeID 为空、或 ds.DeviceName 为空时忽略（无法定位存储键）；
//   - ds.NodeID 以参数 nodeID 为准（消息来源即权威，防 payload 伪造错位）；
//   - namespace 缺省补 "default"；
//   - properties/lastReportedAt 以本次上报为准整体替换；
//   - desired 保留云端已有期望值：DeviceReport 不含 desired 字段，整体
//     覆盖会清空设备指令写入的期望值，因此这里做字段级合并。
func (s *DeviceStatusStore) Upsert(nodeID string, ds DeviceStatus) {
	if nodeID == "" || ds.DeviceName == "" {
		return
	}
	ds.NodeID = nodeID
	if ds.Namespace == "" {
		ds.Namespace = DefaultNamespace
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	byNode, ok := s.devices[nodeID]
	if !ok {
		byNode = make(map[deviceKey]DeviceStatus)
		s.devices[nodeID] = byNode
	}
	key := deviceKeyOf(ds.Namespace, ds.DeviceName)
	if existing, ok := byNode[key]; ok && len(existing.Desired) > 0 {
		ds.Desired = existing.Desired
	}
	byNode[key] = cloneDeviceStatus(ds)
}

// SetDesired 写入（或更新）一台设备的期望值（云端设备指令下发成功的落点）。
//
// 语义：仅更新指定属性的期望值；已上报属性（properties/lastReportedAt）
// 保留；设备记录不存在时自动创建（云端已下发指令，设备即进入云端视野）。
// namespace 缺省补 "default"；nodeID/deviceName/property 为空时忽略。
func (s *DeviceStatusStore) SetDesired(nodeID, namespace, deviceName, property string, value float64) {
	if nodeID == "" || deviceName == "" || property == "" {
		return
	}
	key := deviceKeyOf(namespace, deviceName)
	s.mu.Lock()
	defer s.mu.Unlock()
	byNode, ok := s.devices[nodeID]
	if !ok {
		byNode = make(map[deviceKey]DeviceStatus)
		s.devices[nodeID] = byNode
	}
	ds, ok := byNode[key]
	if !ok {
		ds = DeviceStatus{
			NodeID:     nodeID,
			DeviceName: deviceName,
			Namespace:  normNamespace(namespace),
		}
	}
	// 先拷贝再改：避免原地修改存储内部 map；拷贝同时把 nil map 归一为空 map
	ds = cloneDeviceStatus(ds)
	ds.Desired[property] = value
	byNode[key] = ds
}

// Count 返回存储内设备状态记录总数（供 /metrics 的 devices_total 指标使用）。
func (s *DeviceStatusStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, byKey := range s.devices {
		n += len(byKey)
	}
	return n
}

// Get 查询单台设备状态。
//
// 返回值是拷贝，调用方修改不会污染存储。
// namespace 缺省补 "default"；不存在时返回 false。
func (s *DeviceStatusStore) Get(nodeID, namespace, deviceName string) (DeviceStatus, bool) {
	if nodeID == "" || deviceName == "" {
		return DeviceStatus{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	ds, ok := s.devices[nodeID][deviceKeyOf(namespace, deviceName)]
	return cloneDeviceStatus(ds), ok
}

// Delete 删除一台设备状态；不存在时静默成功（幂等）。
// namespace 缺省补 "default"；返回是否存在且被删除。
func (s *DeviceStatusStore) Delete(nodeID, namespace, deviceName string) bool {
	if nodeID == "" || deviceName == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	byNode, ok := s.devices[nodeID]
	if !ok {
		return false
	}
	key := deviceKeyOf(namespace, deviceName)
	if _, ok := byNode[key]; !ok {
		return false
	}
	delete(byNode, key)
	// 节点下已无设备时清理空 map，避免空壳节点长期驻留
	if len(byNode) == 0 {
		delete(s.devices, nodeID)
	}
	return true
}

// ListByNode 返回指定节点的全部设备状态（按 namespace/deviceName 排序，
// 保证输出确定性）；节点无设备时返回空切片（非 nil，JSON 编码为 []）。
func (s *DeviceStatusStore) ListByNode(nodeID string) []DeviceStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	byNode := s.devices[nodeID]
	out := make([]DeviceStatus, 0, len(byNode))
	for _, ds := range byNode {
		out = append(out, cloneDeviceStatus(ds))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].DeviceName < out[j].DeviceName
	})
	return out
}

// ListAll 返回全部设备状态（按 nodeID → namespace/deviceName 排序，
// 保证输出确定性）；无数据时返回空切片（非 nil，JSON 编码为 []）。
func (s *DeviceStatusStore) ListAll() []DeviceStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]DeviceStatus, 0, len(s.devices))
	for _, byNode := range s.devices {
		for _, ds := range byNode {
			out = append(out, cloneDeviceStatus(ds))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NodeID != out[j].NodeID {
			return out[i].NodeID < out[j].NodeID
		}
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

// cloneDeviceStatus 深拷贝设备状态（map 必须复制，防止调用方经快照修改
// 存储内部状态；拷贝后 map 恒为非 nil，JSON 编码为 {} 而非 null）。
func cloneDeviceStatus(ds DeviceStatus) DeviceStatus {
	c := ds
	c.Properties = make(map[string]float64, len(ds.Properties))
	for k, v := range ds.Properties {
		c.Properties[k] = v
	}
	c.Desired = make(map[string]float64, len(ds.Desired))
	for k, v := range ds.Desired {
		c.Desired[k] = v
	}
	return c
}

// 后续对接 K8s apiserver 的迁移点：
//   - 本存储的 map[nodeID]map[deviceKey]DeviceStatus 将由 apiserver 的
//     Device 对象 + Informer 缓存取代，DeviceStatus 结构体保留为 API 响应 DTO
//   - Upsert（上报落点）对应 Device status 子资源更新（Status.Twins 的 reported）
//   - SetDesired（指令落点）对应 Device spec 更新（Spec.Properties 的 desired）
//   - Delete 对应 Device 删除（或由 DeviceController 收敛）
//   - ListAll/ListByNode 对应 apiserver 的 List（按 spec.nodeName 字段过滤）
//   - 节点断开清理策略届时改为 apiserver 侧 TTL/驱逐逻辑
