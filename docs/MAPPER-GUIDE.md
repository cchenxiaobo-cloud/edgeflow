# Mapper 框架指南（WBS 5.1 / 5.5）

> 日期：2026-08-14 ｜ 范围：`edge/pkg/mapper/`（框架）+ `mappers/mock_sensor/`（示例模拟设备）
> 对应提交：`feat(mapper): device mapper framework and mock sensor (WBS 5.1/5.5)`
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
`DeviceNames() []string` 声明自己管理的设备名，注册表据此建立
`deviceName → Mapper` 路由索引；未实现则退化为"注册名即设备名"。

## 4. MapperRegistry 注册表

```go
r := mapper.NewRegistry()
r.Register(m)                    // 注册（重复名/设备名冲突报错，冲突时整体回滚）
m, ok := r.Get(name)             // 按注册名查询
m, ok := r.Route(deviceName)     // 按设备名路由（优先设备名索引，回退注册名）
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

**选项**（函数式）：`WithNamespace` / `WithInterval`（默认 2s）/ `WithSeed`（测试确定性）。

**默认参数**：注册名 `mock-sensor`、命名空间 `default`、目标温度 28、波动周期 2s。

## 8. 验证

```bash
go build ./...
go vet ./edge/pkg/mapper/... ./mappers/...
go test -race -cover ./edge/pkg/mapper/... ./mappers/...
golangci-lint run ./edge/pkg/mapper/... ./mappers/...
```

## 9. 下一步（缺口与风险）

- **MQTT 接入点**：`DeviceMapper` 的 `Start/Collect/HandleCommand` 即 MQTT 适配器
  的挂载点——`Start` 里订阅 topic，`Collect` 里读本地影子值，`HandleCommand` 里
  publish 写操作；框架无需改动；
- **真实设备协议适配**：Modbus/OPC-UA 只需新增 `DeviceMapper` 实现，注册名与
  设备名规划需与 DeviceTwin 的设备模型（属性字典）对齐；
- **EdgeCore 装配**：main.go 注册/启动/停止注册表 + DeviceTwin 指令分发与周期上报
  （下一轮集成，接口已就绪）；
- **多设备共享 Mapper**：`DeviceNames()` 已支持一台 Mapper 管多台设备，真实接入时
  需为每台设备维护独立状态（map[deviceName]state）。
