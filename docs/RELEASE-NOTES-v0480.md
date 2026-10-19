# EdgeFlow v0.48.0 发布说明（RELEASE-NOTES）

- 版本：v0.48.0（spec 0021：G32 日志采集链路 + G11 运维日志查看）
- 基线：v0.47.0（845e84a）
- 定位：补齐方案缺口 O4「日志采集链路：无→有」与 A5「运行监控/日志查看：日志面补齐」。

## N1 能力面（新增）

1. **边缘日志采集器 `edge/pkg/logcollector`**（opt-in，`EDGEFLOW_EDGED_LOGCOLLECTOR=on`
   默认关零行为）：
   - 文件源：`EDGEFLOW_EDGED_LOG_DIR` 下 `*.log` 追加 tail（启动从 EOF 不回放历史；
     truncate 检测重置；1s 轮询；`[LEVEL]` 前缀推断级别）；
   - 事件源：`Emit(source, level, message)` 程序内注入（scriptrunner 生命周期接线示范：
     部署成功/失败/卸载三态）；
   - 批量：满 batchN（默认 50）或 flush 间隔（默认 5s）→ 一条 LogSync（≤200 条/批
     拆批）；单条 message 4KB 截断（rune 边界安全）；缓冲上限 1000 条满丢最旧。
2. **消息契约**：新增 `LogSync`（边→云，批量日志条目），16→**17** 条消息（只增不改）。
3. **云端接收**：CloudHub `SetLogSyncHandler`（与 AlarmEvent 同构：校验失败回
   invalid Ack；通过后 msgID 滚动去重——独立实例不与 RuleEvent 计数混淆——锁外回调）。
4. **云端存储 `cloud/pkg/logstore`**：per-node 环形缓冲（默认 5000 条/节点，
   `EDGEFLOW_CLOUDCORE_LOGSTORE_MAX` 可调）+ 可选 JSONL 持久化
   （`EDGEFLOW_CLOUDCORE_LOGSTORE_FILE`，16MB 单代轮转 `.old`，启动回放损坏行跳过）；
   环形淘汰即轮转。
5. **检索 API 5 端点**（契约 97→**102**，只增不改）：
   `GET /api/v1/logs`（node/source/level/q/since/until/limit/offset，node 缺省跨节点）、
   `GET /api/v1/logs/nodes`、`GET /api/v1/logs/nodes/{nodeID}`（概要）、
   `GET /api/v1/logs/nodes/{nodeID}/tail`（尾部 N 条，默认 100 上限 1000）、
   `DELETE /api/v1/logs/nodes/{nodeID}`（清空）。
6. **断网不丢**：上行复用 v0.39 补传队列（UPLINK on 入队/off 直发；写出成功即删行，
   重发由云端 msgID 去重消化）——零新队列，语义与 alarm/rule 完全同构。

## N2 兼容与冻结

- 既有 16 类消息、97 端点、PodSync/ConfigSync 载荷零改动（只增不改）。
- scriptrunner 仅新增可选 `SetEventSink`（nil 安全，未注入零行为）。
- 边缘采集默认关；云端 logstore 无条件启用（内存模，无 503 分支）。
- 冻结带（v0240–v0350）零改动；go.mod 零变化。

## N3 测试（新增 14 例）

- logcollector 6：禁用零行为/文件 tail+级别推断/增量+truncate 重置/4KB 截断
  （rune 边界）/背压丢最旧/200 拆批。
- logstore 6：环形淘汰/过滤（level/source/关键字/时间窗/分页/跨节点归并）/
  概要+tail+删除/JSONL 持久化+回放+损坏行/轮转/负载 round-trip。
- cloudhub 1：LogSync 校验+去重（随包内既有消息处理测试口径）。
- e2e 1（`TestV0480LogPipelineE2E`，22.28s）：US-1 全链（4 条采集→检索可见 +
  tail/概要/清单三形态）、US-2 断网不丢（停云窗口 3 条 → 补传全量到达）、
  US-3 过滤（level/关键字/分页）、US-4 轮转（环形封顶 8 + DELETE 清空）、
  US-5 性能预算（2000 条注入后单查询 <2s 宽松断言）。

## N4 边界（登记 KNOWN-ISSUES §49）

- 文件行无可靠时间源：ts 统一取采集时刻（行内时间解析不做）。
- 采集 offset 不持久化：边缘重启从 EOF，重启窗口内新增行不上报（轻量采集定位）。
- 文件源 rotate 窗口可能短时重复/丢行（size<offset 重置从头；行级去重不做）。
- 云端保留策略=环形容量；按天清理与外部索引留后续。
- q/since 等查询参数需合法编码（URL 字面空格会被服务端 400 拒绝）。

## N5 门禁（2026-10-19）

- `gofmt` 清洁；`go vet ./...` 全净。
- `go test -race` 6 包全绿：logstore 4.111s / logcollector 6.288s / edged 2.385s /
  cloudhub 6.962s / appdeploy 2.990s / appstore 2.568s。
- 契约：**102 端点 / 17 消息**（四件套对齐；契约测试 13.872s 含子进程探测与文档比对）。
- 定向 e2e：`TestV0480LogPipelineE2E` **PASS 20.198s**（run7；run1-7 修复轨迹：
  ①q 参数含空格未编码 → 400（改单词过滤）；②LOGSTORE_MAX env 守卫 n<10 误拒
  测试值 8（放开为 n≤0 回退默认）；③US-4/US-5 重构为真实容量断言（复核 P2-4）；
  ④burst 背压吞行（自查，maybeFlush 同步放水）；⑤淘汰方向断言静默锚点）。
- 全量 e2e：**686.568s exit=0**（修复后终验，.cluster/edgeflow-v0480/e2e-full-v0480.log）。

## N6 复核（15min 硬时限，实际 11m41s）

- 原始结论：**FAIL（P1×3 / P2×6 / P3×9）**；处置后全绿放行（回执
  .cluster/edgeflow-v0480/review.md，原始报告 review-original.md）。
- **P1×3 全修**：①Query 跨节点递归 RLock 死锁 → 节点收集内联持锁段；
  ②文件源首见全量回放 → 启动快照二分语义（scanInitial 登记既有文件 EOF，
  启动后新文件从 0 收集）；③64KB 块无换行热自旋 → offset 恒前进 + 跨轮
  carry 半行缓冲 + 1MB 单行上限。
- **P2×6 全修**：UTF-8 净化（GBK 日志不再整批静默丢失）/clampSource rune 对齐/
  X-Total-Count=过滤后总数（对齐 §7.2 分页契约）/e2e US-4/US-5 真实容量重构/
  背压丢最旧内容断言/LogSync 逐消息 nodeID 身份比对（加固）。
- **P3**：快修 6（duplicate 注释/Stop sync.Once/envLogstoreMax 上界/Validate
  括号/NowMs 死代码/Stop 后 Emit 拒绝）；留档 3（appendJSONL 持锁写盘/e2e
  定宽 sleep/wake 无直接单测）→ KI §49。
- **自查新增**：burst 背压吞行（复核未涉及，e2e run5 暴露）→ tailFile 行循环
  maybeFlush 同步放水 + US-4 硬断言 rotateTotal ≥2900 防回归。
- 修正后复验：race ×7 包绿（新增 protocol）+ 契约 11.907s + 定向 e2e run7
  PASS 20.198s + 全量 e2e 692.336s exit=0。
