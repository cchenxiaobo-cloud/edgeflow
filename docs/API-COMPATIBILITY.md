# EdgeFlow API 兼容矩阵（WBS 10.3）

> 版本：v0.1.0 定稿 · 2026-08-15 · 依据：`docs/API-SPEC.md` + `pkg/protocol/message.go` + `apis/edge/v1alpha1/`
> 用途：发行与交接时核对「哪些接口/字段受版本约束、哪些是新增/预留」，为 v0.2.0 演进提供基线。

---

## 1. REST API 端点矩阵（cloudcore，HTTP :8080）

| # | 方法 | 路径 | v0.1.0 状态 | 认证要求（A4） | 说明 |
|---|------|------|------------|---------------|------|
| 1 | GET | `/healthz` | ✅ 稳定 | 免认证 | 健康检查（探针） |
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

> 认证：`EDGEFLOW_CLOUDCORE_API_TOKEN` 设置为 `on` 时全部管理端点（除 healthz/metrics）要求
> `Authorization: Bearer <token>`（WBS 7.2）；未设置保持匿名（向后兼容，仅限受信网络）。

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
