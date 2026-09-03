# EdgeFlow 边缘计算产品开发规范 v1.0.0

**生效**: 2026-09-01 ｜ **适用**: EdgeFlow 全部仓库与衍生工作 ｜ **方法**: GitHub spec-kit（Spec-Driven Development）
**配套**: 工程宪法 `.specify/memory/constitution.md`（八原则）｜ 特性台账 `docs/solution-manual/特性-场景映射表.csv`（F01–F48）｜ 代码基线 v0.27.0+

---

## 0. 文档定位

本规范回答三个问题：**产品有哪些场景（第 2 章）、每个场景背后真正的要求是什么（第 3 章）、要求的实现按什么纪律推进（第 4-5 章）**。所有新特性必须能追溯到某个场景的某条需求；所有需求必须能映射到模块与测试。宪法（八原则）是本规范的上位法，冲突时以宪法为准。

---

## 1. 产品边界

EdgeFlow 是云边协同的边缘计算平台：云端控制面（cloudcore + etcd）+ 边缘数据面（edgecore + metamanager/edged）+ 协议接入层（Modbus / OPC-UA / MQTT mapper）+ 模型与 AI 交付管理（modelrepo/modelrelease）。对标 KubeEdge 子集，自研 MQTT/OPC-UA 协议栈零第三方依赖。

**In scope**: 设备接入与采集、弱网自治、云边控制面、模型交付、设备状态、安全凭证、部署运维、可观测。
**Out of scope（当前版本）**: K8s 完整调度语义、多集群联邦、边缘侧容器运行时管理（edged 仅声明式调谐子集）。

---

## 2. 场景拆解（四大场景）

场景来自《EdgeFlow 解决方案手册》四大业务场景，每个场景拆到「用户故事 + 参与组件 + 特性编号」粒度：

| 场景 | 用户故事（谁/要什么/为什么） | 参与组件 | 特性编号 | 现状 |
|---|---|---|---|---|
| S1 物联网数据采集 | 设备集成工程师把 Modbus/OPC-UA/MQTT 设备接入平台，点位数据经 DeviceTwin 影子→EventBus→云端设备 API 可查；指令经 device-command 下发闭环并留 op-ledger 台账 | mappers/{modbus,opcua,mqtt}、edge/pkg/eventbus、DeviceTwin、cloud 设备 API | F21–F27、F28–F30、F46 | ✅ 主体闭环；OPC-UA 加密通道 🚧（v0.28.0 起分段） |
| S2 弱网自治 | 现场网络抖动/断网时边缘自治运行（本地调谐、SQLite 持久化、自愈），恢复后指数退避重连+消息幂等补投，云端状态语义收敛（Ready/Unknown/Offline） | edgehub（WebSocket 云边通道）、metamanager（ledger/订阅）、edged（5s 调谐/CrashLoopBackOff）、pkg/mqtt（QoS/持久化） | F02–F09、F11–F20、F28–F29、v0240–v0270 系列 | ✅ QoS0/1/2 + TLS/mTLS + in-flight 持久化恢复闭环 |
| S3 模型管理 | AI 平台管理员把模型入库/版本化，灰度发布（白名单/比例/分批/fail-fast/取消/回滚），部署影子台账与镜像漂移核查 | cloud/pkg/{modelrepo,modelrelease}、keadm 升级 | F41–F42、F48 | ✅ 发布生命周期闭环；运行时扫描 🚧 |
| S4 模型应用与交付 | 运维工程师把模型/应用交付到边缘节点（Pod 声明式调谐、健康自愈、镜像漂移重建），性能有基线可依 | edged、podstatus 上报、PERFORMANCE-BASELINE | F11–F18、F38、F40 | ✅ 10 节点 201.3ms 基线已固化 |

**场景→版本溯源规则**: 每个新版本 RELEASE-NOTES 的每条特性必须标注所属场景（S1–S4）与 FR 编号；无法标注的特性需要说明属于哪个新场景并在本文件补场景卡。

---

## 3. 需求重梳

### 3.1 功能需求（按场景分组，验收标准可测）

**S1 数据采集**
- **FR-S1-01 设备模型实例化 mapper**：合法 DeviceModel/Device 配置 → mapper 启动并完成一次点位读取上报。锚点：tests/e2e/device_e2e_test.go。✅
- **FR-S1-02 Modbus TCP 采集**：读点表→上报周期可控。锚点：hack/modbus-e2e。✅
- **FR-S1-03 OPC-UA 接入**：连接/浏览/读点/订阅（monitored item→publish 通知）。锚点：opcua_e2e、opcua_subscription_e2e。✅
- **FR-S1-04 OPC-UA 安全通道**：SecurityPolicy 可选（None 默认 / Basic256Sha256 分段实现），非 None 策略下证书字段与指纹校验强制，不支持策略显式拒绝；OPN 体加密与端到端互通（客户端加密 OPN + sim opt-in 对等处理 + 双侧密钥协商）。锚点：pkg/opcua v0280_security_test.go（17 例）+ v0281_security_test.go（4 例）+ v0281_security_e2e_test.go（2 例）。🚧 v0.28.1 OPN 段完成；MSG 对称覆盖 v0.29.0。
- **FR-S1-05 指令闭环留痕**：device-command 下发→mapper 执行→op-ledger 记账。✅
- **FR-S1-06 mapper 配置文件化**：mapper 参数走配置文件（v0.26.0）。✅

**S2 弱网自治**
- **FR-S2-01 云边通道**：wss + Token 认证（默认 off/401）+ mTLS 可选。锚点：require_token_test、v0250/v0260 TLS 测试。✅
- **FR-S2-02 节点生命周期**：注册 10s ACK、心跳 30s、退避重连 1s→60s、幽灵节点清理、重注册。锚点：v0220_* 测试。✅
- **FR-S2-03 状态语义**：在线/离线双路径判定（90s/180s）→ Ready/Unknown/Offline。✅
- **FR-S2-04 消息可靠投递**：5s×3 幂等去重；EventBus QoS1；QoS2 exactly-once（默认关闭 opt-in）。锚点：v0260_qos2_test（含 per-connection 隔离）。✅
- **FR-S2-05 QoS2 会话恢复**：in-flight 交换落盘（temp+rename 原子写）、重连回放、packetID 计数器快进防碰撞；broker 侧重启恢复（孤儿表）。锚点：v0270_persistence_test（client 8 例 + sim 4 例）。✅ v0.27.0
- **FR-S2-06 断网自治**：断网 60s 本地调谐/持久化继续，恢复后状态收敛。锚点：autonomy_test、multi_node_test。✅
- **FR-S2-07 MQTT 5.0**：版本参数化/原因码/流控（阶段一）、会话语义/共享订阅（阶段二）。❌ 评估完成（docs/MQTT5-EVALUATION.md），分期 v0.29/v0.30 非承诺。

**S3 模型管理**
- **FR-S3-01 模型仓库与版本**：入库/版本/部署影子台账。✅ F41
- **FR-S3-02 灰度发布**：白名单/按比例、分批、fail-fast、取消、回滚。锚点：v0160–v0200 e2e。✅ F42
- **FR-S3-03 完整性**：digest 校验失败拒发、镜像源漂移核查、pause window 冻结、budget 超限暂停。✅
- **FR-S3-04 发布级镜像扫描**：发布流程内完成。✅（运行时扫描 ❌ F48）

**S4 模型应用与交付**
- **FR-S4-01 声明式调谐**：期望状态 5s 周期收敛，健康自愈、CrashLoopBackOff（3 次/30s/60s）、镜像漂移重建。✅
- **FR-S4-02 状态回传**：PodStatus 周期上报（30s 可配）。✅
- **FR-S4-03 部署形态**：keadm（9 子命令）、Helm Chart、多架构镜像（amd64+arm64）、升级回滚（备份/演练/审计）。✅ F36–F39

### 3.2 非功能需求（宪法延伸，全部可验证）

| 编号 | 需求 | 验证方式 |
|---|---|---|
| NFR-01 | 零第三方运行时依赖 | go.mod 无 require；每版全仓构建 |
| NFR-02 | 对外契约冻结 | tests/contract 42 端点全绿 |
| NFR-03 | 向后逐字兼容 | 冻结测试带零改动全绿（v0240→v0270 全套） |
| NFR-04 | 性能基线不回归 | docs/PERFORMANCE-BASELINE.md（10 节点 201.3ms）；新版本对照 |
| NFR-05 | 并发安全 | 受影响包 -race 绿 |
| NFR-06 | 安全默认收敛 | Token 默认 off、证书 0600、审计台账 JSONL |
| NFR-07 | 可观测 | 运行指标 ≥5 项、healthz、MONITORING-ALERTING 告警基线 |

### 3.3 缺口清单 → 版本映射（唯一权威来源，更新需走 ROADMAP）

| 缺口 | 来源 | 归属 | 状态 |
|---|---|---|---|
| OPC-UA OPN 体加密（RSA-OAEP 封 ClientNonce‖legacyBody + 双端签名/验签 + 加密响应；规范 RequestHeader 扩展偏差登记 §29） | FR-S1-04 分段 | ~~v0.28.1~~ | ✅ 已实现（线格式偏差登记 KNOWN-ISSUES §29） |
| OPC-UA MSG 对称加密签名（AES-128-CBC + HMAC-SHA1 全帧覆盖） | FR-S1-04 分段 | v0.29.0 | 规划 |
| MQTT 5.0 阶段一（版本参数化+原因码+流控） | FR-S2-07 | v0.29.0 草案 | 非承诺 |
| MQTT 5.0 阶段二（会话解耦+共享订阅） | FR-S2-07 | v0.30.0 草案 | 非承诺 |
| mapper 自动 Resume 接线（重连后自动回放 in-flight） | FR-S2-05 延伸 | 待排 | 规划 |
| 完整 RBAC（多角色） | F43 | 待排 | 规划中 |
| 设备级身份（当前仅节点级 Token） | F44 | 待排 | 部分实现 |
| Protobuf 通道编码（当前 gzip） | F47 | 待排 | 规划中 |
| 镜像运行时扫描 | F48 | 待排 | 规划中 |
| 性能基线自动刷新机制 | NFR-04 | 待排 | 规划 |

---

## 4. 开发流程（spec-kit SDD 链路）

每个特性（= 每个版本段）走固定链路，产物全部落盘可追溯：

<div class="rich-timeline">
<div class="rich-step"><span class="rich-step-marker">1</span><div class="rich-step-body"><div class="rich-step-title">constitution 宪法校准</div><div class="rich-step-text">新特性开工前确认八原则无冲突；需要修宪先走修订流程</div></div></div>
<div class="rich-step"><span class="rich-step-marker">2</span><div class="rich-step-body"><div class="rich-step-title">specify 规格</div><div class="rich-step-text">specs/NNN-*/spec.md：用户故事（P1/P2/P3 优先级）+ Given/When/Then 验收场景 + FR/NFR 编号</div></div></div>
<div class="rich-step"><span class="rich-step-marker">3</span><div class="rich-step-body"><div class="rich-step-title">clarify 澄清</div><div class="rich-step-text">歧义点结构化问答，结论回写规格；不留口头共识</div></div></div>
<div class="rich-step"><span class="rich-step-marker">4</span><div class="rich-step-body"><div class="rich-step-title">plan 计划 + tasks 任务</div><div class="rich-step-text">技术方案与可执行任务清单；每条任务可独立验证</div></div></div>
<div class="rich-step"><span class="rich-step-marker">5</span><div class="rich-step-body"><div class="rich-step-title">analyze 一致性分析</div><div class="rich-step-text">规格-计划-任务交叉核对（可选但分段交付强烈建议）</div></div></div>
<div class="rich-step"><span class="rich-step-marker">6</span><div class="rich-step-body"><div class="rich-step-title">implement 实现 + 门禁</div><div class="rich-step-text">按宪法 VI 门禁验证；冻结带零改动；新测试 v0NN0_*</div></div></div>
<div class="rich-step"><span class="rich-step-marker">7</span><div class="rich-step-body"><div class="rich-step-title">复核 + 发布</div><div class="rich-step-text">独立复核（骨架先落盘+时间盒）；五件套 + DELIVERY 双格式；提交推送</div></div></div>
</div>

**分支与提交纪律**: main 直接演进（个人仓库节奏）；每版本一个 feat commit（标题含版本号与特性主题）；spec-kit 脚手架与规范文档独立 chore/docs commit。推送 origin/main 前必须全仓 EXIT=0。

---

## 5. 需求生命周期与冻结带

需求状态机：**planned（缺口清单登记）→ in-progress（某版本 spec/specify）→ frozen（实现合入 + 测试入冻结带）**。
冻结带规则：入带测试永不修改；行为演进 = 新测试文件 + 冻结测试保持绿；若冻结测试确实无法表达新契约，说明该变更是 breaking，必须升 minor 版本并在 RELEASE-NOTES 顶部声明。

---

## 6. 附录：spec-kit 命令速查

- `/speckit.constitution` — 修订宪法（已落地，见 .specify/memory/constitution.md）
- `/speckit.specify <特性描述>` — 生成 specs/NNN-*/spec.md
- `/speckit.clarify` / `/speckit.plan` / `/speckit.tasks` / `/speckit.analyze` / `/speckit.implement` — 链路后续步骤
- `/speckit.checklist` — 质量清单（规格完备性/清晰度/一致性）

> 本规范 v1.0.0 由 v0.10.0–v0.27.0 共 20+ 发布轮实践沉淀；下一次修订预期在 v0.28.x 分段计划稳定后（补充 FR-S1-04 全段验收标准）。
