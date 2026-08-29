# Mapper 框架指南（WBS 5.1 / 5.5，含 MQTT 数据面 §8）

> 日期：2026-08-14 ｜ 范围：`edge/pkg/mapper/`（框架）+ `mappers/mock_sensor/`（示例模拟设备，含 MQTT 数据面模式）
> 对应提交：`feat(mapper): device mapper framework and mock sensor (WBS 5.1/5.5)`、`feat(mapper): wire mock sensor to MQTT event bus data plane`
> 协作契约：与 DeviceTwin Agent 约定（见 §6，字段不可改）

## 1. 概述

Mapper 是边缘侧"设备接入"的统一抽象，对标 KubeEdge 的 Mapper 概念：**每种设备接入方式
（Modbus / OPC-UA / MQTT 传感器等）实现 `DeviceMapper` 接口，由 `MapperRegistry` 统一管理
生命周期，并按 deviceName 把云端下发的 `DeviceCommand` 路由到对应 Mapper，把采集结果
聚合成 `DeviceReport` 供 EdgeHub 周期上报。**

设计目标：

- **协议无关**：Mapper 只暴露 `Collect()/HandleCommand()` 两个业务原语，底层是串口、
  TCP 还是 MQTT 对上层完全透明；
- **按设备路由**：云端指令携带 `deviceName`，注册表负责找到管这台设备的 Mapper，
  DeviceTwin 侧无需关心设备具体由谁接入；
- **生命周期统一**：启动/停止由注册表批量管理，EdgeCore 启停时一行调用即可；
- **零新依赖**：仅标准库（context / sync / time / math/rand）。

```
        cloud (DeviceTwin)
            │  TypeDeviceCommand（下发）   TypeDeviceReport（上报）
            ▼
       EdgeHub (edgehub.Client)
            │                          ▲
            │ Dispatch(cmd)            │ 周期 Collect → DeviceReport
            ▼                          │
   MapperRegistry ──路由 deviceName──► DeviceMapper
                                          │
                     mappers/mock_sensor ◄┘（示例实现，真实协议适配同理）
```

## 2. 核心数据结构（与云边契约一致）

### DeviceCommand（云→边）

```go
type DeviceCommand struct {
    DeviceName string  `json:"deviceName"` // 目标设备名（注册表据此路由）
    Namespace  string  `json:"namespace"`  // 设备所属命名空间（默认 default）
    Property   string  `json:"property"`   // 目标属性名，如 targetTemp / reset
    Value      float64 `json:"value"`      // 属性目标值（无值命令可省略）
}
```

### DeviceReport（边→云）

```go
type DeviceReport struct {
    DeviceName string             `json:"deviceName"`
    Namespace  string             `json:"namespace"`
    Properties map[string]float64 `json:"properties"` // 属性名 → 数值
    ReportedAt int64              `json:"reportedAt"` // 毫秒时间戳
}
```

> ⚠️ JSON 字段名（deviceName / namespace / property / value / properties / reportedAt）
> 是与 DeviceTwin Agent 约定的云边契约，**不可修改**。

## 3. DeviceMapper 接口

```go
type DeviceMapper interface {
    Name() string                              // 注册名（注册表唯一键，重复注册报错）
    Start(ctx context.Context) error           // 启动设备接入（幂等）
    Stop() error                               // 停止设备接入（幂等，可多次调用）
    HandleCommand(cmd DeviceCommand) (DeviceReport, error) // 处理云端指令，返回最新快照
    Collect() (map[string]float64, error)      // 采集当前属性值
}
```

可选接口 `DeviceNameResolver`：一个 Mapper 可管理多台同类设备，实现
`DeviceNames() []string` 声明自己管理的设备名；可选接口 `DeviceNamespaceResolver`：
实现 `DeviceNamespace() string` 声明设备命名空间（缺省 "default"），注册表据此
建立 `namespace/deviceName → Mapper` 路由索引；未实现则退化为"注册名即设备名"。

## 4. MapperRegistry 注册表

```go
r := mapper.NewRegistry()
r.Register(m)                    // 注册（重复名/设备名冲突报错，冲突时整体回滚）
m, ok := r.Get(name)             // 按注册名查询
m, ok := r.Route(namespace, deviceName) // 按 namespace+设备名路由（优先索引，回退注册名）
r.List()                         // 全部 Mapper（按注册名排序，顺序稳定）
r.StartAll(ctx) / r.StopAll()    // 批量生命周期管理（单台失败不影响其余，errors.Join 聚合）
r.Dispatch(cmd)                  // 指令下发入口：路由 + 处理一步到位（DeviceTwin 接入点）
```

**线程安全**：内部 `sync.RWMutex` 保护，可并发注册/查询/路由/派发（`-race` 已验证）。

**生命周期语义**：

- `Register` 只登记不启动；`StartAll` 统一启动（幂等）；
- `Start(ctx)`：建立连接/开启采集循环；ctx 取消或 `Stop()` 均能退出循环；
- `Stop()`：等待采集循环退出后返回，保证停止后状态不再变化（冻结）；
- 停止后可再次 `Start`（支持重连重启场景）。

## 5. 如何开发新设备 Mapper（三步）

### 第一步：实现接口

新建 `mappers/<name>/<name>.go`，实现 `DeviceMapper`（建议实现 `DeviceNameResolver`
声明设备名）：

```go
package mydevice

type MyDevice struct { /* 设备句柄、状态、锁 */ }

func (d *MyDevice) Name() string { return "my-device" }
func (d *MyDevice) DeviceNames() []string { return []string{d.deviceName} }
func (d *MyDevice) Start(ctx context.Context) error { /* 建连 + 采集循环 */ }
func (d *MyDevice) Stop() error                     { /* 断开 + 停循环 */ }
func (d *MyDevice) HandleCommand(cmd mapper.DeviceCommand) (mapper.DeviceReport, error) {
    /* 真实设备协议写操作：如 Modbus 写寄存器；返回最新快照 */
}
func (d *MyDevice) Collect() (map[string]float64, error) {
    /* 真实设备协议读操作：如 Modbus 读保持寄存器 → 属性映射 */
}
```

要点：

- **并发安全**：采集循环与指令处理在不同 goroutine，状态必须加锁；
- **属性映射**：`Collect()` 返回的 map 键即上报属性名（DeviceTwin 落库字段），
  与设备协议寄存器/点位做一对一映射；
- **幂等**：Start/Stop 可重复调用不报错。

### 第二步：注册

```go
r := mapper.NewRegistry()
r.Register(mydevice.New("sensor-01")) // 注册名冲突/设备名冲突会报错
```

### 第三步：装配（下一轮集成，当前不修改 cmd/edgecore/main.go）

```go
// 生命周期随 EdgeCore 启停
r.StartAll(ctx)  // 启动后注册表会定期 Collect 出 DeviceReport
defer r.StopAll()

// 指令下发（DeviceTwin 侧）：EdgeHub 收到 TypeDeviceCommand 后
report, err := r.Dispatch(cmd) // 路由 + 处理，找不到设备返回 error → 回 Ack error
// 上报：把 r 各 Mapper 的 Collect() 结果打包成 TypeDeviceReport 走 client.Send(msg)
```

> 说明：EdgeCore 装配（main.go）与 DeviceTwin 的指令/上报链路属于下一轮集成，
> 本提交只交付框架 + 示例 Mapper，二者接口已就绪。

## 6. 与 DeviceTwin 的协作契约

| 项 | 约定 |
|----|------|
| 指令消息 | `TypeDeviceCommand`（云→边），走现有可靠投递（EdgeHub 回调分发） |
| 上报消息 | `TypeDeviceReport`（边→云），走边缘周期上报（EdgeHub `client.Send`） |
| 指令负载 | `{"deviceName":"sensor-01","namespace":"default","property":"targetTemp","value":25}` |
| 上报负载 | `{"deviceName":"sensor-01","namespace":"default","properties":{"temperature":25.5,"humidity":60},"reportedAt":1755168000000}` |
| 属性类型 | `properties` 为 `map[string]float64` |
| 路由键 | `deviceName`（注册表 `Route`/`Dispatch`） |

## 7. 模拟传感器说明（mappers/mock_sensor）

`mocksensor.New(deviceName, opts...)` 创建一个虚拟温湿度传感器 Mapper，用于在真实
设备接入前打通"云→边指令、边→云上报"完整链路。

**属性**：

| 属性 | 范围 | 行为 |
|------|------|------|
| `temperature` | [20, 35] | 每周期向 `targetTemp` 收敛（比例 0.3）+ 小随机扰动（±0.8） |
| `humidity` | [40, 70] | 随机游走（±0.8） |

**指令**：

| property | value | 行为 |
|----------|-------|------|
| `targetTemp` | [20, 35] | 设置目标温度，后续采集向新目标收敛；越界报错 |
| `reset` | — | 恢复出厂：温度/湿度重新随机，目标温度回默认 28 |

**选项**（函数式）：`WithNamespace` / `WithInterval`（默认 2s）/ `WithSeed`（测试确定性）/ `WithEventBus`（开启 MQTT 数据面，见 §8）。

**默认参数**：注册名 `mock-sensor`、命名空间 `default`、目标温度 28、波动周期 2s。

## 8. MQTT 数据面模式（WBS 3.6 集成，双通道语义）

> 对应提交：`feat(mapper): wire mock sensor to MQTT event bus data plane`
> 依赖：`edge/pkg/eventbus`（MQTT EventBus 封装）+ 边缘侧 MQTT broker（如 mosquitto）

### 8.1 何时用 MQTT 模式

MQTT 数据面把模拟传感器的“设备侧行为”真实化：遥测不再是内部状态，而是
真正**发布到 MQTT 总线**上可被任何订阅者（mosquitto_sub、真实设备网关、
其他边缘模块）消费的数据流；指令也可通过总线从“边缘本地操作者”下发。
适合：

- **M3 验收“MQTT 读写设备”**：mosquitto_sub 订阅遥测、mosquitto_pub 下发指令，
  直接验证边缘设备数据面；
- **真实 MQTT 设备接入前的协议演练**：把 MockSensor 当作“假装是 MQTT 设备的
  程序”，验证 broker/主题/QoS 约定后再接真设备；
- **多消费者场景**：遥测在总线上广播，除云端周期上报外，本地监控/告警模块
  可独立消费。

纯本地模式（默认）适用于不需要设备数据面的场景：状态只在内存中波动，
仅经云边通道（DeviceCommand/DeviceReport）与云端交互。

### 8.2 如何开启（装配层职责）

```go
bus := eventbus.New(eventbus.DefaultBrokerAddrFromEnv()) // 默认 tcp://127.0.0.1:1883
if err := bus.Connect(ctx); err != nil { /* 失败 → 降级为纯本地模式 */ }
reg := mapper.NewRegistry()
reg.Register(mocksensor.New("sensor-01", mocksensor.WithEventBus(bus)))
reg.StartAll(ctx)
// ... 优雅退出：先 reg.StopAll() 再 bus.Disconnect()
```

要点：

- **Connect 由装配层负责**（MockSensor 不建连）；`Start` 前总线须已连接，
  订阅才会成功；
- **降级决策**：EventBus 连接失败时 EdgeCore 记 Warn 并**不退出**，
  Mapper 以纯本地模式继续运行（云边设备链路不受影响）；降级是装配期决策，
  broker 之后才可用时 Mapper 不会自动切换，重启 edgecore 即恢复；
- 总线断线重连后的订阅恢复由 EventBus 自动完成（paho 重连 + 订阅表恢复），
  Mapper 无感知。

### 8.3 主题与负载格式

| 方向 | 主题 | 负载 | QoS |
|------|------|------|-----|
| 设备 → 边缘 | `devices/<namespace>/<deviceName>/telemetry` | `{"temperature":25.5,"humidity":60,"ts":1755168000000}` | 1 |
| 边缘 → 设备 | `devices/<namespace>/<deviceName>/command` | `{"property":"targetTemp","value":25}` / `{"property":"reset"}` | 1 |

- `ts` 为采集时刻的 Unix 毫秒时间戳；
- 指令 `property` 取值与云边通道一致：`targetTemp`（范围 [20,35]，越界忽略并告警）/ `reset`；
- 命名空间/设备名缺省为 `default`/注册时指定，主题由
  `eventbus.TelemetryTopic/CommandTopic` 构造（段值非法自动回退纯本地模式）。

### 8.4 双通道语义（重要）

MQTT 模式下传感器有**两个指令入口、一个遥测出口**：

```
云端（管理面）                                 边缘本地（数据面）
POST /device-command ──► EdgeHub ──► HandleCommand ─┐        ┌──► MQTT command 主题 ◄── mosquitto_pub
                                                     ├─► 同一份目标状态（targetTemp）
                                                     └──► 采集循环 ◄── 遥测发布到 MQTT telemetry 主题
```

| 通道 | 入口 | 可靠性 | 语义 |
|------|------|--------|------|
| 云边通道 | `HandleCommand`（DeviceCommand 消息） | 可靠投递 + Ack（失败云端可见） | 管理面：云端运维/应用下发 |
| MQTT 数据面 | `command` 主题订阅 | QoS 1（至少一次，本地） | 数据面：边缘本地操作者/设备网关下发 |

- 两通道都写**同一份目标温度**（`targetTemp`），后到者生效（last-write-wins）；
  不存在“通道优先级”，业务上按需选用；
- 遥测只有 MQTT 数据面一个出口（每次采样发布，QoS1）；云端看到的 DeviceReport
  由采集结果聚合后走云边通道，两者数值一致（同一份状态）；
- 总线断开时采集循环照常波动，发布跳过并告警，不 panic；恢复后自动续发。

### 8.5 验证方法（mosquitto）

```bash
mosquitto_sub -t 'devices/+/sensor-01/telemetry' -v   # 应看到温度数据流
mosquitto_pub -t devices/default/sensor-01/command -m '{"property":"targetTemp","value":25}'
# 对比发布前后 telemetry 温度：向 25 收敛
```

## 9. 验证

```bash
go build ./...
go vet ./edge/pkg/mapper/... ./mappers/... ./edge/pkg/eventbus/...
go test -race -cover ./edge/pkg/mapper/... ./mappers/... ./edge/pkg/eventbus/...
golangci-lint run ./edge/pkg/mapper/... ./mappers/... ./edge/pkg/eventbus/...
```

> MQTT 模式集成用例（`mappers/mock_sensor/mqtt_mode_test.go`）需要本机
> mosquitto（`brew install mosquitto`），缺省自动 `t.Skip`。

## 10. 下一步（缺口与风险）

- **MQTT 接入点**：`DeviceMapper` 的 `Start/Collect/HandleCommand` 即 MQTT 适配器
  的挂载点——`Start` 里订阅 topic，`Collect` 里读本地影子值，`HandleCommand` 里
  publish 写操作；框架无需改动；
- **真实设备协议适配**：Modbus/OPC-UA 只需新增 `DeviceMapper` 实现，注册名与
  设备名规划需与 DeviceTwin 的设备模型（属性字典）对齐；
- **双通道竞态**：云边通道与数据面同时写目标温度时按 last-write-wins 收敛，
  无跨通道互斥；若未来需要“管理面优先”，可在 HandleCommand 侧加通道优先级；
- **broker 依赖**：MQTT 模式强依赖边缘侧 broker 可用；broker 缺席时自动降级为
  纯本地模式（不退出），但**不会**在 broker 恢复后自动升级（装配期决策，
  重启 edgecore 恢复）；另注意 Connect 超时后 paho 仍在后台重连，日志出现
  “已连接”不代表 Mapper 处于 MQTT 模式；
- **多设备共享 Mapper**：`DeviceNames()` 已支持一台 Mapper 管多台设备，真实接入时
  需为每台设备维护独立状态（map[deviceName]state），MQTT 主题按设备名区分。

## 11. MQTT 设备 Mapper（v0.24.0，订阅型采集）

Modbus/OPC-UA 是**轮询型**：框架定时调 `Collect()` 主动读设备。MQTT Mapper
（`mappers/mqtt`，基于自研 `pkg/mqtt` 客户端，零第三方依赖）是**订阅型**：
设备主动把属性上报到 broker，Mapper 订阅数据主题合并进本地快照，
`Collect()` 只返回快照副本。两者共用同一 `DeviceMapper` 接口与装配路径
（`cmd/edgecore/device_mapper.go` 的 buildMapperRegistry）。

### 11.1 环境变量

| 环境变量 | 说明 | 默认值 |
| --- | --- | --- |
| `EDGEFLOW_MQTT_BROKER` | broker 地址 `host:port`，**非空即注册**（opt-in 开关） | 未设置（不注册） |
| `EDGEFLOW_MQTT_TOPICS` | 订阅 filter，逗号分隔（如 `demo/+/state,demo/#`） | `devices/+/state` |
| `EDGEFLOW_MQTT_DEVICE_NAME` | 设备名（注册表路由键） | `mqtt-device-01` |
| `EDGEFLOW_MQTT_NAMESPACE` | 设备命名空间 | `default` |
| `EDGEFLOW_MQTT_CMD_TOPIC` | 指令发布主题 | 首个 filter 去掉通配段拼前缀 + `/cmd`；无字面段回退 `edgeflow/mqtt/cmd` |

### 11.2 订阅/命令主题模型

- **数据（设备 → 边缘）**：QoS 0 订阅全部 filter。payload 容错解析：JSON
  对象的顶层可转数值字段，或 `k=v` 文本（逗号/空白分隔）；解析失败整条跳过
  （记台账 error，快照不变）。
- **指令（边缘 → 设备）**：`HandleCommand` 把 `DeviceCommand`（与云边契约
  同 JSON 字段：`deviceName/namespace/property/value`）发布到 cmd 主题
  （QoS 0）后返回当前快照；设备执行后把新状态上报数据主题即完成闭环
  （订阅型无同步回读）。
- **断连监管**：`pkg/mqtt` Client 无自动重连——Mapper 监管循环每 2s 向
  `edgeflow/mqtt/health` 发布空消息探测存活，失败即重 Dial + 重订阅
  （2s 间隔，context 取消即退）。不用 Subscribe 探测：client 对同 filter
  是 append 语义，反复重订会使 handler 无限累积。

### 11.3 与 modbus/opcua 的差异

| 维度 | modbus/opcua（轮询型） | mqtt（订阅型） |
| --- | --- | --- |
| 采集 | `Collect()` 现场读写 | 设备推送 → 快照，`Collect()` 读缓存 |
| 断连 | 操作时按需重连 | 监管 goroutine 主动探测 + 自动重连 |
| 指令 | 直写寄存器/节点 + 回读验证 | 发布到 cmd 主题，设备异步执行 |

### 11.4 最小示例

```bash
# 设备侧：向数据主题上报 JSON 属性（k=v 文本如 "temperature=25.5" 亦可）
mosquitto_pub -h 127.0.0.1 -t demo/sensor-01/state -m '{"temperature":25.5,"humidity":60}'

# edgecore 侧装配（broker 非空即注册）
EDGEFLOW_MQTT_BROKER=127.0.0.1:1883 \
EDGEFLOW_MQTT_TOPICS=demo/+/state \
EDGEFLOW_MQTT_CMD_TOPIC=demo/cmd \
./edgecore

# 云端下发指令 → Mapper 发布 {"deviceName":"mqtt-device-01","property":"setpoint","value":42} 到 demo/cmd
curl -X POST http://<cloud>/api/v1/nodes/<node>/device-command \
  -d '{"deviceName":"mqtt-device-01","namespace":"default","property":"setpoint","value":42}'
```
