# EdgeFlow v0.22.0 发布说明（P1 缺陷修复包：一致性 + 幂等 + 状态机 + 契约 + 吊销链）

- **发布日期**：2026-08-28
- **版本基线**：v0.21.0 → v0.22.0
- **主题**：把审计台账的 7 项 P1 缺陷（T-05~T-11）全部闭环——云边通道内存/风暴防线、边缘指令幂等落盘、发布状态机契约对齐、API 归属校验收口、吊销链可配收紧
- **兼容性**：HTTP 端点保持 **42** 不变；全部新开关默认关闭/默认值与 v0.21.0 缺省行为一致；升级零迁移；老边缘零动作；**零新依赖**
- **验证**：`go build ./...` ✓ · `go vet ./...` ✓ · 全部单测包绿 · `tests/contract` 全绿（含 42 端点守卫）· `tests/e2e` 全量绿（190s）

## 一、核心修复

### 1. 云边通道一致性与容量防线（T-05 / T-08，审计 CHN-07 / CHN-02）

#### T-05：换 ID 重注册事件顺序验收（cloudhub + registry）
- 旧 nodeID 无主场景：先补发 `OnNodeDisconnected(旧ID)` 再 `OnNodeRegistered(新ID)`（HEAD 既有逻辑，本轮以测试钉死）。
- 新增 `cloud/pkg/cloudhub/v0220_reregister_test.go`：事件顺序断言（disconnect(old)→register(new)）+ 注册表无幽灵 Ready 节点断言。
- 新增 `cloud/pkg/registry/v0220_ghost_node_test.go`：适配器层重注册无幽灵 + 漏断开缺陷签名测试（import 环约束下与 cloudhub 测试分域）。

#### T-08：慢客户端防线 + 注册风暴退避（cloudhub）
- **ack 前置（CHN-07）**：`handleRegister` 改为先回 RegisterAck 再执行登记类事件回调——云端下游故障窗口内边缘立即可进入心跳循环，消除「注册失败→重连→再注册」风暴自我放大面。事件顺序约束（T-05）不变。
- **发送缓冲字节计量（CHN-02）**：单连接发送缓冲新增字节配额，默认 **64MiB**（`cloudhub.SendQuotaBytesDefault`）；入队前检查，超配额丢弃新消息并关闭慢客户端连接（与消息数满同策略）；CAS 计量与写循环扣减并发安全；配额关闭（<=0）退化为仅消息数上限（v0.21.0 行为）。
  - env：`EDGEFLOW_CLOUDHUB_SEND_QUOTA_BYTES`（字节数，非法回退默认，显式 <=0 关闭计量）。
- **广播内存可观测**：新 gauge `edgeflow_cloudcore_hub_send_buffer_bytes`（全部活跃连接发送缓冲在途字节合计），广播 N 节点内存峰值≈该值，逼近配额即存在慢客户端。
- **广播内存闸门（可选）**：`cloudhub.WithBroadcastMemLimit`（默认 0=不启用，行为与 v0.21.0 一致）。

### 2. 边缘下行指令幂等落盘（T-07 / T-10，审计 CHN-03 / CHN-05）

#### T-07：下行去重键 SQLite 持久化（metamanager + edgehub + edgecore）
- 新增 `edge/pkg/metamanager/dedup.go`：`dedup_keys(msg_id PRIMARY KEY, expires_at)` 表；TTL 24h、容量上限 10000 条（约 1MB）、按过期时间升序批量淘汰最旧（500 条/次）；三重清理时机（启动 / 每 200 次写异步触发 / 后台 1h 协程）。
- edgehub 接线：`Options.DedupStore`——内存未命中回源 SQLite，双写（SQLite 权威 + 内存热身）；**未装配时自动退化为纯内存去重（v0.21.0 行为）**。
- cmd/edgecore 装配：初始化失败告警降级不阻断启动；维护协程复用 main 既有优雅退出。
- 效果：边缘重启后云端重试同 msgID 的下行指令不再重复执行（真实 WS 链路集成测试覆盖）。

#### T-10：容器迁移复核 + 调谐容错（edged）
- **迁移后 Inspect 复核**：Index<0 旧命名容器迁移（EnsureStopped 成功后）追加 Inspect 复核——外部 docker 干预（容器被重建拉起）导致「删除成功但实际仍在」时记 Unknown + 下轮重试，不误标迁移完成。
- 澄清（审计口径修正）：`DockerRuntime.List()` 每次即时 exec docker CLI，**不存在 90s 固化快照**（90s 是 Absent 终态保留窗口 `DefaultRemovedRetention`）；单轮调谐共用本轮即时 List 属声明式调谐标准做法。
- 外部 docker 干预容错单测 7 用例（迁移复核 3 + 干预容错 4）：外部删容器幂等补齐 / 外部停止自愈 / List 失败不误杀 / 创建失败可诊断自愈等。

### 3. 发布状态机契约对齐（T-06，审计 CLD-01/02/04/06）

- **终态写点接入权威状态机断言**：`setTerminal`（succeeded/failed 统一漏斗）、回滚完成、回滚中止（succeeded/canceled 跨终态收敛豁免）、failure budget autopause 共 4 类写点全部接入 `assertReleaseTransition`；违例拒绝落库 + 观测上报（`reportIllegalTransition`）。
- **digest 复核失败同源预算**：镜像 digest 复核失败写 head 事件时间线，并与批次失败同源计入 failureBudget（预算耗尽自动暂停，TestV0220DigestFailure_SameSourceBudgetAutoPause 钉死）。
- **failFast 与 head 参数解耦**：批次中止判定只由调谐决策产生，失败返回不依赖/不改 head 参数。
- 新增 `v0220_contract_test.go` 8 用例（状态机对拍/终态写点/事件时间线/digest/failFast/回滚 done+abort）。

### 4. API 归属校验收口（T-09，审计 CLD-06 + canary 语义）

- **release 子资源跨模型引用一律 404**：新增 `ownedRelease` helper（模型 404 先行 → release 404 → 归属不符 404 同语义同响应体），接入 7 个端点（GET 详情/digest、cancel/pause/resume/rollback、PATCH）；snapshot/retry/DELETE 三个端点既有内联校验同语义。全部 10 个 release 子资源端点行为收敛一致，防跨模型 id 枚举。
- **canary 独占语义登记**：API-SPEC §7.11 登记同模型单在途发布 guard（409 + releaseID 回指）语义；§4 状态码表补充 404 行为说明。**§1.1 端点总览零触碰**（42 端点契约守卫绿）。
- 新增 `tests/contract/v0220_release_ownership_test.go` 跨模型 404 契约测试。

### 5. 吊销链可配收紧（T-11，审计 SEC-04）

- **CRL 缺失可配**：`certs.LoadTLSConfigWithOptions` + `CRLVerifyOptions.FailOnMissing`——`EDGEFLOW_CLOUDCORE_CRL_STRICT=on` 时 CRL 产物缺失 mTLS 握手拒绝（fail-closed）；**默认 off 放行（v0.21.0 行为）**。
- **OCSP 新鲜度可配**：`FreshnessPolicyFromEnv`——`EDGEFLOW_CLOUDCORE_OCSP_FRESH=on` 时 nextUpdate 过期拒绝；默认 off。
- main.go TLS 装配改走 Options 路径（缺省行为逐字节一致），启动日志登记当前收紧面。
- 新增 `pkg/certs/v0220_revocation_test.go`（既有测试零改动）。

## 二、行为变化明示（审计纪律：既有测试修复逐条登记）

1. **既有 e2e 辅助函数配套修复**（T-09 行为修复的必然结果）：`v160waitReleaseStatus` 原硬编码 `models/mnist/` 轮询 URL，v0170/v0180 测试靠 T-09 修复前的「不校验归属」缺陷才能通过。修复后新增 `v160waitReleaseModelStatus`（显式模型名）供 v0170/v0180 共 3 处调用切换；v0160 既有调用与 helper 签名零改动。
2. **`conn.close()` nil-ws 防御**（1 处）：单测直接构造 conn 视图验证配额语义所需，生产语义不变。
3. 除上述外，**全部既有测试零改动、全绿**；缺省外部行为与 v0.21.0 一致（全部新开关默认关闭/默认值兼容）。

## 三、新 env 开关一览（全部默认关闭/默认值兼容）

| env | 默认 | 语义 |
|---|---|---|
| `EDGEFLOW_CLOUDHUB_SEND_QUOTA_BYTES` | 67108864（64MiB） | 单连接发送缓冲字节配额；<=0 关闭计量 |
| `EDGEFLOW_CLOUDCORE_CRL_STRICT` | off | on 时 CRL 缺失 → mTLS 握手拒绝（fail-closed） |
| `EDGEFLOW_CLOUDCORE_OCSP_FRESH` | off | on 时 OCSP nextUpdate 过期拒绝 |

## 四、升级与回滚

- 升级：直接替换二进制/镜像；无 schema 迁移（dedup_keys 表由 edgecore 首次启动自建）；老边缘无需任何动作（ack 前置/字节计量均为云端侧行为）。
- 回滚：直接回退 v0.21.0 二进制即可（新增 SQLite 表对旧版本无影响；全部开关关闭时行为逐字节一致）。

## 五、部署建议（吊销链）

- 启用 CRL：`keadm cert revoke` 生成 crl.pem 后，设置 `EDGEFLOW_CLOUDCORE_CRL_STRICT=on` 实现缺失即拒（fail-closed）。
- 启用 OCSP 新鲜度：对接 OCSP 响应器后设置 `EDGEFLOW_CLOUDCORE_OCSP_FRESH=on`。
- 两者均建议先在灰度环境验证证书链完整再开启，避免产线握手误拒。
