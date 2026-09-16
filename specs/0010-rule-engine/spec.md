# Spec 0010：规则引擎与实时处理（含数据治理）

- 版本归属：v0.37.0（基线 a834e51 = v0.36.0）
- 规范依据：《EdgeFlow 后续开发计划（对齐智能边缘计算平台解决方案）》v0.37 定义（差距 G15 规则引擎/实时处理 + G14 数据治理最小面）+ 用户方向「继续开发后续功能，形成新的版本」
- 裁定结论（本 spec 前置决策）：
  - 全链交付：规则模型与校验（pkg/rules）→ 规则包下发（RuleSync 协议）→ 边缘实时评估（状态机 + 治理过滤器）→ 事件上行（RuleEvent）→ 云端统一接收与查询（rules API + 事件查询）→ 执行台账（边缘 SQLite）。
  - 契约扩容轮：HTTP 端点 42 → 51（+9），云边消息类型 12 → 14（+RuleSync/+RuleEvent）——契约只增不改，全链同步（routes.go / API-SPEC / API-COMPATIBILITY / 文档一致性测试）。
  - 零第三方依赖（宪法 II）；默认零行为（无规则/无治理策略时，采集/影子/上报路径逐字节不变）；v0240–v0350 冻结测试零改动；MQTT 3.1.1 路径零触碰。

## 用户故事与验收

### US-1 规则模型与校验（pkg/rules）
- `Rule`：`ruleId`（必填，^[a-z0-9][a-z0-9-]{0,62}$（1-63 字符），保留字 `governance`/`events` 拒绝）、`name`、`namespace`（缺省 default）、`deviceName`（必填）、`property`（必填）、`enabled`（缺省 true）、`condition`、`action`。
- `Condition`：`type` ∈ {`threshold`, `range`}；threshold op ∈ {`gt`,`lt`,`gte`,`lte`} + `value`；range op ∈ {`between`,`outside`} + `min`,`max`（min<max）；`forSeconds`（int, ≥0，缺省 0）。
- `Action`：`type` ∈ {`event`}（v0.37 唯一动作类型）；`severity` ∈ {`info`,`warning`,`critical`}（缺省 warning）；`message`（模板，支持 `${device}`/`${property}`/`${value}` 占位）。
- `GovernancePolicy`：`deviceName` + `property` 必填；`deadband`（float, >0 启用）、`debounce`（int, ≥2 启用）、`range`（{min,max} 可选）三选一以上；namespace 缺省 default。
- `RuleSet`：`version`（int64, ≥1）、`rules`、`governance`；`Validate()` 逐条校验并在错误中给出 ruleId/位置。
- 验收：字段边界（op 白名单/min<max/保留字/forSeconds 负数）、JSON roundtrip、模板替换（含未知占位符保留原文）、空 RuleSet 合法。

### US-2 规则评估器（edge 语义状态机）
- 输入：`Observe(deviceName, namespace, property string, value float64, ts int64)`（毫秒）。
- 状态机（每 rule × 目标设备属性）：`idle → pending（记录首次满足 ts）→ firing（已触发）`：
  - `forSeconds=0`：首次满足即触发（当轮进入 firing）。
  - `forSeconds>0`：满足持续 `ts - pendingStart >= forSeconds*1000` 才触发；期间任一采样不满足 → 回 idle（重新计时）。
  - firing 后不重复触发；采样变为不满足 → 回 idle（可再次触发）。
  - 规则 `enabled=false`：不评估（且清理该规则状态）。
  - 坏值不经过评估器（由管道层跳过，见 US-6）；NaN → 视为不满足（防御）。
- 输出：`[]Event`（ruleId/ruleName/deviceName/namespace/property/value/severity/message/triggeredAt/ruleSetVersion）。
- `ApplyRuleSet(rs)`：替换规则集 + 重置全部评估状态；`Version()`、`Stats()`（evaluated/triggered 计数）。
- 验收：阈值四 op、区间两 op、forSeconds 计时（含跨越触发/中断重置）、firing 不重复、恢复再触发、多规则独立、替换规则集后状态清零。

### US-3 数据治理过滤器（pkg/rules）
- `Governor`（每 device×property 独立状态）：`Filter(deviceName, namespace, property string, value float64, ts int64) Decision`。
- 处理顺序（固定）：
  1. **range**：启用且 value 越界 [min,max] → `{Accept:false, Reason:"out_of_range"}`（值不更新，状态保持）；
  2. **debounce** N：value ≠ lastAccepted 时需连续 N 次采样值严格相等才获候选资格（期间变化则计数重置）；value == lastAccepted 直接获候选资格；未获资格 → `{Accept:false, Reason:"debounce"}`；
  3. **deadband** D：候选值与 lastAccepted 差 |Δ| < D → `{Accept:false, Reason:"deadband"}`；否则接受并更新 lastAccepted。
- **同值快速路径（as-built 补充，2026-09-16，复核 P1-2 处置）**：value == lastAccepted 时直接采纳（不进入 deadband 判定——Δ=0 无信息，重复拦截为计数噪音）、并重置 debounce 候选连续性（严格「连续 N 次」不允许被稳态值打断后跨段累计）。
- 未配置策略（device×property 无 policy）→ 恒 `{Accept:true, Value:value}`（直通，零开销路径）。
- `Decision{Accept bool, Value float64, Reason string}`；`Governor.Stats()`（拦截计数 by reason）。
- 验收：三种过滤器独立生效、两两组合顺序、（range 拦截不污染 debounce 计数）、直通零变化、`ApplyPolicies` 替换后状态重置。

### US-4 协议扩展（pkg/protocol）
- 新增常量：`TypeRuleSync = "RuleSync"`（云→边：规则包全量下发）、`TypeRuleEvent = "RuleEvent"`（边→云：规则触发事件）。
- 契约矩阵同步：`ContractMessageTypes` 12 → 14 条（活跃 12 + 占位 2）；`docs/API-COMPATIBILITY.md` §2 矩阵 +2 行。
- 验收：常量存在（契约文件编译期绑定）；未知类型处理路径不变（边/云 default 分支忽略）。

### US-5 云端：规则包存储 + API + 下发/接收（cloud/pkg/rulestore + cmd/cloudcore）
- **存储（rulestore.Store）**：
  - 规则单条 CRUD（内存 + etcd 写穿，写穿模式与 devicestatus 同构：etcd 成功才更新内存；异步多副本一致）、治理策略全量（单键 `/edgeflow/ruleset/governance`）、包版本号（`/edgeflow/ruleset/version`，变更时 `max(now, version+1)` 单调递增）。
  - 事件 ring：最近 500 条（FIFO 滚动，内存；多副本聚合/持久化登记 KI §38）。
  - `Load(ctx)` 启动恢复；键空间 `/edgeflow/ruleset/*`。
- **API（9 端点，全部挂 auth/audit 链）**：
  1. `POST /api/v1/rules`：创建（ruleId 重复 409；校验失败 400；成功 201）。
  2. `GET /api/v1/rules`：列表（K8s List 风格 items；按 ruleId 排序）。
  3. `GET /api/v1/rules/{ruleID}`：详情（404）。
  4. `PUT /api/v1/rules/{ruleID}`：更新（404；不可改 ruleId）。
  5. `DELETE /api/v1/rules/{ruleID}`：删除（404）。
  6. `POST /api/v1/nodes/{nodeID}/rules/sync`：下发当前全量规则包（version+规则+治理）到指定节点（reliableSend；离线 404 / ack 拒绝 502 / 超时 504，与 device-command 五态对齐）。
  7. `GET /api/v1/rules/events`：事件查询（ruleId/deviceName 过滤、limit≤200 默认 50、按时间倒序）。
  8. `GET /api/v1/rules/governance`：治理策略列表（含 version）。
  9. `PUT /api/v1/rules/governance`：治理策略全量替换（校验失败 400；成功返回新 version）。
- **事件接收**：cloudhub `handleMessage` +`case TypeRuleEvent` → 回调注入（`SetRuleEventHandler`）→ rulestore.AppendEvent（+日志）。
- 验收：CRUD 全路径（含校验/404/409）、sync 组包内容正确（离线 404 / 拒绝 502 / 超时 504 语义）、事件接收→查询闭环、etcd 写穿（失败不更新内存）、Load 恢复、版本单调。

### US-6 边缘：装配全链（cmd/edgecore + edge/pkg/metamanager）
- **RuleSync 处理**（rule_handlers.go）：解析 payload → `rules.RuleSet.Validate` → 版本检查（`version < current` → error「规则包版本陈旧」；`>=` 接受）→ 引擎 `ApplyRuleSet` → 持久化 `store.Put("rules/current", json)`（失败 → error 供云端重试，重试幂等）。
- **启动恢复**：`loadRuleSet(store)` 读取持久化规则包 → 校验+应用；损坏 → Warn + 空规则（安全降级，不阻断启动）。
- **采集管道挂钩**（device_mapper.go）：
  - 保留原 `collectMapperReports(reg, twins, now)` 签名（行为逐字节不变）；新增 `collectMapperReportsWith(reg, twins, now, pipe *samplePipeline)`，`reportDeviceReports` 改用新函数（pipe=nil 时与旧路径等价）。
  - `samplePipeline`（装配层组合）：对每台 Mapper 的采集值 → 治理过滤（逐属性）→ 通过值写影子；拦截值不写（计数）。随后对"有效值"（含拦截项沿用 lastAccepted 语义：out_of_range 跳过、其余用治理后有效值）调用引擎 `Observe`，收集事件。
  - 无规则且无策略：等价直通（零行为）。
- **事件上行 + 台账**：事件 → ① `client.Send(TypeRuleEvent)`（失败 Warn，尽力而为——恢复靠后续触发）；② `RuleLedger.SaveEvent`（metamanager 新表 `rule_events`：ts/rule_id/device_id/severity/value/message，保留 30 天，ListRuleEvents 查询）。
- 验收：RuleSync 接受/陈旧拒绝/非法拒绝、重启恢复、管道直通等价、治理拦截不写影子、事件发送+台账、默认零行为（无规则下 collect 路径逐字节等价）。

### US-7 兼容与冻结
- 默认路径（无规则/无治理）：edgecore 启动无新日志分支、采集/影子/上报全链逐字节不变（旧函数保留 + 等价测试锚）。
- 契约 42 端点零改动（只增 9）；消息类型只增 2。
- MQTT 3.1.1 路径零触碰；v0240–v0350 冻结测试零改动。
- edgecore 消息处理 default 分支不变（未知类型忽略路径保持）。
- 零新依赖（纯标准库 + 既有包）。

## 冻结与兼容
- 新协议类型仅在"规则包下发/事件上报"场景出现；既有 12 类型行为与编码零变化。
- edgecore 未收到 RuleSync 时行为与 v0.36.0 完全一致（引擎空转路径 = 无副作用调用；由"无规则零行为"测试锚守护）。
- 云端 rules API 为新增路由；`/api/v1/rules/*` 前缀此前无路由（无遮蔽）；`governance`/`events` 段为保留字（ruleId 校验拒绝，路由歧义不可能）。
- 契约测试"源码反向断言（无契约外路由）"要求 9 条新路由与 routes.go 同步落地。

## 测试锚（预估，实现后核对）
- pkg/rules（v0370_test.go + governance 测试，预估 30-40 例）：模型校验/求值/状态机/治理/引擎。
- cmd/edgecore（v0370_*_test.go，预估 12-18 例）：RuleSync 处理、恢复、管道、事件构造、兼容等价。
- cloud/pkg/rulestore（v0370 测试，预估 8-12 例）：CRUD/写穿/Load/版本/事件 ring。
- cmd/cloudcore（v0370_rules_api_test.go，预估 15-20 例）：9 端点全路径。
- tests/e2e（v0370_rule_e2e_test.go，预估 2-3 例）：规则全链（创建→下发→触发→事件查询）、治理拦截（值冻结）。
- tests/contract（改动）：计数更新 + 新端点纳入探测。

## 边界与非目标（登记 KNOWN-ISSUES §38）
- v0.37 动作类型仅 `event`；指令联动/影子写入动作后续版本评估。
- 告警"恢复/清除"事件不做（状态机内部恢复，不产生 cleared 事件）。
- 云端事件 ring 内存 500 条（多副本聚合、持久化归档后续评估）。
- 表达式引擎为阈值/区间的受限子集（非通用表达式）；debounce 严格相等（容差版后续评估）。
- 治理策略为空=直通；"多策略叠加/策略优先级"不做（每 device×property 单策略）。
- 规则包不下发到离线节点（重连后需重新 sync；自动补发后续评估）。
- ruleID 与 deviceName 的 K8s 风格强校验不做（宽松：非空 + 长度上限）。

## as-built 登记（开发完成，2026-09-16）

- 实现面：pkg/rules（rules.go / eval.go / governance.go）；pkg/protocol（+TypeRuleSync/+TypeRuleEvent）；
  cmd/edgecore（rule_handlers.go：handleRuleSync / loadRuleSet / 事件出口 / 采样管道；
  device_mapper.go 管道内核 collectMapperSamples；device_handlers.go 包装变体；main.go 装配）；
  edge/pkg/metamanager/rule_ledger.go（rule_events 表，保留 30 天）；cloud/pkg/rulestore（内存 +
  etcd 写穿，键空间 /edgeflow/ruleset/*）；cloud/pkg/cloudhub/rule_event.go；cmd/cloudcore/rules_api.go
  （9 端点）+ main.go 装配；契约与双文档矩阵同步。
- 测试锚（逐条核对，2026-09-16 grep 口径）：pkg/rules 21 个用例函数 / 59 个叶子（含 42 子测试）；cmd/edgecore 11 例；cloud/pkg/rulestore 9 例；
  cmd/cloudcore 7 组 / 10 叶子（RuleCRUD / RuleCreateValidation / Governance / EventsQuery / Sync /
  SyncFailureMapping / ReservedSegmentRouting）；e2e 2 例（TestV0370RuleE2E / TestV0370GovernanceE2E）；
  契约：51 端点静态反扫 + 运行时探测 + 双文档一致性 + 消息矩阵双向比对。
- 开发期调整：① ruleId 下限放宽为 1 字符（实现 ^[a-z0-9][a-z0-9-]{0,62}$，spec 文本同步）；
  ② 契约测试运行时探测新增 {ruleID} 替换与无参数 PUT 空体分支（期望 400）；
  ③ 契约源码反扫新增 rules_api.go（原仅 main.go/model_api.go）。
- 复核处置（2026-09-16）：④ 治理同值快速路径补充「重置 debounce 候选连续性」（跨打断累计语义瑕疵修复，含回归锚
  TestGovernorDebounceCandidateResetOnSteadyValue）；⑤ 数字口径修正（本段）；⑥ spec 键空间/sync 状态码文案修正；
  ⑦ 空版本规则包（version=0）拒绝测试锚补充；⑧ 「启动零新日志分支」措辞修正（仅就绪日志）。
- 门禁：见 RELEASE-NOTES-v0370 N5。
