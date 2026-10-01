// Package rest 提供 REST 采集器设备 Mapper（v0.41.0，spec 0014 US-2）。
//
// 形态评估（发展规划 v0.41「轮询/推送接收两形态评估后取一」）：取【轮询】——
//  1. 与 mapper 框架的 Collect 周期语义一致（采集循环统一驱动，确定性可测）；
//  2. 边缘侧不新增入站 HTTP 端口（安全面零变化；推送接收需边缘暴露写入口）；
//  3. 复用既有超时/容错/影子语义（与 modbus mapper 同构）。
//
// 「推送接收」（外部系统向边缘 POST 样本）属平台对接语义（G18 方向），
// 登记后续候选（见 ROADMAP §37 / KI §42）。
//
// 协议约定：Collect 对配置的 URL 发起 GET，应答为 JSON：
//
//	{"values": {"temperature": 25.5, "humidity": 60}}
//
// values 内全部键值对映射为设备属性（数值型）；非 200/解析失败/超时按
// 采集失败处理（Warn + 影子旧值，与 modbus mapper 容错语义一致）。
// HandleCommand 明确报错（REST 采集器为只读采集面，无指令语义）。
package rest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"edgeflow/edge/pkg/mapper"
	"edgeflow/pkg/log"
)

// 配置常量（env 与默认值）。
const (
	// EnvURL 是采集端点 URL 环境变量（未设置 = REST Mapper 不装配）。
	EnvURL = "EDGEFLOW_REST_URL"
	// EnvDevice 是设备名环境变量（默认 rest-01）。
	EnvDevice = "EDGEFLOW_REST_DEVICE"
	// EnvNamespace 是设备命名空间环境变量（默认 default）。
	EnvNamespace = "EDGEFLOW_REST_NAMESPACE"
	// EnvTimeout 是单次采集超时环境变量（默认 3s）。
	EnvTimeout = "EDGEFLOW_REST_TIMEOUT"

	// DefaultName 是 Mapper 注册名（注册表唯一键）。
	DefaultName = "rest-mapper"
	// DefaultDeviceName 是默认设备名。
	DefaultDeviceName = "rest-01"
	// DefaultNamespace 是默认命名空间。
	DefaultNamespace = "default"
	// DefaultURL 是默认采集端点（测试用；生产经 env 配置）。
	DefaultURL = "http://127.0.0.1:18080/values"
	// DefaultTimeout 是单次采集超时。
	DefaultTimeout = 3 * time.Second
)

// Option 是 RESTMapper 的可选配置项（函数式选项，与 modbus mapper 同风格）。
type Option func(*RESTMapper)

// WithURL 设置采集端点 URL。
func WithURL(url string) Option {
	return func(m *RESTMapper) { m.url = url }
}

// WithDeviceName 设置设备名（默认 rest-01）。
func WithDeviceName(name string) Option {
	return func(m *RESTMapper) { m.deviceName = name }
}

// WithNamespace 设置设备命名空间（默认 default）。
func WithNamespace(ns string) Option {
	return func(m *RESTMapper) { m.namespace = ns }
}

// WithTimeout 设置单次采集超时（默认 3s）。
func WithTimeout(d time.Duration) Option {
	return func(m *RESTMapper) { m.timeout = d }
}

// 编译期断言：RESTMapper 实现 Mapper 框架接口（含设备名/命名空间声明）。
var (
	_ mapper.DeviceMapper            = (*RESTMapper)(nil)
	_ mapper.DeviceNameResolver      = (*RESTMapper)(nil)
	_ mapper.DeviceNamespaceResolver = (*RESTMapper)(nil)
)

// RESTMapper 是 REST 轮询采集器（单设备/单端点；多端点聚合为后续候选）。
type RESTMapper struct {
	url        string
	deviceName string
	namespace  string
	timeout    time.Duration
	client     *http.Client
}

// New 创建 REST Mapper（未启动）。url 为空时从环境变量 EDGEFLOW_REST_URL
// 读取，仍未设置则用默认端点。namespace 优先级与 modbus mapper 一致：
// 选项 > env > default（保证 DeviceNamespace 恒非空）。
func New(url string, opts ...Option) *RESTMapper {
	if url == "" {
		url = os.Getenv(EnvURL)
	}
	if url == "" {
		url = DefaultURL
	}
	m := &RESTMapper{
		url:        url,
		deviceName: DefaultDeviceName,
		timeout:    DefaultTimeout,
		client:     &http.Client{},
	}
	// 优先级统一为 option > env > default（spec 0014 US-2）：env 在选项
	// 之前应用，选项覆盖 env；非法 env 告警回退默认。
	if d := os.Getenv(EnvDevice); d != "" {
		m.deviceName = d
	}
	if d := os.Getenv(EnvTimeout); d != "" {
		if n, err := strconv.Atoi(d); err == nil && n > 0 {
			m.timeout = time.Duration(n) * time.Second
		} else {
			log.Warnf("%s 非法（%q），回退默认 %s", EnvTimeout, d, DefaultTimeout)
		}
	}
	for _, o := range opts {
		o(m)
	}
	if m.namespace == "" {
		m.namespace = os.Getenv(EnvNamespace)
	}
	if m.namespace == "" {
		m.namespace = DefaultNamespace
	}
	return m
}

// Addr 返回采集端点 URL（诊断/测试用）。
func (m *RESTMapper) Addr() string { return m.url }

// Name 返回注册名（注册表唯一键）。
func (m *RESTMapper) Name() string { return DefaultName }

// DeviceNames 声明本 Mapper 管理的设备名。
func (m *RESTMapper) DeviceNames() []string {
	return []string{m.deviceName}
}

// DeviceNamespace 返回设备所属命名空间。
func (m *RESTMapper) DeviceNamespace() string { return m.namespace }

// Start 启动 Mapper（幂等）：无持久连接（每次采集独立 HTTP 请求），仅日志。
func (m *RESTMapper) Start(_ context.Context) error {
	log.Infof("RESTMapper %s 启动（url=%s, timeout=%s，轮询形态——评估结论见 spec 0014 US-2）",
		m.deviceName, m.url, m.timeout)
	return nil
}

// Stop 停止 Mapper（幂等）：无持久资源释放。
func (m *RESTMapper) Stop() error {
	log.Infof("RESTMapper %s 已停止", m.deviceName)
	return nil
}

// restPayload 是采集端点的应答契约：values 内全部键值对映射为设备属性。
type restPayload struct {
	Values map[string]float64 `json:"values"`
}

// Collect 采集设备当前属性值：GET 端点 → JSON values → map。
// 非 200/解析失败/超时返回错误（采集循环按 Warn + 影子旧值处理）。
func (m *RESTMapper) Collect() (map[string]float64, error) {
	req, err := http.NewRequest(http.MethodGet, m.url, nil)
	if err != nil {
		return nil, fmt.Errorf("构造采集请求失败: %w", err)
	}
	client := &http.Client{Timeout: m.timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("采集请求 %s 失败: %w", m.url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("采集端点 %s 状态码 = %d", m.url, resp.StatusCode)
	}
	var payload restPayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("采集应答解析失败: %w", err)
	}
	if len(payload.Values) == 0 {
		return nil, errors.New("采集应答 values 为空")
	}
	return payload.Values, nil
}

// HandleCommand 处理云端指令：REST 采集器为只读采集面，无指令语义。
func (m *RESTMapper) HandleCommand(cmd mapper.DeviceCommand) (mapper.DeviceReport, error) {
	return mapper.DeviceReport{}, fmt.Errorf(
		"REST 采集器为只读采集面，不支持指令（device=%s, property=%s）——设定值类指令走 modbus/mqtt 链路",
		cmd.DeviceName, cmd.Property)
}
