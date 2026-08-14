# EventBus 设备通信总线指南（WBS 3.6）

> 日期：2026-08-14 ｜ 范围：`edge/pkg/eventbus/`（MQTT EventBus 封装）
> 对应提交：`feat(eventbus): MQTT event bus with reconnect (WBS 3.6)`
> 依赖：mosquitto broker（`brew install mosquitto`，**仅测试需要**；生产可替换任一 MQTT broker）
> 新依赖：`github.com/eclipse/paho.mqtt.golang`（本包唯一新增）

## 1. 概述与定位

EventBus 是**边缘内部设备通信总线**，对标 KubeEdge 的 EventBus：MQTT broker 是
边缘侧的**设备接入点**，真实设备（MQTT 传感器 / Modbus 网关 / 其他 IoT 设备）通过
MQTT 接入边缘，Mapper 通过 EventBus 与设备通信，不再直接持有设备连接。

```
                         ┌────────────── 边缘节点（EdgeCore）──────────────┐
                         │                                                │
  云端 CloudCore          │   ┌──────────┐    ┌───────────┐                │
 ┌────────────┐          │   │  EdgeHub │    │ DeviceTwin│                │
 │ CloudHub   │◄─WS 管理面►│   │ (WBS 3.1)│    │ (WBS 3.5) │                │
 └────────────┘          │   └────┬─────┘    └─────┬─────┘                │
   DeviceCommand/        │        │               │                       │
   DeviceReport          │        │  DeviceCommand/DeviceReport             │
   （云边契约消息）        │        ▼               ▼                       │
                         │   ┌─────────────────────────┐                   │
                         │   │     MapperRegistry       │                   │
                         │   │   （DeviceMapper 路由）    │                   │
                         │   └────────────┬────────────┘                   │
                         │                │  MQTT 客户端                    │
                         │                ▼                                │
                         │   ┌─────────────────────────┐                   │
                         │   │  EventBus (edge/pkg/     │                   │
                         │   │   eventbus, WBS 3.6)     │                   │
                         │   └────────────┬────────────┘                   │
                         │                │ MQTT（本机 1883）                │
                         └────────────────┼────────────────────────────────┘
                                          ▼
                               ┌───────────────────┐
                               │  MQTT broker       │  ← 边缘设备接入点
                               │  （mosquitto）      │
                               └──┬───────┬───────┬─┘
                                  │       │       │
                             遥测/指令   遥测/指令  模块间事件（预留）
                                  ▼       ▼       ▼
                            ┌────────┐ ┌────────┐ ┌────────┐
                            │MQTT 设备│ │MQTT 设备│ │ 内部模块│
                            └────────┘ └────────┘ └────────┘
```

### 与云边 WebSocket 通道的分工

| 维度 | WebSocket（edgehub，WBS 3.1） | MQTT EventBus（本包） |
|---|---|---|
| 角色 | **云边管理面** | **边缘设备数据面** |
| 对端 | 云端 CloudHub（跨节点） | 边缘本机 broker（不出边缘） |
| 消息 | Register/Heartbeat/DeviceCommand/DeviceReport（云边契约） | 设备遥测/指令、模块间事件 |
| 可靠性 | 应用层 Ack（命令可靠投递） | QoS 1（至少一次） |
| 断线 | 指数退避重连 + 注册恢复 | paho 自动重连 + 订阅自动恢复 |

**边界**：`DeviceCommand`/`DeviceReport` 等设备契约消息**仍走 WebSocket 云边通道**
（云端要看到的是边缘上报的聚合结果，不是设备原始字节流）；EventBus 只负责
"设备 ↔ Mapper"和"边缘内部模块 ↔ 模块"的本地消息。设备遥测由 Mapper 采集后
聚合为 DeviceReport 经 EdgeHub 上报云端（见 §5 改造示例）。

## 2. API 速览

```go
bus := eventbus.New("tcp://127.0.0.1:1883", eventbus.WithClientID("edgecore-01"))
if err := bus.Connect(ctx); err != nil { /* 连接失败或 ctx 取消 */ }
defer bus.Disconnect()

// 发布（QoS 1，至少一次投递）
if err := bus.Publish(topic, []byte(`{"temperature":25.5}`)); err != nil { ... }

// 订阅（QoS 1；断线重连后自动恢复订阅，无需重订）
if err := bus.Subscribe(topic, func(topic string, payload []byte) { ... }); err != nil { ... }

if err := bus.Unsubscribe(topic); err != nil { ... }

bus.IsConnected() // 可用性：在线，或断线但自动重连进行中
bus.IsOnline()    // 当前这一刻是否有活连接
```

- 构造函数：`New(brokerAddr string, opts ...Option)`，`brokerAddr` 传空串时
  依次取环境变量 `EDGEFLOW_EDGECORE_MQTT_ADDR` → 默认 `tcp://127.0.0.1:1883`；
- 可选配置：`WithClientID` / `WithCredentials` / `WithConnectTimeout` /
  `WithKeepAlive` / `WithMaxReconnectInterval`；
- 全部方法线程安全；`Publish`/`Subscribe` 需在 `Connect` 成功后调用（未连接返回错误）。

## 3. 主题约定

| 主题 | 方向 | 用途 |
|---|---|---|
| `devices/<namespace>/<deviceName>/telemetry` | 设备 → 边缘 | 设备遥测数据上报 |
| `devices/<namespace>/<deviceName>/command` | 边缘 → 设备 | 边缘向设备下发指令 |
| `edgeflow/<module>/<event>` | 模块 → 模块 | 边缘内部模块间事件（预留） |

主题用构造函数生成，自动校验段值（不允许 `/`、`+`、`#`，防主题注入）：

```go
telemetryTopic, _ := eventbus.TelemetryTopic("default", "sensor-01") // devices/default/sensor-01/telemetry
commandTopic, _   := eventbus.CommandTopic("default", "sensor-01")   // devices/default/sensor-01/command
eventTopic, _     := eventbus.EventTopic("metamanager", "pod-updated") // edgeflow/metamanager/pod-updated
```

**载荷格式**：设备遥测/指令的 payload 为 JSON（字段与 `DeviceCommand`/`DeviceReport`
云边契约保持一致，便于 Mapper 直接复用结构体序列化）。

## 4. 可靠性语义

### QoS 1（至少一次投递）

- 发布与订阅默认 QoS 1：消息**至少到达一次**，broker 与客户端之间有确认与重传；
- **不保证去重**：网络抖动时同一消息可能投递多次，**消费方必须幂等**
  （设备指令按 `seq`/时间戳去重，遥测是状态快照天然幂等）；
- QoS 2（恰好一次）不在当前范围：边缘本地总线延迟低、抖动小，QoS 1 + 幂等
  是成本与可靠性的平衡点，与 KubeEdge EventBus 默认一致。

### 重连行为

- **自动重连**：`SetAutoReconnect(true)`，断线后 paho 后台指数退避重试
  （首次立即、随后 1s 起翻倍，上限 `WithMaxReconnectInterval`，默认 10s）；
- **首次建连重试**：`SetConnectRetry(true)`，broker 晚于 EdgeCore 启动也能恢复
  （重试间隔默认 1s）；`Connect(ctx)` 会阻塞到连接成功或 ctx 取消；
- **订阅自动恢复**：`CleanSession=true` 不依赖 broker 会话持久化——断线期间
  broker 侧的订阅会丢失，本包在每次（重）连接成功回调里**自动重新订阅全部
  已注册主题**，调用方无需感知；
- **状态回调**：连接建立/断线/重连均有日志（`eventbus: 已连接/连接断开/正在重连`），
  业务侧可用 `IsOnline()` 感知瞬态离线（如设备指令期间离线可延迟重发）。

```
时间轴示例：
  [连接] ──► 正常收发 ──► broker 宕机 ──► 断线回调(IsOnline=false)
                                          │  paho 后台重连（1s、2s、4s…）
                     broker 恢复 ◄────────┘
                          │
                   重连成功(IsOnline=true) → 自动恢复订阅 → 继续收发
```

## 5. 把 Mapper 接到 EventBus（mock_sensor 改造示例）

当前 `mappers/mock_sensor` 是"直接采集"的内存模拟：`Collect()` 返回内部状态。
接入 EventBus 后，模拟设备应该"挂在总线上"：**Mapper 订阅设备的 command 主题
收指令、设备往 telemetry 主题发数据**（真实 MQTT 设备接入同理，只是把
"内存模拟设备"换成真设备固件）。

> 本轮（WBS 3.6）只交付 EventBus 包本身；以下为下一轮 Mapper 接入的改造步骤
> 示例，**不在本轮提交中执行**。

### 步骤 1：MockSensor 增加 EventBus 接入（模拟设备"上线"）

```go
// mappers/mock_sensor/mock_sensor.go（示意，非本轮提交）
func (m *MockSensor) Start(ctx context.Context) error {
    // ...原有采集循环...
    // 新增：模拟设备接入 EventBus，向 broker 发布遥测
    go func() {
        ticker := time.NewTicker(m.interval)
        defer ticker.Stop()
        for {
            select {
            case <-ctx.Done():
                return
            case <-ticker.C:
                topic, _ := eventbus.TelemetryTopic(m.namespace, m.deviceName)
                payload, _ := json.Marshal(m.snapshot())
                if err := m.bus.Publish(topic, payload); err != nil {
                    log.Warnf("mock_sensor: 发布遥测失败: %v", err)
                }
            }
        }
    }()
    return nil
}
```

### 步骤 2：Mapper 订阅设备指令主题（边缘 → 设备）

```go
// MockSensor.Start 内（示意）
commandTopic, _ := eventbus.CommandTopic(m.namespace, m.deviceName)
if err := m.bus.Subscribe(commandTopic, func(topic string, payload []byte) {
    var cmd mapper.DeviceCommand
    if err := json.Unmarshal(payload, &cmd); err != nil {
        log.Warnf("mock_sensor: 指令解析失败: %v", err)
        return
    }
    m.applyCommand(cmd) // 更新 targetTemp 等（幂等）
}); err != nil {
    return fmt.Errorf("订阅指令主题失败: %w", err)
}
```

### 步骤 3：DeviceTwin → EventBus 的指令下发（替代直连 Mapper）

```go
// edge/pkg/devicetwin（示意）：指令下发改走总线，按设备主题发布
topic, _ := eventbus.CommandTopic(cmd.Namespace, cmd.DeviceName)
payload, _ := json.Marshal(cmd)
if err := bus.Publish(topic, payload); err != nil {
    return DeviceReport{}, fmt.Errorf("下发指令失败: %w", err)
}
```

### 改造后的数据流

```
云端 DeviceCommand ──WS──► DeviceTwin ──Publish(command topic)──► broker
                                                                  │
        Mapper（订阅 command / 发布 telemetry）◄──────────────────┘
                                                                  │
云端 DeviceReport ◄──WS── EdgeHub ◄──DeviceTwin 聚合 ◄──Collect()◄─┘
```

> 兼容性：改造期间 `DeviceMapper.Collect()/HandleCommand()` 接口保持不变，
> MapperRegistry 路由逻辑不动；接入方式（直连采集 vs 总线通信）对上层透明。

## 6. 测试与验证

### 依赖

- 集成测试需要本机 mosquitto：`brew install mosquitto`；
- macOS brew 的 broker 二进制在 `/opt/homebrew/sbin/mosquitto`（通常不在 PATH），
  测试的 `findMosquitto()` 会探测 PATH 与常见安装路径，找不到则自动 `t.Skip`；
- 测试**不启动系统服务**：每个用例自行起临时 mosquitto（随机空闲端口），
  结束后杀掉进程，不碰 brew services。

### 运行

```bash
go test -race -cover ./edge/pkg/eventbus/...
```

覆盖场景：

| 用例 | 验证点 |
|---|---|
| `TestPublishSubscribeRoundTrip` | 双客户端互发、topic/payload 一致、Unsubscribe 生效 |
| `TestQoS1Delivery` | QoS 1 消息 10/10 全部到达 |
| `TestReconnectAutoRestore` | 停 broker → 断线感知 → 重启 → 自动重连 → **订阅自动恢复**、收发恢复 |
| `TestConnectWaitsForBroker` | broker 晚启动时 Connect 阻塞等待并最终成功 |
| `TestConnectFailsWithoutBroker` | 无 broker + ctx 超时 → Connect 返回错误 |
| `TestConcurrentPublishSubscribe` | 并发收发无竞态（配合 -race） |
| `TestTopicBuilders` 等 | 主题构造与非法段校验、环境变量默认值（纯单测） |

## 7. 后续工作（不在本轮）

1. **edgecore 装配**：`cmd/edgecore/main.go` 创建 EventBus 单例（地址取
   `EDGEFLOW_EDGECORE_MQTT_ADDR`），随 EdgeCore 启停（下一轮）；
2. **Mapper 接入**：按 §5 把 mock_sensor 与真实设备 Mapper 挂到总线；
3. **broker 生命周期**：EdgeCore 侧管理 mosquitto 进程（systemd/容器内嵌），
   或支持连接外部 broker；
4. **安全**：broker 开启认证时用 `WithCredentials`；TLS（mqtts://）按需扩展。
