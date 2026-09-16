# EdgeFlow v0.37.0 发布说明（规则引擎与实时处理 + 数据治理）

- 发布日期：2026-09-16
- 基线：v0.36.0（a834e51）
- 主题：规则引擎与实时处理——规则模型与评估状态机、规则包下发（RuleSync）、事件上行（RuleEvent）、云端规则管理 API（9 端点）、数据治理过滤器（range/debounce/deadband）。契约 42→51 端点、云边消息 12→14；零依赖；默认零行为（无规则/无治理时全链逐字节兼容）。

## N1 能力（新增）

### 1. 规则模型与评估（pkg/rules）
- `Rule`/`Condition`/`Action`/`RuleSet`：阈值条件（gt/lt/gte/lte）+ 区间条件（between/outside）+ `forSeconds` 持续时间；动作类型 `event`（severity 三档 + 消息模板 `${device}`/`${property}`/`${value}`）；完整校验（操作符白名单、range 边界、ruleId 格式与保留字、包内唯一性）。
- 评估状态机（每 rule × 设备属性）：`idle → pending → firing`——首次满足触发（forSeconds=0）或持续满足 N 秒触发（中断重置重新计时）；firing 防重；恢复后可再次触发；规则替换重置全部状态；禁用规则不评估。

### 2. 数据治理过滤器（pkg/rules）
- 固定顺序三层：**range 越界拦截 → debounce 稳定 N 次确认 → deadband 微变抑制**；拦截值不写影子（抑制上报），坏值不进规则评估；未配置策略的属性恒直通（零行为）。

### 3. 云边通道与云端管理面
- 协议 +`RuleSync`（云→边：规则包全量下发，含 version/rules/governance）+`RuleEvent`（边→云：触发事件全字段）。
- 云端 9 端点：规则 CRUD（创建 201/重复 409/校验 400）+ 治理策略列表/全量替换 + 规则包下发（可靠投递，五态语义同 config-sync）+ 事件查询（ruleId/device/limit 过滤，倒序）。
- `cloud/pkg/rulestore`：内存 + etcd 写穿（键空间 `/edgeflow/ruleset/*`，版本毫秒单调、写穿失败不更新内存）；事件内存 ring（≤500，FIFO）。

### 4. 边缘装配（cmd/edgecore + edge/pkg/metamanager）
- `handleRuleSync`：校验 → 版本检查（陈旧拒绝）→ 评估器/治理器应用 → 持久化（`rules/current` 键；失败返回错误供云端重试，重试幂等）。
- 启动恢复：读取持久化规则包（损坏安全降级为空规则，不阻断启动）。
- 采样管道：挂接"采集 → 治理过滤 → 影子写入 + 规则评估 → 事件派发"；旧函数保留为兼容入口（无管道路径逐字节等价 v0.36.0）。
- 事件出口：`rule_events` 台账（SQLite，保留 30 天，组合查询）+ `RuleEvent` 上行（失败 Warn，尽力而为）。

### 5. 契约扩容
- HTTP 端点 42→51（+9，全部新增=向后兼容）；云边消息类型 12→14；API-SPEC §1.1 与 API-COMPATIBILITY §1/§2 矩阵同步；契约测试源码反扫新增 rules_api.go、运行时探测适配。

## N2 兼容与冻结
- **默认零行为**：无规则/无治理策略时，采集/影子/上报路径与 v0.36.0 逐字节等价（旧 `collectMapperReports` 保留为兼容入口 + `reportDeviceReports` 委托）；启动链路仅新增规则链就绪日志（台账就绪/恢复提示），数据路径零变化。
- 契约既有 42 端点零改动（只增 9）；消息既有 12 类型零变化（只增 2）；MQTT/OPC-UA/视频/模型面代码零触碰；v0240–v0350 冻结测试零改动；零新依赖（纯标准库 + 既有包）。

## N3 测试（数字已按 grep 口径逐项核对）
- pkg/rules 21 个用例函数 / 59 个叶子用例（含 42 子测试：模型校验/评估状态机/治理过滤器全语义，含复核候选重置回归锚）；cmd/edgecore 11 例（RuleSync/恢复/管道/事件出口/消息构造）；cloud/pkg/rulestore 9 例（CRUD/哨兵/写穿/Load/ring/损坏跳过）；cmd/cloudcore 7 组 / 10 叶子（9 端点全路径/五态映射/保留段路由）。
- e2e 2 例（真实进程）：规则全链（创建→下发→触发→事件查询→firing 防重）+ 治理拦截（越界值冻结）。
- 契约：51 端点静态反扫 + 运行时探测 + API-SPEC/API-COMPATIBILITY 双文档一致性 + 消息矩阵双向比对。

## N4 边界（登记 KNOWN-ISSUES §38）
- 动作类型仅 `event`；无告警"恢复/清除"事件；云端事件 ring 为内存滚动（多副本聚合/持久化归档后续评估）；debounce 判定用严格相等；规则包不自动补发离线节点；ruleId 校验宽松。

## N5 门禁（2026-09-16 全绿）
- vet（全仓）/ 全仓回归（除 e2e/契约，含 mqtt 3.3s + mqttsim 17.8s 冻结面）/ race ×6 包
  （rules/metamanager/rulestore/cloudhub/edgecore/cloudcore，最长 17.4s）/ 契约 13.4s / e2e 全量 430.3s：
  全绿（0 FAIL）。
- 复核处置后复验：race（pkg/rules + cmd/edgecore）复跑绿、契约重跑绿（处置为 pkg/rules
  实现修复与小测试扩展）。
