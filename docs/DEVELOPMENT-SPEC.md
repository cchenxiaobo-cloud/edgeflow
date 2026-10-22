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
- **FR-S1-04 OPC-UA 安全通道**：SecurityPolicy 可选（None 默认 / Basic256Sha256 分段实现），非 None 策略下证书字段与指纹校验强制，不支持策略显式拒绝；OPN 体加密与端到端互通（客户端加密 OPN + sim opt-in 对等处理 + 双侧密钥协商）。锚点：pkg/opcua v0280_security_test.go（17 例）+ v0281_security_test.go（4 例）+ v0290_security_test.go（5 例）+ v0290_security_e2e_test.go（2 例）。✅ v0.29.0 全链路完成（OPN 体加密 + MSG/CLO 对称覆盖 + 显式 Renew）。
- **FR-S1-05 指令闭环留痕**：device-command 下发→mapper 执行→op-ledger 记账。✅
- **FR-S1-06 mapper 配置文件化**：mapper 参数走配置文件（v0.26.0）。✅
- **FR-S2-07 MQTT 5.0 阶段二（会话解耦 + 共享订阅）**：v5 Clean Start/Session Expiry 会话解耦（断连保留订阅表 + 离线 QoS1、重连 Session Present 恢复、SE 惰性过期）、$share/{group}/{filter} 组内 round-robin（离线成员不暂存）、client PersistentSession opt-in 与 SessionPresent()；3.1.1 路径冻结（整体退出会话状态机）。锚点：pkg/mqtt v0320_test（4 例）+ pkg/mqttsim v0320_test（9 例）+ v0320_e2e_test（2 例）。✅ v0.32.0
- **FR-S1-07 视频流管理与边缘推理对接（阶段一）**：帧源抽象（合成源内置，RTSP 实源阶段二）→ latest-wins 背压槽 → HTTP JSON 推理服务 → 数字指标面（影子上报）+ 结果台账留痕/事件上行；mapper 配置文件化与 stream 指令启停。锚点：pkg/video v0310_test（7 例）+ mappers/video v0310_test（5 例，含装配级 e2e）。✅ v0.31.0

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
| OPC-UA MSG 对称加密签名（AES-128-CBC + HMAC-SHA1 全帧覆盖，含 CLO）+ 显式 Renew | FR-S1-04 分段 | ~~v0.29.0~~ | ✅ 已实现（Renew 形状指纹/KeepAlive 约定登记 KNOWN-ISSUES §30） |
| MQTT 5.0 阶段一（版本参数化+原因码+流控） | FR-S2-07 | ~~v0.30.0~~ | ✅ 已实现（v5 属性区/会话解耦边界登记 KNOWN-ISSUES §31；默认 3.1.1 逐字节冻结） |
| MQTT 5.0 阶段二（会话解耦+共享订阅） | FR-S2-07 | ~~v0.32.0~~ | ✅ 已实现（3.1.1 持久会话不支持/下行 QoS1 重发状态机归阶段三，KNOWN-ISSUES §33） |
| 视频流管理阶段二（实源接入：MJPEG over HTTP + 外部进程桥 + streamOn 降级） | FR-S1-07 分段 | ✅ v0.34.0 | 阶段一 ✅ v0.31.0；原生 RTSP 协议栈/GB28181 信令、云端 VideoStream 管理面（契约扩容）、GPU 运行时仍待排（spec 0007 边界 + KI §35） |
| MQTT 5.0 阶段四（保留消息 Retain 面 + RH 语义） | FR-S2-07 分段 | ✅ v0.35.0 | 阶段一 ✅ v0.30.0 / 二 ✅ v0.32.0 / 三 ✅ v0.33.0；重发定时器、alias 出站方向、共享订阅 granted-QoS1、3.1.1 持久会话仍待排（KI §36） |
| MQTT 5.0 阶段五（遗嘱消息 Will 面） | FR-S2-07 分段 | ✅ v0.36.0 | 阶段一 ✅ v0.30.0 / 二 ✅ v0.32.0 / 三 ✅ v0.33.0 / 四 ✅ v0.35.0；will 属性透传/会话过期取小/接管 delay 语义简化登记 KI §37；重发定时器、alias 出站方向、共享订阅 granted-QoS1、3.1.1 持久会话仍待排 |
| mapper 自动 Resume 接线（重连后自动回放 in-flight） | FR-S2-05 延伸 | 待排 | 规划 |
| 完整 RBAC（多角色） | F43 | 待排 | 规划中 |
| 设备级身份（当前仅节点级 Token） | F44 | 待排 | 部分实现 |
| Protobuf 通道编码（当前 gzip） | F47 | 待排 | 规划中 |
| 镜像运行时扫描 | F48 | 待排 | 规划中 |
| 性能基线自动刷新机制 | NFR-04 | 待排 | 规划 |
| 规则引擎与实时处理（规则包下发/评估/事件上行）+ 数据治理最小面 | 发展规划 v0.37（G15/G14） | ✅ v0.37.0 | 边界（动作仅 event/无 cleared 事件/事件 ring 内存/debounce 严格相等）登记 KI §38；后续：指令联动动作、事件持久化与多副本聚合、可视化编排评估 |
| 边缘时序存储（轻量时序库：写入/查询/聚合/保留/降采样/水位背压） | 发展规划 v0.38（G17） | ✅ v0.38.0 | 边界（段粒度保留/断电窗口≤flush 周期/rollup 无 first/无压缩）登记 KI §39；后续：压缩、标签过滤、云端查询面（v0.39 协同）、两级降采样 |
| 分级上送与补传（边缘持久队列/分级策略/补传 worker/云端幂等与在途缓冲/可视化） | 发展规划 v0.39（G16） | ✅ v0.39.0 | 边界（极端写达窗口/去重窗口容量/丢弃序与挤占/ring 内存窗口/etcd 降级/64KB 上限）登记 KI §40；后续：精确一次（云端逐条确认）、视频告警链路复用补传设施、队列压缩 |
| 告警事件全链 + 设定值通道（告警模型/边缘聚合/云侧告警中心/工单留位/设定值审批与反馈闭环） | 发展规划 v0.40（G26） | ✅ v0.40.0 | 边界（无自动 cleared/联动与工单留位/控制类须评审/节流延迟/审批无快照校验）登记 KI §41；后续：cleared 恢复源、声光/消息联动实现、ITSM 对接、审批快照校验 |
| 采集扩展与性能（Modbus RTU 通道/REST 轮询采集器/高频波形通道/2000 点·1s 压测） | 发展规划 v0.41（G19+G13） | ✅ v0.41.0 | 边界（真串口待硬件/RTU-over-TCP 非标准/矩形窗泄漏/包络谱最小实现/仿真速率出块）登记 KI §42；后续：真串口 transport、REST 多端点聚合、推送接收形态、加窗与 Hilbert 包络 |
| 视频接入增强（RTSP 原生子集/RTSPSource/源健康管理/C6 修复） | 发展规划 v0.42（G20） | ✅ v0.42.0 | 边界（仅 TCP/无 SRTP/无 H.265/SDP 最小面/模拟服务端为测试面/SIP 另立）登记 KI §43；后续：UDP 传输、SRTP、H.265、GB28181 SIP 立项、rtsp 源 e2e 装配进 edgecore |
| 视频管理面（VideoStream 模型/媒资分片上传与断网补传/快照与片段回放/契约扩容 61→69） | 发展规划 v0.43（G21） | ✅ v0.43.0 | 边界（MJPEG 无转码/片段级留存/至少一次/告警关联留位/简易水位）登记 KI §44；后续：转码与 MP4、v0.44 流媒体分发（G22）、媒资 GC 与保留策略 |
| 流媒体分发子集 + 告警片段服务（FLV 透传封装/HTTP-FLV+WS-FLV 分发/告警片段检索/metrics 流式能力） | 发展规划 v0.44（G22） | ✅ v0.44.0 | 边界（仅 H.264 视频透传/CT=0/生产帧源另立/HLS 评估登记/流端点免 Token）登记 KI §45；后续：AAC 音频、RTMP 推流面、HLS 立项、签名 URL |
| 模型面扩展 + 困难样本回传 + 推理运行时（Modality 元数据/场景关联/困难样本采集回传检索/加速卡探测上报/INFERENCE-GUIDE） | 发展规划 v0.45（G23+G25+G24） | ✅ v0.45.0 | 边界（仅 JPEG 帧/弱关联/env 探测/训练留位/发布不校验加速匹配）登记 KI §46；后续：波形段样本、真实枚举器、加速匹配校验、训练接入 |
| RBAC 与隔离 + 镜像凭证（三角色+scope.ns/授权中间件/角色管理面/凭证哈希+AES-GCM/边缘 docker login/运行时扫描评估） | 发展规划 v0.46（G27+G28） | ✅ v0.46.0 | 边界（scope 仅命名空间资源/后写不热加载/运行时扫描不内置/operator 无删除）登记 KI §47；后续：热重载、KMS、LDAP-OIDC、扫描立项 |
| 应用目录与分发增强（应用目录/版本目录/应用级灰度分批暂停恢复取消回滚/离线安装包缓存/脚本化应用生命周期 opt-in scriptrunner） | 发展规划 v0.47（G08+G09+G10+G07，spec 0020） | ✅ v0.47.0 | 边界（状态机独立实现/扫描周期即批间节奏/回滚单版/尽力交付不自动回滚/包内联 ≤8MB/伪 Pod 仅生命周期态）登记 KI §48；后续：部署计划持久化、应用市场包签名、脚本应用资源指标 |
| 日志链路（边缘采集 opt-in 文件 tail+Emit/LogSync 批量上行复用补传队列/云端环形存储+可选 JSONL/检索 API 5 端点） | 发展规划 v0.48（G32+G11，spec 0021） | ✅ v0.48.0 | 边界（ts 取采集时刻/offset 不持久化/rotate 窗口重复丢行/保留=环形容量）登记 KI §49；后续：持久 offset、按天清理、外部索引、声明式 env 注入 |
| 远程运维终端（受控命令会话/边缘白名单执行器 opt-in 不走 shell/命令级审计三层/RBAC 联动） | 发展规划 v0.49（G31+G11，spec 0022） | ✅ v0.49.0 | 边界（同步 exec 受 WriteTimeout 15s/会话易失/边审计不上云/命令不补传）登记 KI §50；后续：PTY、审批双签、异步大超时、副账上云 |

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
| v0.50.0 | 2026-10-20 | 区域层与标签治理（G03+G04，spec 0023）：regionstore/tagstore（JSON 快照）+ 治理 API 7 端点（107→114）+ e2e 六段；RBAC 对齐 v0.46（DELETE=admin）；纯云端管理面消息 19 不变 | spec 0023 / RN-v0500 / KI §51 |
| v0.51.0 | 2026-10-21 | 区域协同层（G01，spec 0024）：hierarchyview 聚合引擎 + 协同 API 5 端点（114→119，无 DELETE 面）+ 真节点 e2e 六段 + DR-ASSESSMENT 灾备评估（GB/T 20988 对标）；纯云端管理面消息 19 不变 | spec 0024 / RN-v0510 / DR-ASSESSMENT / KI §52 |
| v0.52.0 | 2026-10-21 | 规模·韧性·基线（G05 承接，spec 0025）：alarmstore.CountByNodeState + hierarchyview AlarmCountSource（聚合无截断，KI §52#1/#4 修复）+ drstore 备份管理面（API 3 端点 119→122）+ DR-RUNBOOK/CONFIG-BASELINE + bench-baseline 机制；纯云端管理面消息 19 不变 | spec 0025 / RN-v0520 / DR-RUNBOOK / CONFIG-BASELINE / KI §53 |
| v0.53.0 | 2026-10-21 | 数据转发出口（G18 起点，spec 0026）：forwardstore webhook 转发（alarmstore 迁移挂点 + HMAC 签名 + 3 次重试 + 送达台账环形 100）+ 规则 API 6 端点（122→128，首个 DELETE=admin 面，脱敏视图）；纯云端管理面消息 19 不变 | spec 0026 / RN-v0530 / KI §54 |
| v0.54.0 | 2026-10-21 | 三场景端到端验收（G29，spec 0027）：三条场景级 e2e（真实进程+RBAC）+ docs/SCENARIO-E2E.md；R1 复核处置（P2×1+P3×4：hmacHex/台账 ID 随机化/自检告警/钳位措辞，P3-4 写放大登记）；零 API 变更契约 128 维持 | spec 0027 / RN-v0540 / SCENARIO-E2E / KI §55 |
| v0.55.0 | 2026-10-21 | MQTT 出站重发（持续项 B1/B2，spec 0028）：pkg/mqtt Options.ResendInterval/MaxResends + resendLoop 三路径（QoS1/2-phase1 DUP=1、phase2 PUBREL）+ errAckTimeout 哨兵 + SUBACK 语义不变；B3 出站 alias 销项（v0.33 已交付）；库能力零 API 变更契约 128 维持 | spec 0028 / RN-v0550 / KI §56 |
| v0.56.0 | 2026-10-21 | 审计台账查询面（spec 0029）：audit.Ledger Query/Stats（文件扫描过滤+降序分页+聚合桶）+ API 2 端点（128→130，viewer+，无 DELETE）；记录面零改动；纯云端管理面消息 19 不变 | spec 0029 / RN-v0560 / KI §57 |
| v0.57.0 | 2026-10-22 | MQTT 出站修复与共享订阅（持续项 B4，spec 0030）：QoS2 phase2 专用等待循环（迟到 PUBREC 跳过+超时重发 PUBREL，R2 P3-1 处置）+ 共享订阅 granted-QoS1 下行（v5 面，降级锚保留）+ US-4 断言增强（R2 P3-3）；库能力零 API 变更契约 130 维持 | spec 0030 / RN-v0570 / KI §58 |
| v0.58.0 | 2026-10-22 | 告警静默窗口（spec 0031）：mutestore（窗口≤30 天/总数≤200/不可变/JSON 快照）+ hierarchy 活跃口径排除（MuteSource 可选接口）+ API 3 端点（130→133，DELETE=admin）；纯云端管理面消息 19 不变 | spec 0031 / RN-v0580 / KI §59 |
| v0.59.0 | 2026-10-22 | 转发出口增强（spec 0032）：同步测试投递端点（133→134，KI §54#2 处置）+ sinkType webhook|mqtt（pkg/mqtt 短连接 QoS1，topic 可配）+ Rule schema 只增字段向后兼容；纯云端管理面消息 19 不变 | spec 0032 / RN-v0590 / KI §60 |
| v0.60.0 | 2026-10-22 | OPC-UA 安全收口（持续项 D4/D5，spec 0033）：TokenID 严格比对（openIncoming 帧内校验，不匹配显式拒绝）+ deriveKeys PSHA 规范化（HMAC-SHA256，证书退出派生，golden 锚）；库能力零 API 变更契约 134 维持 | spec 0033 / RN-v0600 / KI §61 |
| v0.61.0 | 2026-10-22 | 诊断聚合面（spec 0034）：GET /api/v1/diagnostics/summary 系统健康一屏（节点/告警/区域/静默/转发/etcd 形态/运行时长）+ audit 参数非法值显式 400（R2 P3-2 处置）；纯云端管理面消息 19 不变 | spec 0034 / RN-v0610 / KI §62 |
| v0.62.0 | 2026-10-22 | 静默联动通知抑制（spec 0035）：forwardstore MuteSource（Dispatch 节点+级别静默判定，命中不投递不记台账）+ mutestore severity 维度（仅转发过滤，层级活跃口径节点级）+ mutes POST schema 扩展；零新增端点 135 维持 | spec 0035 / RN-v0620 / KI §66 |
| v0.63.0 | 2026-10-22 | 告警 muted 标记与静默统计（spec 0036）：/api/v1/alarms 列表每条增 muted 只读标记（匿名嵌入保全字段）+ GET /api/v1/alerts/mutes/stats；纯云端管理面消息 19 不变 | spec 0036 / RN-v0630 |
| v0.64.0 | 2026-10-22 | MQTT sink 认证与 TLS（spec 0037）：Rule 增 mqttUsername/mqttPassword/mqttTLS（GET 面脱敏）+ 拨号构造注入（InsecureSkipVerify 自签常态）；库能力零新增端点 134 维持 | spec 0037 / RN-v0640 / KI §66 |
| v0.65.0 | 2026-10-22 | 审计导出与转发规则导出/导入（spec 0038）：GET /api/v1/audit/entries/export（JSONL 流式）+ GET /api/v1/forwards/export（脱敏 JSON，admin）+ POST /api/v1/forwards/import（批量导入去重跳过）；纯云端管理面消息 19 不变 | spec 0038 / RN-v0650 / KI §67 |
