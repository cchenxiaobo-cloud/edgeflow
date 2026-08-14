// Package mapper 定义设备 Mapper 框架（WBS 5.1）。
//
// Mapper 是边缘侧"设备接入"的统一抽象，对标 KubeEdge 的 Mapper 概念：
// 每种真实设备（Modbus / OPC-UA / MQTT 传感器等）实现 DeviceMapper 接口，
// 由 MapperRegistry 统一管理生命周期（启动/停止），并按 deviceName 把
// 云端下发的 DeviceCommand 路由到对应 Mapper、把采集结果聚合成
// DeviceReport 供 EdgeHub 周期上报。
//
// 与 DeviceTwin 的协作契约（与 DeviceTwin Agent 约定，字段不可改）：
//   - DeviceCommand（云→边，TypeDeviceCommand 可靠投递）：
//     {"deviceName":"sensor-01","namespace":"default","property":"targetTemp","value":25}
//   - DeviceReport（边→云，TypeDeviceReport 周期上报）：
//     {"deviceName":"sensor-01","namespace":"default",
//     "properties":{"temperature":25.5,"humidity":60},
//     "reportedAt":1755168000000}，properties 为 map[string]float64
package mapper

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// DeviceCommand 是云端下发的设备操作指令（JSON 字段与云边契约一致，不可改）。
type DeviceCommand struct {
	DeviceName string  `json:"deviceName"` // 目标设备名（注册表据此路由）
	Namespace  string  `json:"namespace"`  // 设备所属命名空间（默认 default）
	Property   string  `json:"property"`   // 目标属性名，如 targetTemp / reset
	Value      float64 `json:"value"`      // 属性目标值（无值命令可省略）
}

// DeviceReport 是边缘侧采集到的设备状态快照（JSON 字段与云边契约一致，不可改）。
// Properties 为属性名 → 数值的映射，供 DeviceTwin 落库与云端展示。
type DeviceReport struct {
	DeviceName string             `json:"deviceName"`
	Namespace  string             `json:"namespace"`
	Properties map[string]float64 `json:"properties"`
	ReportedAt int64              `json:"reportedAt"` // 毫秒时间戳
}

// DeviceMapper 是设备接入的统一接口：一个 Mapper 对应一种设备接入方式
// （可管理一台或多台同类设备）。
type DeviceMapper interface {
	// Name 返回 Mapper 注册名（注册表唯一键，重复注册报错）。
	Name() string
	// Start 启动设备接入：建立连接、开启采集循环。幂等，可重复调用。
	Start(ctx context.Context) error
	// Stop 停止设备接入：释放连接、停止采集循环。幂等，可多次调用。
	Stop() error
	// HandleCommand 处理一条云端下发的设备指令，返回处理后的最新状态快照。
	HandleCommand(cmd DeviceCommand) (DeviceReport, error)
	// Collect 采集设备当前属性值（属性名 → 数值）。
	Collect() (map[string]float64, error)
}

// DeviceNameResolver 是可选接口：Mapper 声明自己管理的设备名列表，
// 供注册表建立 deviceName → Mapper 的路由索引（按设备名路由的前提）。
// 未实现该接口的 Mapper 退化为"注册名即设备名"。
type DeviceNameResolver interface {
	DeviceNames() []string
}

// MapperRegistry 是 Mapper 的线程安全注册表：负责注册/查询/生命周期管理，
// 并按 deviceName 把设备指令路由到正确的 Mapper。
type MapperRegistry struct {
	mu sync.RWMutex
	// byName：注册名 → Mapper
	byName map[string]DeviceMapper
	// byDevice：设备名 → Mapper（Register 时从 DeviceNameResolver 构建）
	byDevice map[string]DeviceMapper
}

// NewRegistry 创建空的 Mapper 注册表。
func NewRegistry() *MapperRegistry {
	return &MapperRegistry{
		byName:   make(map[string]DeviceMapper),
		byDevice: make(map[string]DeviceMapper),
	}
}

// Register 注册一个 Mapper：
//   - 注册名（Name()）重复 → 返回错误（防止同名 Mapper 互相覆盖）；
//   - 若实现 DeviceNameResolver，其声明的每个设备名也注册进路由索引，
//     设备名冲突同样报错，且整个注册回滚（不留半注册状态）。
//
// Register 不启动 Mapper（启动由 StartAll 统一负责）。
func (r *MapperRegistry) Register(m DeviceMapper) error {
	if m == nil {
		return errors.New("mapper 不能为 nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	name := m.Name()
	if name == "" {
		return errors.New("mapper 注册名不能为空")
	}
	if _, ok := r.byName[name]; ok {
		return fmt.Errorf("mapper %q 重复注册", name)
	}

	// 建立设备名路由索引；冲突时回滚已添加的索引
	var added []string
	if res, ok := m.(DeviceNameResolver); ok {
		for _, dn := range res.DeviceNames() {
			if dn == "" {
				continue
			}
			if _, dup := r.byDevice[dn]; dup {
				for _, a := range added {
					delete(r.byDevice, a)
				}
				return fmt.Errorf("设备名 %q 已被其他 Mapper 注册", dn)
			}
			r.byDevice[dn] = m
			added = append(added, dn)
		}
	} else {
		// 退化路径：注册名即设备名
		r.byDevice[name] = m
	}
	r.byName[name] = m
	return nil
}

// Get 按注册名查询 Mapper。
func (r *MapperRegistry) Get(name string) (DeviceMapper, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.byName[name]
	return m, ok
}

// Route 按设备名路由到负责该设备的 Mapper：优先设备名索引，
// 未命中时回退到注册名查找（兼容未实现 DeviceNameResolver 的 Mapper）。
func (r *MapperRegistry) Route(deviceName string) (DeviceMapper, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if m, ok := r.byDevice[deviceName]; ok {
		return m, true
	}
	m, ok := r.byName[deviceName]
	return m, ok
}

// List 返回全部已注册 Mapper（按注册名排序，保证顺序稳定）。
func (r *MapperRegistry) List() []DeviceMapper {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]DeviceMapper, 0, len(r.byName))
	for _, m := range r.byName {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// StartAll 依次启动全部 Mapper（幂等，单台失败不影响其余），
// 返回所有失败的聚合错误（errors.Join）。
func (r *MapperRegistry) StartAll(ctx context.Context) error {
	var errs []error
	for _, m := range r.List() {
		if err := m.Start(ctx); err != nil {
			errs = append(errs, fmt.Errorf("启动 mapper %q: %w", m.Name(), err))
		}
	}
	return errors.Join(errs...)
}

// StopAll 依次停止全部 Mapper（幂等，单台失败不影响其余），
// 返回所有失败的聚合错误（errors.Join）。
func (r *MapperRegistry) StopAll() error {
	var errs []error
	for _, m := range r.List() {
		if err := m.Stop(); err != nil {
			errs = append(errs, fmt.Errorf("停止 mapper %q: %w", m.Name(), err))
		}
	}
	return errors.Join(errs...)
}

// Dispatch 把一条云端下发的设备指令路由到对应 Mapper 处理：
// 找不到设备返回错误（调用方应回 Ack code=error，云端可感知）。
// 这是 DeviceTwin 接入指令下发链路的入口。
func (r *MapperRegistry) Dispatch(cmd DeviceCommand) (DeviceReport, error) {
	m, ok := r.Route(cmd.DeviceName)
	if !ok {
		return DeviceReport{}, fmt.Errorf("设备 %q 未注册任何 Mapper", cmd.DeviceName)
	}
	return m.HandleCommand(cmd)
}
