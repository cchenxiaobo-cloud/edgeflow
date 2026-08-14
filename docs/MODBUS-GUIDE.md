# Modbus Mapper 使用指南（WBS 5.2）

> 日期：2026-08-14 ｜ 范围：`mappers/modbus/`（Mapper）+ `pkg/modbussim/`（模拟器库）+ `hack/modbus-sim/`（模拟器入口）+ `hack/modbus-e2e/`（端到端验证）+ `edge/pkg/metamanager/ledger.go`（操作台账）
> 对应提交：`feat(modbus): modbus tcp mapper with simulator and operation ledger (WBS 5.2)`

## 1. 概述

Modbus 是工业设备（PLC、温控器、传感器）最通用的通信协议之一。本模块提供：

- **Modbus TCP Mapper**（`mappers/modbus`）：实现 `DeviceMapper` 接口，把一台
  Modbus TCP 设备接入 EdgeFlow 设备链路——云端 `DeviceCommand` 下发的
  `targetTemp` / `coil0..coil3` 指令写入设备寄存器/线圈，`Collect` 读取
  温度/湿度上报云端；
- **高保真模拟器**（`hack/modbus-sim` + `pkg/modbussim`）：自实现 Modbus TCP
  协议帧（MBAP + PDU），无需真实设备即可联调；与 goburrow 客户端交叉验证
  帧格式；
- **操作台账**（`edge/pkg/metamanager/ledger.go`）：所有上报/下发操作落
  SQLite（`op_ledger` 表），保留 30 天，可按设备/方向/时间范围查询——验收
  「双向读写验证 + 台账可查」的落地。

```
        cloud (DeviceTwin)
            │ TypeDeviceCommand（下发 targetTemp/coil0..3）  TypeDeviceReport（上报）
            ▼
       EdgeHub ──► MapperRegistry ──路由 mb-sensor-01──► ModbusMapper
                                                              │ goburrow/modbus (TCP:502/15020)
                                                              ▼
                                                    Modbus TCP 设备 / 模拟器
                                                              │
                                                              ▼
                                                 台账 op_ledger（SQLite，30 天）
```

## 2. 快速开始（5 分钟联调）

```bash
# 1) 起模拟器（默认 :15020，env MODBUS_SIM_PORT 可改）
go run ./hack/modbus-sim

# 2) 端到端验证：模拟器 → Mapper 读写 → 台账查询（全自动）
go run ./hack/modbus-e2e

# 3) 直接查台账（SQLite 持久化，重启不丢）
sqlite3 data/modbus-e2e.db \
  "SELECT id,datetime(ts/1000,'unixepoch','+8 hours'),device_id,direction,reg_addr,value,result \
   FROM op_ledger ORDER BY id;"
```

edgecore 集成：设置环境变量后启动即可（见 §5）：

```bash
EDGEFLOW_MODBUS_ADDR=127.0.0.1:15020 ./bin/edgecore
```

## 3. 模拟器（hack/modbus-sim）

### 3.1 设备模型与地址映射表

模拟 1 台 Modbus TCP 设备（unit ID 1），寄存器/线圈地址是 Mapper 与真实
设备对接的**事实标准**，接真实设备时按 §6 替换即可：

| 地址      | 类型     | 名称     | 读写 | 说明                                   |
|-----------|----------|----------|------|----------------------------------------|
| `0x0000`  | 保持寄存器 | 温度   | 只读 | 原始值 ÷10 = °C（250 → 25.0°C）        |
| `0x0001`  | 保持寄存器 | 湿度   | 只读 | 原始值 ÷10 = %RH（679 → 67.9%）        |
| `0x0010`  | 保持寄存器 | 目标温度 | 可写 | 值域 [0,1000]（0~100°C），温度向其收敛 |
| `0x0020`  | 线圈       | 线圈0  | 可写 | 0xFF00=ON / 0x0000=OFF                 |
| `0x0021`  | 线圈       | 线圈1  | 可写 | 同上                                   |
| `0x0022`  | 线圈       | 线圈2  | 可写 | 同上                                   |
| `0x0023`  | 线圈       | 线圈3  | 可写 | 同上                                   |

### 3.2 动态行为（仿 mock_sensor）

温度按比例向目标温度收敛并叠加小随机扰动（0.5°C/周期），湿度随机游走
（0.8%/周期），周期 500ms；寄存器表由波动 goroutine 持续刷新——客户端
轮询即可观察到「写目标温度 → 温度逐渐逼近」的完整闭环。

### 3.3 支持的功能码与异常应答

- 功能码：`0x01` 读线圈、`0x03` 读保持寄存器、`0x05` 写单线圈、
  `0x06` 写单寄存器、`0x10` 写多寄存器；
- 异常应答（功能码|0x80 + 错误码）：
  - `0x01` 非法功能码（如 0x02/0x04/0x07/0x0F）；
  - `0x02` 非法数据地址（越界 / 写只读寄存器）；
  - `0x03` 非法数据值（线圈值非 0xFF00/0x0000、目标温度越界、数量为 0）。

### 3.4 环境变量

| 变量            | 默认值 | 说明                 |
|-----------------|--------|----------------------|
| `MODBUS_SIM_PORT` | 15020 | 监听端口             |

## 4. Mapper（mappers/modbus）

### 4.1 属性映射

| DeviceCommand.property | 目标       | 值语义                              |
|------------------------|------------|-------------------------------------|
| `targetTemp`           | 寄存器 `0x0010` | 物理值 °C，[0,100]；×10 后写入，写后回读验证 |
| `coil0`..`coil3`       | 线圈 `0x0020`..`0x0023` | 非 0 = ON（0xFF00），0 = OFF；写后回读验证 |

`Collect()` 一次读 `0x0000-0x0001` 两个保持寄存器，返回
`{"temperature": 25.5, "humidity": 60.0}`（物理值）。

### 4.2 连接管理

- TCP 长连接，地址 env `EDGEFLOW_MODBUS_ADDR`（默认 `127.0.0.1:15020`），
  单次操作超时 5s（`WithTimeout` 可调）；
- **每次操作前确保连接**（goburrow `Connect` 幂等，未连接才拨号）；
- **断线重连**：传输层错误（设备重启/断网）→ 断开旧连接 → 重连 → 重试一次；
  重连失败返回明确错误（含设备地址与原始错误）；
- Modbus 异常应答（如非法地址）是设备已应答的业务错误，不重试，直接返回；
- 设备晚于 edgecore 启动也没关系：`Start` 预连接失败只告警，操作时按需重连。

### 4.3 依赖说明（唯一新增依赖）

`github.com/goburrow/modbus`（v0.1.0，社区标准 Modbus 客户端）：

- 功能码/异常码/事务一致性校验完整，无需自研协议栈；
- 纯 Go 无 CGO，与项目交叉编译约束一致（linux/amd64、arm64）；
- 模拟器（`pkg/modbussim`）为自实现协议帧，客户端库与服务端自实现
  交叉验证帧格式——两端独立实现，格式错误必然暴露。

## 5. edgecore 装配与台账

### 5.1 装配（cmd/edgecore）

设置 `EDGEFLOW_MODBUS_ADDR` 后 edgecore 启动即注册 Modbus Mapper
（显式 opt-in：没有 Modbus 设备的部署不会出现无谓的连接报错）：

```go
// cmd/edgecore/device_mapper.go（buildMapperRegistry）
if addr := os.Getenv(modbusmapper.EnvAddr); addr != "" {
    reg.Register(modbusmapper.New(addr, modbusmapper.WithLedger(ledger)))
}
```

指令链路无需额外配置：云端下发 `DeviceCommand{deviceName:"mb-sensor-01",
property:"targetTemp", value:25.5}` 即按设备名路由到本 Mapper。

### 5.2 操作台账（验收核心）

**存储选型：独立 SQL 表 `op_ledger`（而非复用 meta_kv KV 表）**，理由：

1. **查询模式**：台账按 `device_id`/`direction`/`ts` 组合过滤 + 排序 + LIMIT，
   SQL 的 WHERE/索引一次完成；KV 前缀扫描需全量拉取后在内存过滤；
2. **清理模式**：保留期删除是范围删除（`DELETE WHERE ts < cutoff`）一条语句；
   KV 需先 List 再逐条 Delete；
3. **语义差异**：meta_kv 是「覆盖写」的当前态存储，台账是「追加写」的历史
   流水，同操作绝不覆盖。

记录字段（`OpRecord`）：`id`（自增）、`ts`（毫秒）、`device_id`、`direction`
（`up`=上报 / `down`=下发）、`reg_addr`（如 `0x0010`、`coil:0x0020`）、
`value`、`result`（ok/error）、`message`（错误详情/回读验证说明）。

**生命周期**：`NewLedger` 建表 + 启动即清一次；edgecore 内每 24h 定期清理
（`RunCleanupLoop`）；保留期 30 天（`LedgerRetentionDays`）。

**查询方式**（满足验收「可按条件查出」）：

```bash
# 全部（时间倒序，最多 200 条）
sqlite3 data/edgeflow.db "SELECT * FROM op_ledger ORDER BY id DESC LIMIT 200;"
# 按设备 + 方向 + 时间范围组合查询
sqlite3 data/edgeflow.db \
  "SELECT id,datetime(ts/1000,'unixepoch','+8 hours'),reg_addr,value,result \
   FROM op_ledger \
   WHERE device_id='mb-sensor-01' AND direction='down' \
     AND ts >= strftime('%s','now','-7 day')*1000 \
   ORDER BY id;"
```

Go 侧等价接口（`edge/pkg/metamanager`）：

```go
ledger, _ := metamanager.NewLedger(store)          // 建表 + 启动清理
ledger.SaveOp(metamanager.OpRecord{...})           // 追加记录
ops, _ := ledger.ListOps(metamanager.OpFilter{     // 条件查询
    DeviceID: "mb-sensor-01", Direction: "down",
    StartTs: start, EndTs: end, Limit: 200,
})
n, _ := ledger.CleanupOps(30 * 24 * time.Hour)     // 清理过期记录
```

## 6. 接入真实设备

1. **地址映射**：把 §3.1 的地址表替换为真实设备的寄存器表（查阅设备手册，
   注意：不同厂商地址起始偏移可能是 0 或 1，如手册写 40001 实际请求地址
   是 0x0000——以「协议地址」为准）；
2. **缩放系数**：温度寄存器可能是 0.1 精度（×10）或 0.01（×100），
   修改 `scaleFactor` 或按设备校准；
3. **只读保护**：模拟器中 0x0000/0x0001 写保护；真实设备写只读寄存器会回
   异常码 0x02，Mapper 会以明确错误返回（可查台账 result=error）；
4. **从站地址**：网关后多台设备用 `WithSlaveID(id)` 区分（默认 1）；
5. **RTU 串口设备**：本实现仅 TCP。RTU 设备需接 Modbus 网关（TCP 侧）或
   换用 `goburrow` 的 RTU handler（本项目暂未做，见 §8 风险）。

## 7. 测试与验证

```bash
# 模拟器协议测试（裸 TCP 帧：读/写/三类异常码）
go test ./pkg/modbussim/ -v

# Mapper 集成测试（起模拟器 → Collect 读 → HandleCommand 写 → 回读验证 → 台账断言）
go test ./mappers/modbus/ -v

# 台账测试（按条件查询、30 天清理、重启持久化）
go test ./edge/pkg/metamanager/ -run 'TestSaveOp|TestListOps|TestCleanupOps|TestLedger' -v

# 端到端验收（真实执行：模拟器 15020 → 读写 → sqlite3 查台账）
go run ./hack/modbus-e2e
```

## 8. 排障 FAQ

| 现象 | 原因 | 处理 |
|------|------|------|
| Collect 报「连接失败: dial tcp ... 拒绝」 | 设备/模拟器未启动 | `go run ./hack/modbus-sim` 起模拟器；确认 `EDGEFLOW_MODBUS_ADDR` |
| 写指令报「重试后仍失败」 | 设备在线但网络抖动，重连重试后仍失败 | 查设备侧日志；确认防火墙放行 TCP 端口；台账查 result=error 记录定位 |
| 台账查不到记录 | 未装配 Ledger（`WithLedger(nil)` 或 NewLedger 失败） | edgecore 日志查「操作台账初始化失败」；单测/工具里确保传入 ledger |
| 写目标温度成功但温度不变 | 写错地址（真实设备寄存器偏移不同） | 按 §6 核对地址表；用模拟器对照：写 0x0010 后温度应向目标收敛 |
| Modbus 异常码 0x02/0x03 | 地址越界 / 值非法 | 核对地址映射与值域；Mapper 的错误信息含异常码，台账可回溯 |
| 台账增长过快 | 采集周期短、设备多 | `op_ledger` 是追加流水；30 天自动清理，也可调小 `Limit` 查询 |
