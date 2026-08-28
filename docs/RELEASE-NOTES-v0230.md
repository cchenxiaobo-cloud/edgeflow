# EdgeFlow v0.23.0 发布说明（P2 缺陷修复包：观测面 + 协议健壮性 + 契约统一 + 安全卫生 + 收官勾稽）

- **发布日期**：2026-08-28
- **版本基线**：v0.22.0 → v0.23.0
- **主题**：审计台账 P2 批次（T-12~T-19）八项并行修复 + T-20 全量勾稽收官——观测面补齐、锁序工程化、OPC-UA 报文纵深、API 契约口径统一、keadm/Helm 安全卫生、退避契约测试；台账 71 条全部闭环或登记 KNOWN-ISSUES §23
- **兼容性**：HTTP 端点保持 **42** 不变；默认行为与 v0.22.0 缺省一致（逐项明示见 §五）；**零新依赖**
- **验证**：`go build ./...` ✓ · `go vet ./...` ✓ · 单测包绿 · `helm lint` ✓（终验数字以交付报告为准）

## 一、观测面补齐（T-12，审计 CHN-14/19/20、CLD-13）

- **订阅丢事件计数（CHN-14）**：metamanager Store 广播缓冲满丢弃计数 `droppedEvents`（atomic，进程生命周期累计），访问器 `DroppedEvents()`；edgecore 无独立 metrics 暴露面，经 Grafana 面板文本面板说明采集口径。
- **续约队列水位（CHN-19）**：lease_registry 新增 `RenewQueueDepth()`（瞬时水位 gauge）/`RenewQueueDropped()`（丢弃 counter），接入 `/metrics`（`edgeflow_cloudcore_lease_renew_queue_depth` / `..._dropped_total`，nil Provider 不输出）；既有背压语义（丢弃+降频 Warn+心跳自愈）不变。
- **listFailed 告警阈值（CHN-20）**：`EDGEFLOW_LISTFAILED_ALERT_ROUNDS`（默认 0=完全关闭、行为不变）——连续失败轮次达阈值打 distinct `Edged ALERT:` Warn 并重置，成功轮清零。
- **paused 续租区分日志（CLD-13）**：refreshActiveLocks 失败/接管日志带 `kind=paused/running/unknown`（读 head 状态判定），paused 发布「占锁保持」语义可审计；控制流零改动。
- **Grafana 面板**：`deploy/grafana/edgeflow-overview.json`（uid=edgeflow-overview）——节点/Pod/设备/连接四 stat + CHN-19 水位（80%/90% 阈值色）与丢弃速率 + L12/L12+ + CHN-02 发送缓冲 + HTTP 速率 + 边缘观测说明。

## 二、锁序工程化（T-13，审计 CHN-11/CHN-23）

- lease_registry.go 文件头「并发与锁序约定」权威段：锁清单 + 4 条锁序规则（含「lm 与 reg.mu 永不嵌套；未来必须嵌套仅允许 lm → reg.mu」前瞻约束）+ CHN-11 串行化边界声明。
- LOCK ORDER 断言注释落 6 处结构性位置（lm/contactMu 字段、enqueue、seedAnchored、upsertFromRecord、gcOnce/gcSweepOne），26 个 Lock() 调用点逐一核对结论=全仓无嵌套持有。
- CHN-11 锚点（edged restartRec 读-决策-写跨锁域）两处边界注释：串行化依赖 + 并行化前必须收敛锁域。
- docs/ARCHITECTURE.md 新增 §13「并发与锁序约定」（锁序表 + CHN-11 两处边界 + CLD-13 占锁语义）。

## 三、OPC-UA 报文健壮性（T-16/T-15 部分，审计 PRT-05~23 族）

- **B 路 pkg/opcua、pkg/opcuasim（10 条修复 + 2 条登记不修，14 个新测试）**：
  - PRT-06 randomNonce 失败即报错（会话建立中止，无时间戳降级）；
  - PRT-07 防重放（seq==0 与 seq≤lastPubSeq 拒绝不 ack）+ 跳帧探针 + gap 检测；
  - PRT-08 OPN 响应三重校验（策略 URI/None 证书字段/寿命>0）；
  - PRT-09 ReceiveBufferSize 上限 16MB（超限握手失败，台账验收硬指标）；
  - PRT-10 Subscribe 失败路径 5 处 deleteSubBestEffort（防孤儿订阅泄漏）；
  - PRT-13 通知解码负长度拒绝（notificationData/monitoredItems/StatusChange）；
  - PRT-15 EndpointUrl 4096 上限；PRT-16 ChunkAbort 哨兵不断连（'C' 照旧拒绝）；
  - PRT-17 roundTrip 超时顺带清 pending；PRT-23 sim Stop wg+幂等+悬挂 Publish select 化；
  - PRT-11/12 登记不修（建议级，理由见 KNOWN-ISSUES §23.2）。
- **B 路接手 mappers 域（3 条，9 个新测试，-race 全绿）**：
  - PRT-19 rebuildSubscription：锁内快照换出 + 锁外清理/重连 + winner check（旧实现持锁拨号最坏挂 10s+15s 全部消除）；
  - PRT-20 Stop：锁内翻转+快照置 nil、Close 移锁外；循环 PubAck 锁内快照（nil 跳过/旧连接报错吞）；
  - PRT-21 modbus withConn：整体预算门 2×timeout，耗尽放弃重试并明示。
- **主线修复（B 路新测试暴露的基线缺陷）**：`CreateMonitoredItemsResponse.encodeUA` 原实现无视输入硬编码 0（statusCode/itemId 恒 Good/0），服务端侧永远无法表达 Bad 结果——改为逐项编码结构体实际值（sim 侧全 Good/0 场景编码字节不变，既有测试零影响）。

## 四、API 契约统一与输入卫生（T-14/T-15，审计 CLD-07/08/10/11/12、PRT-24、CHN-15/16、SEC-06）

- **summary 恒现（CLD-07）**：releaseResponse 去 omitempty + summaryOfRelease helper，8 个发布端点 list/get 口径收敛一致（向后兼容加字段）。
- **releases 全局列表信封化（CLD-08）**：`GET /api/v1/releases` 裸 items → `{kind,apiVersion,items}`（K8s List 风格；**破坏性顶层形态变更**，客户端改读 items，API-COMPATIBILITY 登记）。
- **空 body 判定修正（CLD-10）**：`err.Error()!="EOF"` → `errors.Is(err, io.EOF)`。
- **retry 422 文案细分（CLD-12）**：按 orig.Status 与 releaseID 细分，状态码/链序不变。
- **mirror 探活输入卫生（CLD-11）**：CheckMirror 入口预检 + manifestURL 逐段 PathEscape + tokenURL url.Values 编码（SSRF 面三重收敛）；预检对无 '/' 的 Docker Hub 短形式（如 mnist:v1）按宽松 helper 放行（与既有语义兼容），带路径形态走完整 ValidateMirror。
- **CRD 校验标记（PRT-24）**：3 个 CRD yaml 共 11 处 minLength/pattern（nodeID/deviceModelRef/propertyName 等关键 string 字段），既有 required/enum 全保留；kubectl apply 空/非法值从静默通过改为准入拒绝。
- **协议信封校验收紧（CHN-15）**：Validate 新增 Version 格式校验（`^v[0-9]+$`，畸形信封 Decode 即拒）；Timestamp 显式登记「不校验」策略（注释头三点理由，未来随 v2 信封升级裁决）。
- **发布 ID rand 失败即报错（CHN-16，cmd/cloudcore 落点）**：newReleaseID 改 `(string, error)`，删除时间戳兑底；创建/retry 失败路径 500 化（原静默兑底 202）；可注入熵源 + 失败路径测试。
- **上报日志消毒（SEC-06，cmd/cloudcore 落点）**：sanitizeEdgeField 剥 CRLF/控制字符，PodStatus 未知 phase 告警日志应用；存储值不变，仅日志面。
- **docs**：API-SPEC §7.12 错误码映射表（与 modelError() 逐条对齐）+ 响应形态变更标记；API-COMPATIBILITY v0.23.0 兼容性增量小节。

## 五、安全卫生与测试增强（T-17/T-18/T-19，审计 SEC-03/05/07/08、CHN-08/17）

- **keadm token 卫生（SEC-03/T-17）**：env 文件 0600 核实钉死（测试防回归）；新增 `--token-file`（join+batch，file 优先，向后兼容）；README token 脱敏（前 4 字符+`****`）；KEADM.md 新增安全传递段（umask 077/进程替换技巧）。
- **CheckOrigin 白名单（SEC-05/T-18）**：`WithAllowedOrigins` Option + env `EDGEFLOW_CLOUDCORE_ALLOWED_ORIGINS`（逗号分隔）；空=全放行（行为不变）；非空时「携带 Origin 的握手必须精确命中」，缺失 Origin 放行（边缘 Go 客户端不发 Origin，主路径不受影响——修正了「缺 Origin 即拒」会打断全部边缘接入的语义缺陷）。
- **Helm etcd 密码 Secret 化（SEC-07/T-18）**：values 新增 `external.auth.existingSecret/secretKey`，模板 secretKeyRef 注入；existingSecret 与 password 互斥 fail 渲染期守卫；不设时走原明文路径（向后兼容）；`password` 注释标 deprecated；helm lint 0 failed。
- **--cloudcore-port 校验（SEC-08/T-18）**：1-65535 十进制校验（join+batch），非法值 exitUsage 拒绝（原生成异常产物文件）。
- **退避契约测试（CHN-17/T-19）**：nextBackoff 边界表驱动 3 测试 16 子例（封顶序列/钳制/零起点行为文档化），实现零改动。
- **CHN-08 裁决（T-19）**：文档标注路线——DEPLOYMENT.md §2.5 新增 LB/代理读超时要求（必须 >90s 心跳超时，推荐 150s≈1.5×），KNOWN-ISSUES §23 登记裁决理由（慢链路误杀面 > 收益，待 RTT 基线后实施服务端对称读超时）；服务端实现零改动。
- **cloudhub 日志消毒（SEC-06，cloudhub 侧主落点）**：`sanitizeLogField` 剥 CRLF/控制字符，四个审计锚点日志（注册成功/拒绝/PodStatus/Ack）全部应用。
- **配套测试（D 路）**：keadm 6 测试（token hygiene/file priority/port 表驱动 13 例/batch 透传）、cloudhub checkOrigin 4 子例 + sanitize 8 子例（含后缀伪装与大小写变体拒绝）。
- **文档批**：DEPLOYMENT.md §4.2 可观测性部署（Grafana 面板导入 + EDGEFLOW_LISTFAILED_ALERT_ROUNDS + 新增 metrics 两指标）；KEADM.md §2.1；KNOWN-ISSUES §23 全量登记。

## 六、主线收口（T-20）

- **CHN-13（eventbus 确认等待有界化）**：Publish/Subscribe/Unsubscribe 三处 `token.Wait()` → `WaitTimeout(5s)+超时报错`（`DefaultWaitTimeout`）——半死 broker（TCP 活着不回 PUBACK/SUBACK）下发布方不再阻塞至 paho 内部写超时（约 10~20s）。测试：原生 TCP mock 半死 broker（CONNACK 正常、业务报文沉默）验证三原语有界报错 + mosquitto 快乐路径护栏。**勾稽修正**：本条在勾稽草稿中曾误标 ✅ v0.22.0（§22 实无对应行），回源核实后改判本轮主线闭环。
- **CHN-16 主线裁决（pkg/protocol msg.ID）**：`newID()` 改 `(string, error)`，删除「纳秒时间戳+计数器」降级路径——msg.ID 承载 dedup_keys 去重与请求-响应关联语义，静默降级为低熵可预测 ID 放大碰撞与伪造面；与 OPC-UA nonce（B 路）、发布 ID（C 路）统一「rand 失败即报错」。`NewMessage` 已有 error 签名零调用方结构改动；原 id_test.go 钉住降级行为，按 v0.22.0「增长需主线知会」先例删除并以 v0230_id_test.go 承接（失败即报错 + 正常路径唯一性 + NewMessage 透传三用例）。
- **台账 71 条全量勾稽**：✅ 闭环 / 📌 登记不修（KNOWN-ISSUES §23 逐条理由）/ 审计期撤销（CHN-04），勾稽总表见交付报告。
- **编译产物卫生**：.gitignore 补 `/cloudcore /edgecore /keadm`（`go build ./...` 在仓库根丢弃的 main 包产物）。

## 七、已知限制与登记（详见 docs/KNOWN-ISSUES.md §23）

- 登记不修 14 条（台账 12 条 + PRT-11/12）：多故障叠加/正常客户端不触发/成本收益低/审计原文判可接受四类，逐条附重估触发条件。
- 域外残余票：守卫注释口径（R-1）、cloudhub 个别日志点复检（R-2）、SEC-05 通配扩展位（R-3）、batch token-file 语义差异（R-4）等，见 §23.3。

## 八、行为变化明示（全量，默认行为逐项不变）

**破坏性（1 项）**：`GET /api/v1/releases` 顶层裸 items → ReleaseList 信封（CLD-08，客户端改读 items）。

**向后兼容加字段/加开关（opt-in，不设即与 v0.22.0 逐字节一致）**：summary 恒现（8 端点）；CRD 准入收紧（PRT-24，仅约束新写入）；CheckOrigin 白名单（SEC-05）；Helm existingSecret（SEC-07）；keadm --token-file（SEC-03）；EDGEFLOW_LISTFAILED_ALERT_ROUNDS（CHN-20，默认 0 关闭）。

**错误面收敛（正常路径零变化）**：发布 ID/msg.ID/OPC-UA nonce rand 失败从静默降级改显式报错（CHN-16）；keadm 非法端口改 exitUsage（SEC-08）；协议信封 Version 格式校验（CHN-15）；OPC-UA 恶意报文各拒绝路径（PRT-08/09/13/15）；重试 422 文案细分（CLD-12）；mirror 探活预检拒绝注入面（CLD-11）。

**并发/锁面（-race 全绿佐证）**：mappers rebuild 锁外重连、Stop 快照置 nil、PubAck 快照（PRT-19/20）；modbus withConn 预算门（PRT-21）；eventbus 确认等待 5s 有界化（CHN-13，正常 broker 毫秒级到达不受影响）。

**日志面**：cloudhub 四锚点 + cloudcore 回调日志字段消毒（SEC-06，存储值不变）；keadm README token 脱敏（SEC-03）；paused 续租日志带 kind 标记（CLD-13）。

**纯文档/纯测试**：CHN-08 LB 超时标注、CHN-17 退避边界测试、API-SPEC §7.12、API-COMPATIBILITY v0.23.0、DEPLOYMENT §2.5/§4.2、KEADM §2.1、KNOWN-ISSUES §23。
