# EdgeFlow M2 第二轮代码审查报告（CODE-REVIEW-M2B）

- 审查对象：PodStatus 上报链路（云端存储+API、边缘上报循环、P2-1 status 清理）
- 审查人：M2B 复核员（资深 Go 工程师视角，独立复核）
- 提交：707128f（cloud pkg/podstatus + CloudHub 回调 + cloudcore API）、bc58e40（edge status_report.go + edgecore 上报循环）、d90a495（gofmt）
- 日期：2026-08-14
- 审查结论：**有条件通过**（无 P0；1 项 P1 建议尽快修复；其余 P2）

## 0. 审查范围与方法

- 范围：cloud/pkg/podstatus、cloud/pkg/cloudhub（server.go 的 PodStatus 处理）、cmd/cloudcore/main.go（API+装配）、edge/pkg/edged/status_report.go + edged.go（cleanupStatus 调用点）、cmd/edgecore/main.go + status_report.go（上报循环）、edge/pkg/edgehub/client.go（Send 并发/断线语义）
- 方法：代码阅读 → 测试断言核对 → 命令验证 → 逐维度结论
- 红线：只读代码、未修改任何文件

### 命令验证结果（实测，非缓存）

| 命令 | 结果 |
|---|---|
| `go build ./...` | ✅ 通过 |
| `go vet ./...` | ✅ 通过 |
| `go test -race -count=1 ./...`（14 包，强制重跑） | ✅ 全绿；关键包覆盖率：podstatus 93.3%、cloudhub 84.1%、edged 88.0%、cmd/cloudcore 86.3%、cmd/edgecore 70.0% |
| `golangci-lint run ./...` | ✅ 0 issues |

## 1. 上报循环正确性（cmd/edgecore/status_report.go + main.go）— 通过

**周期实现**：`time.NewTicker` 标准实现，`defer ticker.Stop()` 防泄漏；启动即先上报一轮（`runStatusReportLoop` 首行同步调用），随后每 interval 一轮。符合"云端无需等待即可看到 Pod 状态"的设计意图。周期默认 30s，`EDGEFLOW_EDGECORE_REPORT_INTERVAL` 可配，`durationFromEnv` 对非法/非正数/未设置均回落默认并 Warn（测试覆盖 5 种输入）。

**首次上报时序**：`client.Start()` 在上报循环启动之前调用，但注册是异步的——首轮上报可能在未连接时全部失败。这是可接受的：失败仅 Warn，下一周期自动补报（见下）。

**发送失败处理（QoS 语义成立）**：`reportPodStatuses` 单条失败只记 Warn、循环继续、不重试。关键点是"下一周期补报"语义是否成立——成立：每轮 `BuildStatusPayload` 重新采样全量状态表，失败条目天然在下轮重发，无丢失窗口（前提是断线期间状态未被 cleanup 删除，见 §2 与 P1）。与 edgehub 的断线重连（指数退避 1s→60s）配合，恢复连接后 ≤30s 内全量补报。单条失败不影响本轮其余条目（循环继续），符合注释声明。

**消息构造**：`buildStatusMessages` 纯函数，Source=nodeID、Target="cloud"、Type=PodStatus、Payload=单条快照；信封 ID/Version/Timestamp 由 `protocol.NewMessage` 自动填充。测试断言了信封字段与负载无损往返（`TestBuildStatusMessages`）。

**goroutine 生命周期**：独立 `reportStopCh`/`reportDone`；main 退出顺序为 `close(reportStopCh)` → `<-reportDone`（确保不再有新消息写入通道）→ `edgedSvc.Stop()` → `client.Stop()`。上报循环是通道消费者，先停消费者再停生产者，顺序正确。`TestRunStatusReportLoopExitsOnStop` 实测：client 未连接（Send 必败）时循环不 panic、不退出、不阻塞，stopCh 关闭后 2s 内优雅退出。

**并发写安全**：edgehub `write()` 由 `writeMu` 串行化（gorilla/websocket 不允许并发写），心跳与上报循环并发写安全；断线时 `Send` 返回"未连接到云端"错误而非写已关闭连接（`setConn(nil)` 在会话退出时清理）。

## 2. P2-1 status 清理策略（edge/pkg/edged/status_report.go cleanupStatus）— 有条件通过

**时机**：每轮 `reconcileOnce` 第 5 步（孤儿清理之后）执行，期望集合为唯一权威。调用链完整：期望集合读取失败（`desiredPods` 报错）→ 本轮直接 return，不清理（保守，正确）；List 失败 → `listErr != nil` 传入，进入保守分支。

**条件逻辑**（与代码逐条核对）：
- key 在期望集合 → 保留 ✅
- 不在期望集合且 State==Absent && Err==nil（孤儿清理成功）→ 删除 ✅
- listFailed → 除已确认 Absent 外全部保留（不误删排查信息）✅
- 容器已不存在（不在 localKeys）→ 删除 ✅
- 容器仍在（孤儿清理失败）→ 保留 Error 条目，下一轮收敛后删除 ✅

**与容器实际状态的一致性**：核心正确性由 `EnsureStopped` 的幂等语义保证（docker `rm -f`，不存在即视为成功）；孤儿清理失败时条目带 Err 保留，不误删；`local` 快照在清理前捕获，容器在步骤 4 被删后其 key 仍在 localKeys 中，但 Absent 分支先命中 → 正确删除。三个异常路径均有真实测试覆盖（`TestEdgedCleanupStatusKeepsErrorEntryUntilContainerRemoved`、`TestEdgedCleanupStatusRemovesStaleEntryWhenContainerGone`、`TestEdgedCleanupStatusKeepsEntryWhenListFails`），断言具体、非形式化。

**并发安全**：`cleanupStatus` 持 `e.mu.Lock` 全量写；`setStatus`/`Status`（BuildStatusPayload 底层）分别持 Lock/RLock，无竞争（-race 实测通过）。

**问题（P1，见 §7）**：Absent 条目在"孤儿清理成功 → 同一轮 reconcile 内即被删除"的窗口内几乎不可能被 30s 周期的上报循环采样到——**契约中的 Absent 阶段实际不可达**，云端 API 对已删除 Pod 将永久显示陈旧状态（Running/Stopped）。注释声称"不会进入上报"，但技术上存在微秒级采样窗口（`setStatus(Absent)` 与 `delete` 之间持锁不同），该绝对化表述不严谨（P2 文档问题）；更实质的是设计后果：云端从未收到终态 Absent，`/api/v1/pods` 数据准确性受损。修复建议：延迟删除（Absent 条目保留至少一个上报周期）或删除前上报一次 Absent 终态。

## 3. 云端存储 PodStatusStore（cloud/pkg/podstatus）— 通过

**锁粒度**：单一 `sync.RWMutex`，所有方法内部加锁、Get/List 返回拷贝（测试 `TestReturnedCopies` 实测外部修改不污染存储）。粗粒度锁在当前规模（单机、几十 Pod）完全够用；多节点高频上报与 ListAll 会互斥，属规模问题（P2）。

**key 设计**：`nodeID → map[namespace/podName]PodStatus`，键带 namespace 防止跨命名空间重名覆盖（测试 `TestUpsertGet` 实测 default/web-demo 与 kube-system/web-demo 共存）；namespace 缺省统一补 "default"（两侧一致）。

**Upsert 语义**：空 nodeID/podName 忽略（防御）；`ps.NodeID` 以参数为准（消息来源即权威，CloudHub 层已校验 Source 一致性，双层防护）；同键整体覆盖。语义与测试断言一致。

**ListAll/ListByNode**：确定性排序（nodeID→namespace→podName / namespace→podName），输出可对账；空数据返回非 nil 空切片（`TestListEmpty` 断言 nil 检查），JSON 编码为 `[]` 而非 `null`。Delete 幂等且清空节点空 map。

**测试真实性**：93.3% 覆盖率；含 8 worker × 200 轮并发压测（-race 下验证）。断言全部针对具体值而非形式化。

## 4. CloudHub SetPodStatusHandler 回调 — 通过

**调用时机与校验**：`dispatch` 分发 → `handlePodStatus` 依次校验：未注册（not_registered Ack）→ payload 解析（invalid_message Ack）→ podName 必填（invalid_message Ack）→ payload.NodeID 与 Source 一致性（invalid_message Ack，防伪造错位）。校验通过后 `ps.NodeID = m.Source` 权威化，锁外调用回调，**成功不另行回 Ack**——与边缘侧语义一致（edgehub 出站消息无 Ack 等待机制，不存在悬挂等待；`TestPodStatusRejectedCases` 实测三种非法上报均被拒且不触发回调，Ack code 逐一断言）。

**回调并发**：回调在连接处理 goroutine 中同步执行；多连接上报并发进入 `podStore.Upsert`（内部加锁，安全）。同 nodeID 重复注册时旧连接被 `dead` 标记并 kick，新连接接管（`handleRegister` 换新逻辑），旧连接在关闭窗口内可能投递的最后一条 PodStatus 会被接受——但内容来自同一边缘节点、同一数据源，last-write-wins 语义下无实质影响（P2 备注）。

**panic 风险**：`notifyPodStatus` 锁外快照+调用，防回调反向调用 CloudHub 死锁（与 NodeEvents 同约定）✅；但 `readLoop`/`dispatch` 无 `recover` 防护——注入回调一旦 panic 将击穿整个进程。当前装配的回调（podStore.Upsert）不会 panic，属防御性缺口（P2）。

## 5. API 层（cmd/cloudcore/main.go）— 通过

**空数据处理**：`listPods`/`listNodePods` 均 `make([]PodStatus, 0)` 起步，存储未注入（测试场景）也返回空数组不 panic；`TestPodStatusAPIEmptyArray` 字节级断言 `"items":[]` 且不含 null。✅

**404 语义**：`/api/v1/nodes/{nodeID}/pods` 三态明确——节点未注册 → 404（与 `/api/v1/nodes/{nodeID}` 一致）；节点存在但无 Pod → 200 + `[]`（"节点健康只是没 Pod"与"节点未知"语义分离）。测试三态逐一断言。✅

**JSON 字段**：PodStatus 的 tag 与协议 payload 契约字段一一对应（nodeID/podName/namespace/phase/message/lastReconcileAt），小驼峰；`podStatusList` 为 K8s List 风格（kind=PodStatusList、apiVersion=v1、items），与 edgenodes 的 edgeNodeList 形态及未来 apiserver 响应结构对齐（客户端解析逻辑可平移）。✅

**已知注意点**：云端不做 phase 取值校验（任意字符串入库透出）——边侧可信前提下可接受，但属防御性缺口（P2）；404 错误体为 `{"error":..., "nodeID":...}` 内联 map，与现有 API 风格一致。

## 6. 生产就绪度 — 有条件通过

**日志**：关键路径均有日志（上报循环启动/停止、发送失败带 namespace/pod/phase 标识、云端收到记录、reconcile 摘要）。注意：cloudhub 对每条 PodStatus 记 Info（每 30s × Pod 数），规模大时是日志噪音（P2）。

**异常路径**：发送失败 Warn 不阻塞 ✅；List 失败保守清理 ✅；meta 订阅失败降级轮询 ✅；非法 env 回落默认 ✅；非法消息回 Ack 不中断连接 ✅。

**配置**：上报周期（EDGEFLOW_EDGECORE_REPORT_INTERVAL）、调谐周期（RECONCILE_INTERVAL）均 env 可配且带校验回落。缺口：无上下限约束（可配 1ms 打爆云端）、无多节点抖动（同拍上报）（P2）。

**已记录的已知缺口**（代码注释明示，均属 M3/apiserver 范围）：
- 上报尽力而为：无 Ack/缓存/持久化/排序 → M3 可靠上报；
- 云端内存态无持久化，重启丢失 → apiserver 对接；
- 节点断开/删除时 Pod 状态永久残留（与 registry 保留离线节点策略一致，有意为之）→ 需 TTL/GC；
- `LastReconcileAt` 为边侧时钟，跨节点时钟偏差影响语义（P2）；
- 注释引用 `e2e/mock-cloudhub` 目录不存在，实际为 `hack/mock-cloudhub`（冒烟工具，非自动化 e2e）——文档不准确（P2）；
- 每条 Pod 一条消息无批量（P2 规模备注）。

## 7. 结论与问题清单

**结论：有条件通过**（无 P0；1 项 P1 建议在 M2 收尾/进入 M3 前修复；其余 P2 可排期）。

### P0（无）

### P1
1. **契约 Absent 阶段实际不可达，云端 Pod 删除后状态永久陈旧**：cleanupStatus 在孤儿清理成功的同一轮 reconcile 内立即删除 Absent 条目，而上报周期 30s——删除窗口为微秒级，Absent 几乎不可能被采样上报。后果：a) 契约明确定义的 Absent 阶段成为死代码（云端 PodStatusStore 的 PhaseAbsent 常量亦无实际数据流）；b) 云侧删除 Pod 后 `/api/v1/pods` 与 `/api/v1/nodes/{nodeID}/pods` 永久显示该 Pod 的旧状态（Running/Stopped），状态 API 数据准确性受损（云端无期望状态可对照，无法自行收敛）。建议（任选其一）：① Absent 条目延迟删除——保留至"至少一次上报"（如在 cleanup 中删除前检查 LastReconcile 距今 > 一个上报周期，或由上报循环消费后标记删除）；② 删除前补发一条 Absent 终态消息。当前实现行为已在代码注释中声明为有意为之，但"云端知晓删除动作"仅对调用 syncPod 的调用方成立，对状态 API 的消费方不成立。

### P2
1. 文档不准确：status_report.go 注释称 Absent"不会进入上报"——技术上存在微秒级采样窗口（setStatus 与 delete 之间不持同一把锁路径），绝对化表述应改为"极难进入上报"；且 `e2e/mock-cloudhub` 引用路径不存在（实际 `hack/mock-cloudhub`）。
2. 云端无 phase 取值校验（任意字符串入库透出）；建议 CloudHub 或 store 层按契约枚举校验。
3. CloudHub 注入回调（SetPodStatusHandler/SetNodeEvents 路径）无 recover 防护，回调 panic 将击穿进程；建议 dispatch 层加防御性 recover。
4. cloudhub 每条 PodStatus 记 Info 日志，Pod 规模大时为噪音；建议降级 Debug 或按节点聚合。
5. PodStatusStore 全局单锁：单节点高频上报与 ListAll 互斥；规模扩大后建议分片/每节点锁。
6. 上报周期 env 无上下限约束（可配 1ms 自伤）；多节点同拍上报无抖动，建议加 jitter。
7. 节点断开/删除后 Pod 状态永久残留（有意为之，与 registry 一致）；建议文档化预期并规划 TTL。
8. `LastReconcileAt` 为边侧时钟，跨节点时钟偏差影响"最近协调时间"语义；建议云端记录接收时间戳作对照。
9. 每条 Pod 独立一条消息，无批量/压缩；Pod 数量线性增长（M3 优化项）。
10. 旧连接被 kick 后的关闭窗口内仍可投递 PodStatus（同节点同数据源，last-write-wins 无实质影响）；可在 dispatch 层对 dead 连接做短路。

### 复核确认
- 主线声明全部复核成立：14 包 `-race` 全绿（强制重跑非缓存）、总覆盖率 82.9% 与实测一致、lint 0、build/vet 干净。
- 测试断言真实：phase 映射 6 条全覆盖、cleanup 三异常路径、Ack 拒绝三场景、API 三态+字节级空数组断言、并发压测，均断言具体值。
- 本报告未修改任何代码。
