# EdgeFlow API 兼容矩阵（WBS 10.3）

> 版本：v0.1.0 定稿 · 2026-08-15 · 依据：`docs/API-SPEC.md` + `pkg/protocol/message.go` + `apis/edge/v1alpha1/`
> 用途：发行与交接时核对「哪些接口/字段受版本约束、哪些是新增/预留」，为 v0.2.0 演进提供基线。

---

## 1. REST API 端点矩阵（cloudcore，HTTP :8080）

| # | 方法 | 路径 | v0.1.0 状态 | 认证要求（A4） | 说明 |
|---|------|------|------------|---------------|------|
| 1 | GET | `/healthz` | ✅ 稳定 | 免认证 | 健康检查（探针）。**v0.6.0 语义分叉**：外部模式 + 多副本（`EDGEFLOW_CLOUDCORE_MULTI_REPLICA=1`，Chart 在 replicaCount>1 时自动注入）→ 反映 etcd 连接（失联 >TTL → 503，liveness 重启自愈）；其余形态（embed、单副本外部）保持进程存活语义（恒 200）。见 ARCHITECTURE R15 / DEPLOYMENT §10.8.3 |
| 2 | GET | `/metrics` | ✅ 新增（10.1） | 免认证 | Prometheus 文本格式，五指标 |
| 3 | GET | `/api/v1/nodes` | ✅ 稳定 | Bearer Token | 运行视角节点列表 |
| 4 | GET | `/api/v1/nodes/{nodeID}` | ✅ 稳定 | Bearer Token | 单节点详情 |
| 5 | GET | `/api/v1/edgenodes` | ✅ 稳定 | Bearer Token | CRD 对象视角（K8s List 风格） |
| 6 | GET | `/api/v1/edgenodes/{nodeID}` | ✅ 稳定 | Bearer Token | 单 EdgeNode 对象 |
| 7 | GET | `/api/v1/pods` | ✅ 稳定 | Bearer Token | 全节点 Pod 状态 |
| 8 | GET | `/api/v1/nodes/{nodeID}/pods` | ✅ 稳定 | Bearer Token | 单节点 Pod 状态 |
| 9 | GET | `/api/v1/devices` | ✅ 稳定 | Bearer Token | 全部设备状态 |
| 10 | GET | `/api/v1/nodes/{nodeID}/devices` | ✅ 稳定 | Bearer Token | 单节点设备状态 |
| 11 | POST | `/api/v1/nodes/{nodeID}/podsync` | ✅ 稳定 | Bearer Token | 可靠下发 Pod 配置 |
| 12 | POST | `/api/v1/nodes/{nodeID}/config-sync` | ✅ 稳定 | Bearer Token | 可靠下发配置 |
| 13 | POST | `/api/v1/nodes/{nodeID}/device-command` | ✅ 稳定 | Bearer Token | 下发设备指令 |
| 14 | POST | `/ocsp` | ✅ 新增（7.1） | 免认证（协议端点，响应自带 CA 签名） | OCSP 在线吊销查询（RFC 6960，DER 请求/响应） |

> **v0.7.0 追加（17 个模型 API 端点，均为新增 = 向后兼容；既有 14 行逐字节不变）**：

| # | 方法 | 路径 | v0.7.0 状态 | 认证要求（A4） | 说明 |
|---|------|------|------------|---------------|------|
| 15 | GET | `/api/v1/models` | ✅ 新增（v0.7.0） | Bearer Token | 模型列表（K8s List 风格，按 name 排序） |
| 16 | POST | `/api/v1/models` | ✅ 新增（v0.7.0） | Bearer Token | 创建模型 |
| 17 | GET | `/api/v1/models/{modelName}` | ✅ 新增（v0.7.0） | Bearer Token | 模型详情 |
| 18 | PUT | `/api/v1/models/{modelName}` | ✅ 新增（v0.7.0） | Bearer Token | 更新模型（description/type/metadata） |
| 19 | DELETE | `/api/v1/models/{modelName}` | ✅ 新增（v0.7.0） | Bearer Token | 删除模型（无 active 版本、无在途发布；级联） |
| 20 | GET | `/api/v1/models/{modelName}/versions` | ✅ 新增（v0.7.0） | Bearer Token | 版本列表（按 tag 排序） |
| 21 | POST | `/api/v1/models/{modelName}/versions` | ✅ 新增（v0.7.0） | Bearer Token | 创建版本（初始 draft） |
| 22 | GET | `/api/v1/models/{modelName}/versions/{version}` | ✅ 新增（v0.7.0） | Bearer Token | 版本详情 |
| 23 | DELETE | `/api/v1/models/{modelName}/versions/{version}` | ✅ 新增（v0.7.0） | Bearer Token | 删除版本（仅 draft/archived） |
| 24 | POST | `/api/v1/models/{modelName}/versions/{version}/activate` | ✅ 新增（v0.7.0） | Bearer Token | 激活（draft→active，自动降级旧 active） |
| 25 | POST | `/api/v1/models/{modelName}/versions/{version}/archive` | ✅ 新增（v0.7.0） | Bearer Token | 归档（active→archived） |
| 26 | POST | `/api/v1/models/{modelName}/releases` | ✅ 新增（v0.7.0） | Bearer Token | **创建灰度发布（异步，202）** |
| 27 | GET | `/api/v1/models/{modelName}/releases` | ✅ 新增（v0.7.0） | Bearer Token | 发布列表（按 createdAt 升序） |
| 28 | GET | `/api/v1/models/{modelName}/releases/{releaseID}` | ✅ 新增（v0.7.0） | Bearer Token | 发布详情（含 perNode 汇总） |
| 29 | GET | `/api/v1/models/{modelName}/releases/{releaseID}/digest` | ✅ 新增（v0.12.0） | Bearer Token | 发布 digest 复核（D-1：mirrorDigest vs 各节点当前 imageDigest 一致结论） |
| 30 | POST | `/api/v1/models/{modelName}/releases/{releaseID}/cancel` | ✅ 新增（v0.7.0） | Bearer Token | 取消（pending/running） |
| 31 | POST | `/api/v1/models/{modelName}/releases/{releaseID}/rollback` | ✅ 新增（v0.7.0） | Bearer Token | **回滚（异步，逆序批量，202）** |
| 32 | GET | `/api/v1/models/{modelName}/deployments` | ✅ 新增（v0.7.0） | Bearer Token | 部署影子（版本—节点—时间台账） |
| 33 | POST | `/api/v1/models/{modelName}/releases/{releaseID}/pause` | ✅ 新增（v0.16.0） | Bearer Token | 暂停发布（running→paused，节点边界生效） |
| 34 | POST | `/api/v1/models/{modelName}/releases/{releaseID}/resume` | ✅ 新增（v0.16.0） | Bearer Token | 恢复发布（paused→running，NextBatchAt 保持原节奏） |
| 35 | GET | `/api/v1/models/export` | ✅ 新增（v0.16.0） | Bearer Token | 模型目录导出（全量快照 JSON） |
| 36 | POST | `/api/v1/models/import` | ✅ 新增（v0.16.0） | Bearer Token | 模型目录导入（幂等 upsert） |
| 37 | PATCH | `/api/v1/models/{modelName}/releases/{releaseID}` | ✅ 新增（v0.17.0） | Bearer Token | 发布运行中可调参数（batchSize/pauseBetween/failFast 部分更新，批边界生效；另 v0.17.0：发布列表 +status 过滤、创建 +dryRun 预检，均不新增端点） |
| 38 | GET | `/api/v1/deployments` | ✅ 新增（v0.18.0） | Bearer Token | 全局部署影子查询（跨模型聚合，model/nodeID 过滤可选） |
| 39 | GET | `/api/v1/models/{modelName}/releases/{releaseID}/snapshot` | ✅ 新增（v0.19.0） | Bearer Token | 发布审计快照（头含 events+逐节点结果+summary 五计数+generatedAt 只读全景；另 v0.19.0：PATCH 白名单扩展 failureBudget 运行中可调） |
| 40 | GET | `/api/v1/releases` | ✅ 新增（v0.19.0） | Bearer Token | 全局发布查询（status 多值过滤 limit≤500 X-Total-Count CreatedAt 降序 tie-break by ID） |
| 41 | POST | `/api/v1/models/{modelName}/releases/{releaseID}/retry` | 失败节点重试（克隆新发布，RetryOf 回指；nodeIDs 可选 failed 子集） | v0.20.0 |
| 42 | DELETE | `/api/v1/models/{modelName}/releases/{releaseID}` | 终态发布归档删除（非终态 409；与 GC 同源「在途绝不删」） | v0.20.0 |

> **v0.37.0 追加（9 个规则管理 API 端点，均为新增 = 向后兼容；既有 42 行逐字节不变）**：

| # | 方法 | 路径 | 说明 | 版本 |
|---|------|------|------|------|
| 43 | POST | `/api/v1/rules` | 创建规则（校验失败 400、重复 ruleId 409） | v0.37.0 |
| 44 | GET | `/api/v1/rules` | 规则列表（K8s List 风格，按 ruleId 排序） | v0.37.0 |
| 45 | GET | `/api/v1/rules/{ruleID}` | 规则详情（不存在 404） | v0.37.0 |
| 46 | PUT | `/api/v1/rules/{ruleID}` | 更新规则（body ruleId 缺省补路径值） | v0.37.0 |
| 47 | DELETE | `/api/v1/rules/{ruleID}` | 删除规则 | v0.37.0 |
| 48 | GET | `/api/v1/rules/events` | 规则事件查询（ruleId/device/limit 过滤，倒序） | v0.37.0 |
| 49 | GET | `/api/v1/rules/governance` | 治理策略列表（含规则包版本） | v0.37.0 |
| 50 | PUT | `/api/v1/rules/governance` | 治理策略全量替换 | v0.37.0 |
| 51 | POST | `/api/v1/nodes/{nodeID}/rules/sync` | 规则包下发（可靠投递，五态语义同 config-sync） | v0.37.0 |

> **v0.39.0 追加（2 个上行补传可视化端点，均为新增 = 向后兼容；既有 51 行逐字节不变）**：

| # | 方法 | 路径 | 说明 | 版本 |
|---|------|------|------|------|
| 52 | GET | `/api/v1/uplink/overview` | 上行补传概览（各节点积压/丢弃/上送/接收计数） | v0.39.0 |
| 53 | GET | `/api/v1/nodes/{nodeID}/uplink` | 单节点上行状态（无数据 404） | v0.39.0 |

> **v0.40.0 追加（8 个端点：统一告警中心 5 + 设定值通道 3，均为新增 = 向后兼容；既有 53 行逐字节不变）**：

| # | 方法 | 路径 | 说明 | 版本 |
|---|------|------|------|------|
| 54 | GET | `/api/v1/alarms` | 告警列表（nodeID/state/severity/limit 过滤） | v0.40.0 |
| 55 | GET | `/api/v1/alarms/stats` | 告警统计（byState/bySeverity/total） | v0.40.0 |
| 56 | POST | `/api/v1/alarms/{alarmID}/ack` | 告警确认（raised → acked，operator 必填） | v0.40.0 |
| 57 | POST | `/api/v1/alarms/{alarmID}/assign` | 告警派单（工单集成点回调） | v0.40.0 |
| 58 | POST | `/api/v1/alarms/{alarmID}/close` | 告警闭环（任意非 closed → closed 终态） | v0.40.0 |
| 59 | POST | `/api/v1/nodes/{nodeID}/setpoints` | 设定值建单（审批开关/单笔 requireApproval） | v0.40.0 |
| 60 | POST | `/api/v1/setpoints/{setpointID}/approval` | 设定值审批（approve\|reject，仅 pending-approval 可审） | v0.40.0 |
| 61 | GET | `/api/v1/setpoints` | 设定值列表（含执行反馈 outcome/error） | v0.40.0 |

> **v0.43.0 追加（8 个视频管理面端点，均为新增 = 向后兼容；既有 61 行逐字节不变）**：

| # | 方法 | 路径 | 说明 | 版本 |
|---|------|------|------|------|
| 62 | GET | `/api/v1/videostreams` | 视频流列表（nodeID 过滤） | v0.43.0 |
| 63 | POST | `/api/v1/videostreams` | 创建视频流（name/nodeId/deviceName 必填） | v0.43.0 |
| 64 | GET | `/api/v1/videostreams/{name}` | 视频流详情（含片段索引） | v0.43.0 |
| 65 | PUT | `/api/v1/videostreams/{name}` | 更新视频流（deviceName/sourceType/status/description） | v0.43.0 |
| 66 | DELETE | `/api/v1/videostreams/{name}` | 删除视频流索引（不删媒资文件） | v0.43.0 |
| 67 | GET | `/api/v1/videostreams/{name}/snapshot` | 最新快照（image/jpeg 字节） | v0.43.0 |
| 68 | GET | `/api/v1/videostreams/{name}/segments` | 片段索引列表 | v0.43.0 |
| 69 | GET | `/api/v1/videostreams/{name}/segments/{mediaID}` | 片段回放（video/x-mjpeg 字节） | v0.43.0 |

> **v0.44.0 追加（3 个端点：流媒体分发 2 + 告警片段检索 1，均为新增 = 向后兼容；既有 69 行逐字节不变）**：

| # | 方法 | 路径 | 说明 | 版本 |
|---|------|------|------|------|
| 70 | GET | `/media/streams/{name}/live.flv` | HTTP-FLV 拉流（H.264 透传封装，chunked） | v0.44.0 |
| 71 | GET | `/media/streams/{name}/live.ws` | WS-FLV 拉流（二进制透传） | v0.44.0 |
| 72 | GET | `/api/v1/alarms/{alarmID}/segments` | 告警关联片段检索（时间窗联合查询） | v0.44.0 |

> 契约详情见 API-SPEC.md §7（v0.7.0）/ §1.1（v0.37.0 规则 API 行、v0.39.0 上行可视化行、v0.43.0 视频管理面行、v0.44.0 流媒体分发行）。

> 认证：`EDGEFLOW_CLOUDCORE_API_TOKEN` 设置为 `on` 时全部管理端点（除 healthz/metrics）要求
> `Authorization: Bearer <token>`（WBS 7.2）；未设置保持匿名（向后兼容，仅限受信网络）。
> `/ocsp` 为协议端点（非管理端点），始终免 Token 认证：响应由 CA 私钥签名，客户端验签防伪造。

## 2. 云边通道消息类型矩阵（WebSocket /v1/edge）

| 类型 | 方向 | 负载（v0.1.0 字段） | 版本说明 |
|------|------|--------------------|----------|
| `Register` | 边→云 | nodeID / arch / os / edgecoreVersion / cpu / memory / **token** | **token 为 v0.1.0 新增（WBS 7.3）**：`keadm join --token` 写入 edgecore env，注册携带；云端 `EDGEFLOW_CLOUDCORE_NODE_TOKEN` 非空时校验 |
| `RegisterAck` | 云→边 | accepted / nodeName / message | 稳定 |
| `Heartbeat` | 边→云 | timestamp（毫秒） | 稳定 |
| `HeartbeatAck` | 云→边 | nodeStatus | 稳定 |
| `PodSync` | 云→边 | nodeID / namespace / podName / image / replicas / action | 稳定（M1/M2） |
| `ConfigSync` | 云→边 | nodeID / kind / name / data / operation | 稳定（M2） |
| `PodStatus` | 边→云 | nodeID / podName / namespace / phase / restartCount / ... | 稳定（M1/M2） |
| `DeviceReport` | 边→云 | nodeID / deviceID / properties（含 direction/regAddr/value/result/message） | 稳定（M3） |
| `DeviceCommand` | 云→边 | nodeID / deviceID / property / value（设备指令，对应 POST /device-command 端点） | 稳定（M3） |
| `RuleSync` | 云→边 | ruleSet（version / rules[] / governance[]，规则包全量下发） | 新增（v0.37.0） |
| `RuleEvent` | 边→云 | ruleId / ruleName / deviceName / namespace / property / value / severity / message / triggeredAt / ruleSetVersion | 新增（v0.37.0） |
| `UplinkReport` | 边→云 | depth / dropped / sent / oldestTs（上行补传队列状态周期上报） | 新增（v0.39.0） |
| `AlarmEvent` | 边→云 | alarmId / nodeId / source / severity / state / message / count / raisedAt / updatedAt（告警事实，三处同构） | 新增（v0.40.0） |
| `SetpointResult` | 边→云 | setpointId / ok / value / error / ts（设定值执行反馈闭环） | 新增（v0.40.0） |
| `MediaUpload` | 边→云 | mediaId / kind（snapshot\|segment）/ nodeId / deviceName / streamName / capturedAt / contentType / frameCount / totalBytes / sha256 / chunkSeq / chunkTotal / chunkData（base64 分片） | 新增（v0.43.0） |
| `Ack` | 双向 | id / ok / error | 稳定（可靠投递） |

兼容规则：
- 云边消息字段**只增不删**；新增字段必须可选（`omitempty`），老版本对端忽略未知字段（JSON 解码容忍）。
- 协议 `Version=v1` 为云边兼容锚点；CloudCore/EdgeCore 建议同版本部署。
- **v0.7.0 无新消息类型**：模型发布/回滚完全复用既有 `PodSync`（镜像 Pod）与 `ConfigSync`（模型版本/参数 ConfigMap，载荷约定见 API-SPEC §7.4）——云边协议零改动，旧版 edgecore 无需任何升级（**边缘零改动**）。

## 3. CRD 类型矩阵（edgeflow.io/v1alpha1）

| 类型 | kind | 关键 spec 字段 | 状态 |
|------|------|---------------|------|
| EdgeNode | `EdgeNode` | nodeID / role / addresses；status: phase / heartbeatTime / conditions | ✅ 稳定（config/crd/edgenodes.edgeflow.io.yaml） |
| DeviceModel | `DeviceModel` | protocol / properties[]（name/dataType/accessMode/min/max/unit） | ✅ 稳定 |
| Device | `Device` | deviceModelRef / nodeName / protocol / properties[]（desired） | ✅ 稳定 |

演进策略：v1alpha1 阶段允许字段新增（向后兼容）；破坏性变更必须升级 v1alpha2/v1 并双版本 served（storage 单版本），迁移期至少一个迭代共存。

## 4. 变更登记（v0.1.0 相对早期草案）

| 变更 | 类型 | 说明 |
|------|------|------|
| `/metrics` 新增 | 新增端点 | 10.1 可观测性（commit `4c5b9c6`）；已同步回写 API-SPEC §1.1 |
| `Register.token` 新增 | 新增可选字段 | 7.3 设备认证（commit 见台账 B1） |
| API Token 认证 | 行为开关 | 默认 off 向后兼容，env 开启（commit `4c5b9c6`） |
| 错误语义 404/502/504 | 稳定 | 已定稿于 API-SPEC §1.2 |

## 5. 维护约定

- 每次增删端点/字段，同步更新本矩阵 + API-SPEC.md + 解决方案手册附录 A。
- 版本兼容检查纳入发布清单（RELEASE-CHECKLIST.md）：发版前对照 §1-§3 全表核对。


## v0.13.0 兼容性增量（2026-08-26）

| 变更 | 兼容性 |
|---|---|
| `GET .../deployments` 新增 `limit`/`offset` query 参数 + `X-Total-Count` 响应头 | 零破坏：缺省全量（旧行为逐字节一致）；非法参数才 400（旧客户端不传）；列表形态不变 |
| `/api/v1/nodes`（`offlineAt`）、`/api/v1/edgenodes`（`status.lastOfflineTime`）新增可选响应字段 | 零破坏：JSON 宽容（老客户端忽略未知字段；新客户端读旧数据缺省省略）；瞬态内存数据不落盘 |
| `DeleteModel` 在 GC 显式开启时级联清理该模型全部终态发布 | 默认关闭（GC-off）= L31 审计口径零变化；仅运维已开启 GC 时行为扩展（既有 GC 开启后口径变更的既定分支） |


### v0.15.0

- **端点**：零新增（总数维持 32）；设备数据面新增订阅采集模式（边缘侧行为，云端无感）。
- **新增 env（1 个，边缘侧 opt-in）**：`EDGEFLOW_OPCUA_SUBSCRIPTION`（on/off，缺省 off=轮询模式与 v0.14.0 逐字节一致）。存量变量语义零变化。
- **升级零迁移**：无键空间/schema 变化；老边缘零动作；go.mod 零新依赖；pkg/opcua 既有导出 API 零变更（泵模式仅订阅启用后生效）。

### v0.14.0

- 端点：零新增（总数维持 32）；`/api/v1/devices` 对 OPC-UA 设备自然扩展。
- 新增 env（4 个，边缘侧 opt-in）：EDGEFLOW_OPCUA_ENDPOINT/NODES/DEVICE_NAME/NAMESPACE（见 DEPLOYMENT §14）。
- 升级零迁移；老边缘零动作；零新依赖。

## v0.23.0 兼容性增量（2026-08-28）

本轮为响应形态口径统一轮：端点总数 42 不变，零新增/零删除；全部变化集中在发布对象响应形态，逐条登记如下。

| 变更 | 涉及端点 | 兼容性 |
|---|---|---|
| 发布对象 `summary` 五计数恒现（去 omitempty；total/pending/running/succeeded/failed，零值发布输出全零对象而非字段缺省） | `POST .../releases`（202 响应）、`GET .../releases/{id}`、`GET .../releases`（逐条）、`POST .../releases/{id}/cancel`、`POST .../releases/{id}/rollback`、`POST .../releases/{id}/retry`、`PATCH .../releases/{id}`、`GET /api/v1/releases`（逐条） | **向后兼容（加字段）**：新增恒现响应字段，旧客户端按 JSON 宽容语义忽略未知字段；仅依赖「summary 字段缺失=零值发布」这一非承诺语义的客户端需改为判 `summary.total==0`（该语义从未入文档承诺） |
| `GET /api/v1/releases` 响应自裸 `items` 数组改为 K8s List 包装 `{kind:"ReleaseList", apiVersion:"edgeflow.io/v1alpha1", items:[...]}` | `GET /api/v1/releases` | **破坏性（顶层形态变更）**：直接迭代响应顶层数组的客户端需改为读 `items` 字段；分页语义（status/limit/offset/X-Total-Count/排序）零变化。与模型域既有 K8s List 风格（ModelList/VersionList 等）对齐，属双口径收敛 |
| `POST .../releases/{id}/retry` 对终态版本 422 文案携带 `orig.Status` | `POST .../releases/{id}/retry` | **零破坏**：状态码（422）与校验链序不变，仅错误文案信息量增加（机器可读解析不受影响，`error` 字段仍为字符串） |
| `GET .../releases/{id}/snapshot` summary 口径与详情/列表统一（五计数同源） | `GET .../releases/{id}/snapshot` | **零破坏**：字段集不变，数值口径收敛 |

云边协议与 CRD 说明：`pkg/protocol` Validate 新增 Version 宽松格式校验（`^v[0-9]+$`，非空既有契约不变；Timestamp 显式不校验）——NewMessage 产出的信封（Version="v1"）全部通过，旧边缘零改动；CRD `config/crd/*.yaml` 关键 string 字段补 minLength/pattern 校验标记（K8s 准入层拒绝脏对象），apis/ 类型零变化，已存在的合法对象全部兼容。

## v0.37.0 兼容性增量（2026-09-16）

端点总数 42 → 51（+9 规则管理 API，全部新增 = 向后兼容；既有 42 行逐字节不变）；云边消息类型 10 → 12（+RuleSync/+RuleEvent）。

| 变更 | 兼容性 |
|---|---|
| 新增 `/api/v1/rules*` 9 端点（含规则包下发 `/api/v1/nodes/{nodeID}/rules/sync`） | **零破坏**：全新路由（`/api/v1/rules/*` 前缀此前无路由）；默认无规则/无治理策略时规则引擎全链空转，采集/影子/上报路径与 v0.36.0 逐字节一致 |
| 云边消息 +`RuleSync`（云→边）/`RuleEvent`（边→云） | **零破坏**：新增类型不影响既有 12 类型；未下发规则包时两类型不出现；旧边缘收到未知类型走既有 default 忽略路径 |
| 云端规则存储（etcd 键空间 `/edgeflow/ruleset/*`；纯内存模式无持久化） | 升级零迁移：新增键空间；重启自动恢复；与 devicestatus 同构的写穿语义（写穿失败不更新内存） |
| 边缘规则包持久化（SQLite `rules/current` 键 + `rule_events` 表，保留 30 天） | 升级零迁移：新增键/表；无规则包时零行为；规则包损坏安全降级为空规则 |
| 零新依赖 / 既有包 API | go.mod 零变化；`pkg/rules` 为新增共享包；MQTT/OPC-UA/视频/模型面代码零触碰；v0240–v0350 冻结测试零改动 |

## v0.39.0 兼容性增量（2026-09-16）

端点总数 51 → 53（+2 上行补传可视化端点，全部新增 = 向后兼容；既有 51 行逐字节不变）；云边消息类型 12 → 13（+UplinkReport）。

| 变更 | 兼容性 |
|---|---|
| 新增 `/api/v1/uplink/overview`、`/api/v1/nodes/{nodeID}/uplink` 2 端点 | **零破坏**：全新路由（`/api/v1/uplink/*` 与 `.../uplink` 此前无路由）；数据源为增量统计缓存，无数据时空列表 / 404 |
| 云边消息 +`UplinkReport`（边→云） | **零破坏**：新增类型不影响既有 12 类型；未启用补传（`EDGEFLOW_EDGECORE_UPLINK` off）时不上报；旧边缘不发送 |
| 云端 RuleEvent 接收幂等（消息 ID 滚动去重，窗口 10000，FIFO 淘汰） | **零破坏**：单发路径不命中去重集；重复仅在补传重发时被消化（丢弃 + 计数） |
| 云端在途缓冲（etcd `/edgeflow/ruleevents/*`：写→入环→删；启动 Load 恢复） | 升级零迁移：新增键空间；事件 ring 语义不变（内存窗口）；etcd 不可用时降级内存环（Warn） |
| 边缘上行补传（opt-in `EDGEFLOW_EDGECORE_UPLINK=on`；SQLite `uplink_queue` / `uplink_meta` 表） | **默认零行为**：未开启时规则事件出口与 v0.38.0 直发路径逐字节一致；开启后为至少一次语义（发送失败留队列、恢复续传、云端幂等） |
| 零新依赖 / 既有包 API | go.mod 零变化；MQTT/OPC-UA/视频/模型面代码零触碰；v0240–v0350 冻结测试零改动 |

## v0.40.0 兼容性增量（2026-10-01）

端点总数 53 → 61（+8：告警中心 5 + 设定值通道 3，全部新增 = 向后兼容；既有 53 行逐字节不变）；云边消息活跃类型 13 → 15（+AlarmEvent / +SetpointResult）。

| 变更 | 兼容性 |
|---|---|
| 新增告警中心 5 端点（`/api/v1/alarms`、`/api/v1/alarms/stats`、`/api/v1/alarms/{alarmID}/ack|assign|close`） | **零破坏**：全新路由（`/api/v1/alarms*` 此前无路由）；空库返回空列表；操作端点 operator 必填（审计留痕） |
| 新增设定值通道 3 端点（`POST /api/v1/nodes/{nodeID}/setpoints`、`POST /api/v1/setpoints/{setpointID}/approval`、`GET /api/v1/setpoints`） | **零破坏**：全新路由；建单与既有 device-command 端点零耦合（既有路径零触碰）；审批默认 off |
| 云边消息 +`AlarmEvent`（边→云） | **零破坏**：新增类型；无规则时零行为；旧边缘不发送 |
| 云边消息 +`SetpointResult`（边→云） | **零破坏**：新增类型；仅 class=setpoint 且带 setpointId 的指令回告；普通指令路径零变化 |
| DeviceCommand 负载仅增可选字段（class / setpointId） | **零破坏**：JSON 仅增；旧边缘解码忽略未知字段；普通指令（无 setpointId）逐字节等价 |
| 边缘告警链（规则触发源挂点；alarm_ledger 表 + 本地联动 logLinkage） | 随规则启用自然生效；无规则时零行为；台账失败降级仅内存聚合 |
| 云端告警中心 / 设定值存储（etcd 写穿 `/edgeflow/alarms/*`、`/edgeflow/setpoints/*`） | 升级零迁移：新增键空间；重启自动恢复；纯内存形态降级同 rulestore |
| 设定值投递 flush（默认 30s，`EDGEFLOW_CLOUDCORE_SETPOINT_FLUSH_SEC` 可调；审批 `EDGEFLOW_CLOUDCORE_SETPOINT_APPROVAL` 默认 off） | **默认零行为**：无建单时 flush 空转；审批不开时建单直接 pending-send |
| 零新依赖 / 既有包 API | go.mod 零变化；MQTT/OPC-UA/视频/模型面代码零触碰；v0240–v0350 冻结测试零改动 |


## v0.43.0 兼容性增量（2026-10-02）

端点总数 61 → 69（+8：视频管理面，全部新增 = 向后兼容；既有 61 行逐字节不变）；云边消息活跃类型 15 → 16（+MediaUpload）。

| 变更 | 兼容性 |
|---|---|
| 新增视频管理面 8 端点（`/api/v1/videostreams*`） | **零破坏**：全新路由（此前无路由）；空库返回空列表；媒资文件存储与既有 etcd 键空间隔离 |
| 云边消息 +`MediaUpload`（边→云，分片上传） | **零破坏**：新增类型；media 未配置时零行为；旧云端对未知类型仅日志忽略 |
| 云端存储 +videostream（etcd 写穿 `/edgeflow/videostreams/*`）/ +mediastore（`/edgeflow/media/*` + 媒资目录） | 升级零迁移：新增键空间与媒资目录（`EDGEFLOW_CLOUDCORE_MEDIA_DIR`，默认 data/media）；重启自动恢复 |
| 边缘媒资采集（video 配置 `media` 块 + `EDGEFLOW_MEDIA_SPOOL_DIR`，依赖 `EDGEFLOW_EDGECORE_UPLINK=on`） | **默认零行为**：media 未配置/补传未开启时不上传（Warn）；既有视频链路零变化 |
| 零新依赖 / 既有包 API | go.mod 零变化；MQTT/OPC-UA/规则/时序面零触碰；v0240–v0350 冻结测试零改动 |


## v0.44.0 兼容性增量（2026-10-02）

端点总数 69 → 72（+3：流媒体分发 2 + 告警片段检索 1，全部新增 = 向后兼容；既有 69 行逐字节不变）；云边消息活跃类型 16 维持（本版无新消息）。

| 变更 | 兼容性 |
|---|---|
| 新增流媒体分发 2 端点（`/media/streams/{name}/live.flv|live.ws`） | **零破坏**：全新 `/media/*` 命名空间（此前无路由）；无注册帧源 404（默认零行为）；H.264 透传封装（无转码） |
| 新增告警片段检索 1 端点（`GET /api/v1/alarms/{alarmID}/segments`） | **零破坏**：全新路由；联合查询只读（alarmstore×videostream）；窗默认 [RaisedAt-5s,+60s] |
| 云端帧源注册表 + 演示源 opt-in（`EDGEFLOW_CLOUDCORE_DEMO_H264_STREAMS`） | **默认零行为**：未配置不注册演示源；注册表为进程内存态 |
| 零新依赖 / 既有包 API | go.mod 零变化；MQTT/OPC-UA/规则/时序面零触碰；v0240–v0350 冻结测试零改动 |
