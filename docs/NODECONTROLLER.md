# NodeController — 云端节点心跳超时管理（WBS 2.4）

NodeController 是 cloudcore 内的一个后台扫描组件（`cloud/pkg/nodecontroller/`），
职责是**心跳静默超时判定**：定时扫描节点注册表，把心跳停滞超过阈值的节点
标记为 `Offline`。对标 KubeEdge 的 NodeController（云端统一判定节点离线）。

## 1. 设计意图：与 CloudHub 断开事件互补

节点离线有两条独立判定路径，各覆盖一类场景：

| 路径 | 触发条件 | 覆盖场景 | 时延 |
|---|---|---|---|
| CloudHub 断开事件 | 连接 90s 无任何消息 → 断开 → `OnNodeDisconnected` → `MarkOffline` | 连接断了（断网、进程崩溃、防火墙断连） | ~90s |
| NodeController 扫描 | `LastHeartbeatAt` 距今超过 timeout（默认 180s） | **连接还活着但心跳停滞**（应用卡死/心跳 goroutine 异常）、**连接断开但事件未触发**（事件丢失/回调异常） | timeout + 最多一个扫描周期 |

两者是**互补而非重复**的关系：

- 常规场景（连接断开）由 CloudHub 事件先判 Offline（90s < 180s），
  NodeController 不参与、不重复打日志（已 Offline 的节点直接跳过）。
- 异常场景（心跳停滞但连接存活、断开事件丢失）由 NodeController 兜底，
  保证"节点状态最终收敛到 Offline"，不会出现"连接断了但节点永远 Ready"的悬挂状态。

## 2. 配置项

环境变量（cloudcore 启动时读取，装配期校验，非法值直接报错退出）：

| 环境变量 | 默认值 | 说明 | 示例 |
|---|---|---|---|
| `EDGEFLOW_CLOUDCORE_NODE_SCAN_INTERVAL` | `30s` | 扫描周期。值支持 Go duration（`15s`、`1m30s`）或纯秒数（`15`） | `10s` |
| `EDGEFLOW_CLOUDCORE_NODE_TIMEOUT` | `180s` | 心跳超时阈值：`LastHeartbeatAt` 距今超过该值即判 Offline | `3m`、`180` |

取值建议：

- timeout 应**大于** CloudHub 连接失活阈值（90s），否则慢网络下
  "连接还活着、心跳略慢"的节点会被控制器先判 Offline（误伤）。
- 默认 180s ≈ 6 个心跳周期（边侧心跳 30s），留了足够余量。
- scan interval 建议 ≤ timeout/3，保证感知时延不超过 timeout + 一个周期。

## 3. 状态机（复用 registry 现有逻辑）

```
                注册成功 / 心跳更新
                    │
                    ▼
  ┌─────────┐   UpdateHeartbeat   ┌─────────┐
  │  Ready  │ ◄────────────────── │ Offline │
  └─────────┘                     └─────────┘
       │                              ▲
       │ LastHeartbeatAt 超时          │ MarkOffline（断开事件 / 扫描兜底）
       └──────────────────────────────┘
```

- `Ready` → `Offline`：CloudHub 断开事件，或 NodeController 扫描发现心跳停滞。
- `Offline` → `Ready`：节点重新心跳（`UpdateHeartbeat`）自动恢复，
  无需人工干预——边缘重启后注册+心跳即回到 Ready。
- 判定单位：`LastHeartbeatAt` 是**毫秒时间戳**，控制器换算为 `time.Time`
  后与当前时间比较，避免单位混淆；`LastHeartbeatAt = 0`（从未心跳）的
  节点按"未知"跳过，不误判。

## 4. 运行与验证

```bash
# 默认参数启动
./cloudcore

# 缩短参数（联调/测试用）：5s 扫描、15s 超时
EDGEFLOW_CLOUDCORE_NODE_SCAN_INTERVAL=5s \
EDGEFLOW_CLOUDCORE_NODE_TIMEOUT=15s \
./cloudcore
```

启动日志：

```
[INFO] [NodeController] 心跳超时扫描已启动（interval=30s, timeout=3m0s）
[INFO] NodeController 装配完成: 扫描周期 30s, 心跳超时 3m0s
```

标记 Offline 日志（WARN 级别，每节点每次状态转移只打一次）：

```
[WARN] [NodeController] 节点 node-1 心跳超时（lastHeartbeat=2026-08-14T18:20:00+08:00, timeout=3m0s），标记 Offline
```

验证 API（与 registry 查询接口一致）：

```bash
curl -s http://127.0.0.1:8080/api/v1/nodes | jq .
# 心跳正常 → "status": "Ready"
# 停滞超时 → "status": "Offline"
# 重启 edgecore → 自动恢复 "status": "Ready"
```

## 5. 异常排障

| 现象 | 可能原因 | 排查 |
|---|---|---|
| 节点一直 Ready，不判 Offline | timeout 配置过大；边侧实际在心跳（连接健康） | 看 `lastHeartbeatAt` 是否在增长；对比 timeout 与心跳周期 |
| 节点被误判 Offline | timeout 小于 CloudHub 90s 连接阈值，慢网络心跳偶尔超时 | 调大 timeout（建议 ≥180s），观察是否复现 |
| 日志出现重复 Offline 告警 | 正常不会：已 Offline 节点被跳过 | 若反复出现，检查是否有代码绕过 registry 直接改状态 |
| 启动报"NodeController 配置无效" | 环境变量非法（非 duration、非正数） | 用 `15s`/`180` 格式，参考第 2 节 |
| 边侧恢复心跳但状态仍 Offline | 不应该：`UpdateHeartbeat` 无条件回 Ready | 检查 edgecore 是否真的在发心跳（`heartbeatInterval` 30s） |
| 心跳停滞但连接不断（异常场景） | 边侧心跳 goroutine 卡死 | NodeController 正是为此兜底，等待 timeout + 一个扫描周期 |

## 6. 边界与已知限制

- **单实例扫描**：NodeController 是 cloudcore 进程内组件，单实例部署；
  未来多副本部署时需要选主（或迁移到 apiserver 的 Node 控制器语义）。
- **与断开事件竞态**：扫描与 CloudHub 事件回调并行，理论上有
  "扫描判 Offline → 同时断开事件也 MarkOffline"的重复标记（幂等，无害）；
  "心跳刚到 → 扫描已判 Offline"的窗口极窄（timeout 180s vs 心跳 30s），
  下一跳心跳即恢复 Ready，无需处理。
- 本组件只做状态标记，不触发驱逐/告警外发；将来可在 `scanOnce` 中
  挂接通知（如 Feishu 告警、Pod 迁移）。
