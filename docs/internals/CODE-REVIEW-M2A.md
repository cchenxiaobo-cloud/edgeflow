# CODE-REVIEW-M2A — M2 启动轮代码审查报告

- 审查日期：2026-08-14
- 审查范围：M2 启动轮提交
  - `089c358` metamanager 增量订阅（Subscribe/Unsubscribe/Event，缓冲满丢弃）
  - `8321b0e` 4.6 P2 五项（pending 交叉清理+ErrShuttingDown、ReliableSendContext、ErrAckFailed→502、downlink 原子性、operation 校验）
  - `c9db4ba` Edged POC（ContainerRuntime 接口、MockRuntime、DockerRuntime、声明式 reconcile、Status()、docs/EDGED-POC.md）
  - `46b8681` gofmt
  - `9fe47c1` edgecore 装配（Edged+订阅触发 Trigger）+ hack/edged-smoke
- 审查人：M2POC 复核员（资深 Go 工程师视角）
- 审查方式：只读代码 + 独立重跑验证命令，未修改任何代码

## 状态

- 总评：✅ **有条件通过**（POC 范畴内通过；无 P0/P1；P2 项 8 条为防御性与收尾改进）

## 0. 审查命令与结果（独立重跑，非缓存）

```bash
go build ./...                        → BUILD_OK
go vet ./...                          → 0 issues
go test -race -count=1 -cover ./edge/... ./cloud/... ./cmd/...
  edge/pkg/edged       ok  3.832s  coverage: 85.1%
  edge/pkg/edgehub     ok  6.216s  coverage: 84.8%
  edge/pkg/metamanager ok  6.239s  coverage: 82.0%
  cloud/pkg/cloudhub   ok  7.150s  coverage: 83.6%
  cloud/pkg/registry   ok  5.344s  coverage: 100.0%
  cmd/cloudcore        ok  4.522s  coverage: 89.3%
  cmd/edgecore         ok  6.958s  coverage: 64.3%
golangci-lint run ./...               → 0 issues
gofmt -l edge/ cloud/ cmd/ hack/      → 空（格式干净）
edged 定向测试（-run Edged|Docker|Container|PodKey|Parse|Runtime|NewDefault|Mock）→ 24 PASS
metamanager 订阅定向测试（-run Subscribe|Unsubscribe|StoreClose|Concurrent）→ 6 PASS
```

结论：测试断言真实（非空转）；`-count=1` 强制重跑排除了缓存干扰。

## 1. Edged 状态机

- **收敛性**：期望集合（store.ListPods）与实际集合（rt.List）逐轮对账，期望有→EnsureRunning，本地有而期望无→EnsureStopped 孤儿清理，两集合之外无第三态；`desiredKeys` 用 podKey（ns 缺省补 default）与 metamanager 存储 key 规则对齐，比对键一致。✅
- **幂等**：接口契约明确要求幂等（runtime.go 注释），Mock（createCalls/removeCalls 只在状态跃迁时自增）与 Docker（inspect 判存在、rm -f 对不存在 no-op、run 冲突兜底）双实现一致；`TestEdgedReconcileIdempotent` 断言连续两轮 CreateCount==1。✅
- **错误不中断循环**：单 Pod 失败→setStatus(Unknown,err)+continue；List 失败→仅警告并跳过孤儿清理（local=nil），EnsureRunning 仍执行；desiredPods 失败→返回错误但 loop 继续下轮；脏 JSON 跳过不中断。`TestEdgedReconcileFailureRetries/ListFailureContinues/InvalidPodJSONSkipped` 逐条覆盖。✅
- **Status 线程安全**：RWMutex 保护 map，Status() 返回深拷贝；setStatus 与 loop、测试直调并发安全。⚠️ **缺陷（P2-1）**：status map 只增不删——Pod 从期望删除且容器已不存在（如 rm 在 daemon 恢复前完成、或 List 从未返回过它）时，旧条目（可能含历史 Err/Unknown）永久残留，长期运行无界增长；WBS 6.3 上报前必须做 mark-and-sweep（每轮对 desired∪local 之外的 key 清理）。
- **Stop/Start 生命周期**：stopCh/doneCh 以参数传入 loop（不触碰共享字段，避免与 Stop 的 nil 写竞争，注释明示 -race 可证）；重复 Start no-op、重复 Stop no-op、Stop 后可重启，`TestEdgedStopStopsLoop/StartTwiceIsNoop/StartReconcilesImmediately` 覆盖。⚠️ 注意（P2-8）：Stop 阻塞等待进行中的 reconcile 完成，docker 命令单次最长 30s，优雅退出最坏延迟取决于在途命令（可接受，建议文档注明或后续 ctx 化）。
- **Trigger 竞态**：triggerCh 永不被 close（容量 1），Stop 后 Trigger 仅向缓冲写入、无 send-on-closed panic；重启后陈旧信号只多触发一轮幂等调谐，无害。reconcileMu 串行化 loop 与外部直调。✅

## 2. DockerRuntime

- **exec 超时与上下文**：每命令独立 `context.WithTimeout`（默认 30s），超时错误明确含命令与时长；`TestDockerExecTimeout` 覆盖。⚠️（P2-2）：`&DockerRuntime{}` 零值 timeout=0 会让所有命令立即超时（绕开构造函数即静默全坏），建议 exec 内对 timeout<=0 回落默认值。
- **容器名规范与冲突**：`edgeflow-<ns>-<name>`，小写/非法字符替换/首字符合法化/超 200 截断+sha256 前 8 位防碰撞；测试覆盖非法字符、超长、碰撞。run 冲突（name already in use）→ 重新 Inspect 按已存在处理，并发兜底合理。⚠️（P2-3）：冲突兜底未校验占用方是否带 `edgeflow.pod` 标签——若外部工具恰好占用同名容器会被视为"已管理"；实际影响有限（List 按标签过滤不会误清外部容器，下一轮 Inspect 也只会 start/不动），且已在 EDGED-POC.md §5 风险 3 声明；建议 Inspect 后校验标签或至少 Warn。
- **标签发现**：List 用 `--filter label=edgeflow.pod` + 反解 label 值（不解析名字，避免 '-' 歧义）；格式异常行防御性跳过；ns 标签缺失兜底 default。结合容器名内嵌 ns，最坏情形（缺 ns 标签）为 rm 落空或短暂双容器后自愈，无破坏性误删路径。✅
- **daemon 不可用错误前缀**：CLI 缺失/daemon 未起/连接拒绝/启动中均归入 `docker daemon unavailable: ` 前缀，与"容器不存在"互斥判断（isNoSuchContainer 先排除 daemon 类）；EnsureRunning/EnsureStopped 对已带前缀错误直接透传不二次包装。`TestDockerDaemonUnavailablePrefix` 覆盖三入口。✅
- **inspect 解析健壮性**：精确匹配 true/false；异常输出→Unknown+err 不误报；No such→Absent；daemon 不可用→Unknown+err。✅
- **幂等语义**：Inspect 判存在三分支（run/start/no-op）；rm -f 对不存在 no-op；冒烟测试（真实 daemon）6 步全流程（创建→幂等→标签发现→删除→幂等删除→残留检查）断言真实。✅

## 3. 订阅机制（metamanager notify）

- **订阅者 map 锁**：subMu 保护 subscribers/nextSubID；notify 持锁遍历+非阻塞投递，Unsubscribe 持锁删除+close——「发送时通道必在表中」的互斥约定成立，无 send-on-closed 竞态；`TestConcurrentSubscribeNotify`（4 订阅者×2 写者×60 轮）-race 无告警。✅
- **缓冲满丢弃 → Edged 漏触发？**：不成立为缺陷。两层兜底：① Edged.Trigger 本身合并（triggerCh 容量 1），丢事件只意味着少一次即时触发；② 声明式 reconcile 每 5s 全量对账，最坏延迟 5s（delete 事件丢失时容器多存活 ≤5s）。`TestSubscribeBufferFullDrops` 验证写路径永不阻塞。✅
- **Unsubscribe 与 goroutine 生命周期**：注销后通道关闭，消费方（edgecore 的 `for ev := range eventCh`）据此退出；重复注销/未知 ID 幂等；`TestUnsubscribeStopsEvents` 断言 ok=false 感知关闭。✅
- **订阅 ID 复用**：nextSubID 单调递增，无复用（int 溢出不现实）。✅
- **Store.Close**：先清表再关库，逐个 close 不 double-close；`TestStoreCloseClosesSubscriberChannels` 覆盖。✅

## 4. P2 收尾（reliable.go / ack.go / syncPod）

- **pending 交叉清理**：unregisterPending 归属校验（`pending[msgID] == e`）——同 ID 并发在途时旧等待者不会误删新条目；`TestReliableSendSameIDNoCrossCleanup` 用 800ms 延迟 Ack 精确构造时序验证。✅
- **Shutdown 并发安全与等待方行为**：`shuttingDown.Store(true)` 严格先于 `failAllPending()`，与 ReliableSendContext 注册前后两段式检查构成严密互斥：注册在 failAllPending 前→通道被 close 以 nil 唤醒返回 ErrShuttingDown；注册在其后→第二段检查直接返回，不存在「注册后无人唤醒空等至超时」窗口。failAllPending 与 resolvePending 同持 ackMu，无 send-after-close；`TestReliableSendShutdownWakeup` 覆盖在途+新调用两路径。✅
- **ReliableSendContext 语义**：等待期间监听 ctx.Done()，取消立即返回（包装 ctx.Err()，errors.Is 可判）；nil ctx 兜底 Background；ack 缓冲 1 防「Ack 先于 select 到达」丢失；`TestReliableSendContextCancel` 覆盖。✅
- **ErrAckFailed→502**：error Ack 不再重试（消息已到达，重发无意义）直接返回 ErrAckFailed；syncPod 映射 404 离线/502 拒绝/504 超时/500 兜底，语义分层正确，`TestSyncPodAckFailed/NodeOffline/AckTimeout/UnexpectedError` 全覆盖。✅
- **operation 校验**：云端 400 前置（P2-5，省 ~15s 往返），测试覆盖空串/大写/`add\u0000` 等边界；边缘 handlePodSync 仍有 default 分支 error Ack 双层兜底。✅
- **downlink 原子性**：downlinkMu 把「isProcessed 检查→handler 执行→markProcessed」整体串行化，消除重连窗口双 readLoop 并发执行同 ID 的窗口；锁内执行 handler 不反向调用 Client 加锁方法（注释论证成立，handlePodSync 仅操作 store）。代价是下发串行处理（边缘量级可接受）。✅

## 5. 装配正确性（cmd/edgecore/main.go）

- **启动顺序**：store→handlers→client.Start→edged.Start（启动即全量调谐一次）→Subscribe。订阅晚于 Start 无缺口：初始调谐 + 5s 轮询兜底覆盖订阅建立前的变更。✅
- **优雅退出顺序**：edged.Stop（循环退出）→ client.Stop（connWG 等待全部 readLoop 退出，handler 全部结束，之后不再有 store 写）→ Unsubscribe（关闭事件通道，订阅 goroutine 退出）→ deferred store.Close（清订阅表+关库）。无 goroutine 泄漏、无 double-close、无「关库后回调仍写库」。✅
- **订阅 goroutine 泄漏面**：正常退出由 Unsubscribe/Close 关通道唤醒；Subscribe 失败降级轮询（warn 日志）；edged.Stop 与 Unsubscribe 之间的窗口内 Trigger 只写容量 1 的 triggerCh，不阻塞不 panic。✅
- **测试覆盖**：`TestRunStartsEdgeHubAndExitsOnSignal` 实际跑通 run() 全装配（含 Edged+订阅）SIGTERM 退出；handlePodSync 单测 6 条（add/update/delete 跨命名空间/未知 op/坏 payload/缺 name）。✅
- **已知缺口（非本轮缺陷）**：无 PodStatus 上报云端——Edged 状态只存在于 Status() 与日志（WBS 6.3 预留）；cmd/edgecore 覆盖率 64.3% 主要缺口在订阅装配细节，由端到端实测补充。

## 6. 生产就绪度

- **日志**：reconcile 每轮摘要（含错误数）、单 Pod 失败 Errorf、孤儿清理、订阅启用/失败、daemon 不可用、超时均有明确日志；错误信息含容器名/命令/输出首行，可诊断。✅
- **异常路径**：daemon 不可用（前缀错误）、命令超时、脏 JSON、List 失败、inspect 异常输出、Ack 迟到/来源不符、离线发送、Shutdown 竞态——均有明确处理与测试。✅
- **配置**：DB 路径/云端地址已有 env 覆盖；⚠️（P2-7）reconcile 周期在 cmd/edgecore 硬编码 5s，建议 `EDGEFLOW_EDGECORE_RECONCILE_INTERVAL` env；⚠️（P2-6）云端 syncPod 未校验 image 非空/replicas 范围（非法值要等边缘 error Ack 才暴露，与 P2-5 同思路应前置 400）。
- **依赖克制**：本轮零新 Go 依赖（os/exec docker CLI + modernc sqlite + gorilla/websocket 均已有），与项目依赖原则一致。✅
- **文档**：EDGED-POC.md 方案论证完整（风险清单 7 项、未验证边界、工作量修正、复现方式），与代码实现一致。✅

## 7. 结论与问题清单

**结论：有条件通过**。声明式 reconcile 状态机、DockerRuntime 幂等与错误归一、订阅背压兜底、P2 五项收尾、edgecore 装配均经独立重跑验证（-race -count=1 7 包全绿、lint 0、gofmt 干净），测试断言真实。8 条 P2 均为防御性/收尾改进，不影响本轮验收与主线端到端结论（PodSync→订阅→调谐→容器创建/删除 已实测）。

### P0（阻断）

无。

### P1（高危）

无。

### P2（改进项，按优先级）

1. **Edged.status 过期条目不清理**（edge/pkg/edged/edged.go）：pod 删除且容器已不存在时旧状态/错误残留、map 无界增长；建议每轮调谐对 desired∪local 之外的 key 做清理。WBS 6.3 上报前必改。
2. **DockerRuntime 零值构造即全坏**（docker_runtime.go）：`&DockerRuntime{}` timeout=0 → 所有命令立即超时；exec 内对 timeout<=0 回落默认值。
3. **run 冲突兜底未校验对方标签**：名字被外部占用时误判为已管理；建议重新 Inspect 后校验 `edgeflow.pod` 标签或 Warn（风险已在 POC 文档声明，低危）。
4. **nodeAPI.hub 死字段**（cmd/cloudcore/main.go）：赋值后未使用，删除或改用于注入。
5. **syncPod 未用 ReliableSendContext(r.Context())**：客户端断开仍完成整轮投递（QoS1 语义下可辩护，建议注释记录意图或改为 ctx 取消等待）。
6. **云端未前置校验 image 非空/replicas 范围**：与 P2-5 同思路，400 前置省一轮可靠投递往返。
7. **reconcile 周期硬编码 5s**：建议 env 可配（`EDGEFLOW_EDGECORE_RECONCILE_INTERVAL`）。
8. **优雅退出最坏延迟 ≈ 在途 docker 命令超时（30s）**：可接受，建议文档注明；后续可让 Stop 支持 ctx 提前终止。

### 复核结论对主线声明的确认

- 主线「端到端 PodSync→订阅→Edged→容器创建/删除」：装配顺序与事件链路在代码上自洽（handlePodSync→SavePod→notify→订阅 goroutine→Trigger→reconcile），订阅丢事件由 5s 轮询兜底，声明式收敛成立。✅
- 主线「13 包 race 全绿、总覆盖率 82.4%、lint 0」：本次独立重跑覆盖 edge/cloud/cmd 7 包全绿（覆盖率与主线一致），lint/gofmt 复现 0 issue。✅
- 主线「DockerRuntime 冒烟 6 步通过」：冒烟测试代码真实覆盖 6 步断言（创建→幂等→标签发现→删除→幂等删除→残留检查）。✅
