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

> 契约详情见 API-SPEC.md §7（v0.7.0）。

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
