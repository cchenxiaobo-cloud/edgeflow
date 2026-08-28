# EdgeFlow API 规范（v0.1.0 定稿）

> - 对应 ROADMAP WBS 9.2「API 文档」，覆盖两部分：**REST API 参考**（cloudcore 对外 HTTP 接口）与 **CRD 类型定义**（`apis/edge/v1alpha1/`）。
> - 状态：✅ **v0.1.0 定稿**（2026-08-14），**v0.2.0 开发轮已更新**（2026-08-18：podsync 资源字段与 409 语义、device-command namespace 路由、资源调度环境变量），**v0.3.0 开发轮已更新**（2026-08-19：syncPod 400 响应 JSON 安全加固说明 + 第四部分共享库/协议包 API 边界），**v0.4.0 开发轮已更新**（2026-08-24：§1 并发语义、§9 已知限制首条改为分级持久化、nodeID 字符约束登记），**v0.7.0 开发轮已更新**（2026-08-25：模型仓库/版本管理/灰度发布 17 个新端点（**14→31**）、新增 202/422 错误语义、新章节 §7 模型 API），**v0.8.0 开发轮已更新**（2026-08-26：列表分页 limit/offset + X-Total-Count（§7.2）、外部 etcd RBAC 鉴权透传与终态发布 GC 配置（见 DEPLOYMENT §10.7）、指标 5→7 项（§1.2）），**v0.9.0 开发轮已更新**（2026-08-26：发布创建镜像存在性探活（R-1，默认 off，见 §7.2/§7.3）、Pod 状态写穿持久化（重启后 Pod 列表直接可见，§9 行为变化）），**v0.10.0 开发轮已更新**（2026-08-26：设备属性写穿持久化（重启后属性立即可见）、发布批内并发（EDGEFLOW_CLOUDCORE_RELEASE_BATCH_PARALLEL，默认 1=串行，见 §7.2）），**v0.11.0 开发轮已更新**（2026-08-26：镜像 digest 级校验（探活固化 mirrorDigest + 边缘 imageDigest 比对，见 §7.2/§7.3）、/metrics 指标 7→8 项（hb_rebuilds_total，仅外部模式，见 §1.2）），**v0.12.0 开发轮已更新**（2026-08-26：digest 校验端到端落地（真实边缘采集 + 发布复核端点，只增不改，端点 31→32，见 §7.2/§7.3）），**v0.13.0 开发轮已更新**（2026-08-26：deployments 列表分页（A′）、节点 DTO offlineAt/lastOfflineTime（C）、模型删除 GC-on 级联（B）；零新增端点，总数维持 32）。评审记录见 `docs/REVIEWS.md`（9.2 评审归档）。
> - 代码位置：cloudcore 路由装配 `cmd/cloudcore/main.go`、设备 API `cmd/cloudcore/device_api.go`、CRD 类型 `apis/edge/v1alpha1/`。
> - 版本策略：v0.1.0 为 MVP 定稿版；后续接入 Kubernetes 后由 OpenAPI schema / CRD 校验取代，见 §8。

---

# 第一部分 REST API 参考（cloudcore）

## 1. 通用约定

| 项 | 约定 |
|----|------|
| Base URL | `http://<cloudcore-ip>:8080`（端口可用 `--port` / `EDGEFLOW_CLOUDCORE_PORT` 覆盖） |
| 数据格式 | JSON；请求 `Content-Type: application/json`；响应同样为 JSON |
| 时间戳 | Unix 毫秒（心跳/上报/注册时间）；CRD 对象内的时间字段为 RFC3339 字符串 |
| List 风格 | 查询类端点采用 K8s List 风格（`kind`/`apiVersion`/`items`），空数据编码为 `[]` 而非 `null` |
| 路径参数 | `{nodeID}` 为边缘节点 ID（edgecore 注册时上报，默认 `edge-<hostname>`）。**v0.4.0 硬约束**：nodeID 必须匹配 `^[A-Za-z0-9._-]+$`，含 `/` 的 nodeID 写入（注册/设备指令/删除）被拒绝并告警（见 §9） |
| 并发语义 | **v0.4.0 起分级持久化**：云端注册元数据与设备 Desired 跨重启保留（嵌入式 etcd 写穿）；Pod 状态与上报属性（properties）为内存态，重启后短暂清空（≤1 上报周期，边缘重连自愈）；边缘侧 MetaManager（SQLite）持久化 |

### 1.1 端点总览

| 方法 | 路径 | 说明 | 主要状态码 |
|------|------|------|-----------|
| GET | `/healthz` | 健康检查（探针用） | 200 |
| GET | `/metrics` | Prometheus 指标（七指标，WBS 10.1；v0.8.0 增续约失败计数） | 200 |
| GET | `/api/v1/nodes` | 全部节点（运行视角 NodeInfo 列表） | 200 |
| GET | `/api/v1/nodes/{nodeID}` | 单节点详情 | 200 / 404 |
| GET | `/api/v1/edgenodes` | 全部节点（CRD 对象视角，K8s List 风格） | 200 |
| GET | `/api/v1/edgenodes/{nodeID}` | 单节点 EdgeNode 对象 | 200 / 404 |
| GET | `/api/v1/pods` | 全部节点 Pod 状态 | 200 |
| GET | `/api/v1/nodes/{nodeID}/pods` | 单节点 Pod 状态 | 200 / 404 |
| GET | `/api/v1/devices` | 全部设备状态（properties + desired） | 200 |
| GET | `/api/v1/nodes/{nodeID}/devices` | 单节点设备状态 | 200 / 404 |
| POST | `/api/v1/nodes/{nodeID}/podsync` | 可靠下发 Pod 配置（add/update/delete，含资源诉求） | 200 / 400 / 404 / 409 / 502 / 504 |
| POST | `/api/v1/nodes/{nodeID}/config-sync` | 可靠下发 ConfigMap/Secret 配置 | 200 / 400 / 404 / 502 / 504 |
| POST | `/api/v1/nodes/{nodeID}/device-command` | 下发设备指令（期望值） | 200 / 400 / 404 / 502 / 504 |
| POST | `/ocsp` | OCSP 在线吊销查询（RFC 6960；请求/响应均为 DER 编码，Content-Type: application/ocsp-request / application/ocsp-response）。免认证（唯一例外，详见 §1.3）；per-IP 限流（默认 10 req/s，burst 20，超限 429）；成功响应带 `Cache-Control: max-age=3600` | 200 / 400 / 429 / 500 |

> **v0.7.0 模型 API（17 个新端点，总端点 14→31）**：以下 18 行为新增（v0.12.0 增发布 digest 复核端点），注册于既有 apiMux（自动覆盖 auth+audit）；既有 14 行端点零改动。契约详见 §7 模型 API。

| 方法 | 路径 | 说明 | 主要状态码 |
|------|------|------|-----------|
| GET | `/api/v1/models` | 模型列表（K8s List 风格，按 name 排序；v0.8.0 起支持 `limit`(1-1000)/`offset`(≥0) 分页，缺省全量，响应头 `X-Total-Count`） | 200 / 400 |
| POST | `/api/v1/models` | 创建模型 | 200 / 400 / 409 |
| GET | `/api/v1/models/{modelName}` | 模型详情 | 200 / 404 |
| PUT | `/api/v1/models/{modelName}` | 更新模型（description/type/metadata） | 200 / 400 / 404 / 409 |
| DELETE | `/api/v1/models/{modelName}` | 删除模型（无 active 版本、无在途发布；级联 draft/archived 版本+部署影子） | 200 / 404 / 409 |
| GET | `/api/v1/models/{modelName}/versions` | 版本列表（按 tag 排序；v0.8.0 起支持分页，同上）；模型不存在 → 404 | 200 / 400 / 404 |
| POST | `/api/v1/models/{modelName}/versions` | 创建版本（初始 draft） | 200 / 400 / 404 / 409 |
| GET | `/api/v1/models/{modelName}/versions/{version}` | 版本详情 | 200 / 404 |
| DELETE | `/api/v1/models/{modelName}/versions/{version}` | 删除版本（仅 draft/archived） | 200 / 404 / 409 |
| POST | `/api/v1/models/{modelName}/versions/{version}/activate` | 激活（draft→active，自动降级旧 active） | 200 / 400 / 404 / 409 |
| POST | `/api/v1/models/{modelName}/versions/{version}/archive` | 归档（active→archived） | 200 / 404 / 409 |
| POST | `/api/v1/models/{modelName}/releases` | **创建灰度发布（异步执行）**；v0.9.0 起可选镜像存在性探活（R-1：`EDGEFLOW_CLOUDCORE_RELEASE_MIRROR_CHECK` off/warn/fail，默认 off；fail 时探活失败 → 422） | **202** / 400 / 404 / 409 / 422 |
| GET | `/api/v1/models/{modelName}/releases` | 发布列表（按 createdAt 升序；v0.8.0 起支持分页，同上） | 200 / 400 / 404 |
| GET | `/api/v1/models/{modelName}/releases/{releaseID}` | 发布详情（含 perNode 汇总） | 200 / 404 |
| GET | `/api/v1/models/{modelName}/releases/{releaseID}/digest` | **发布 digest 复核（v0.12.0，D-1：发布 mirrorDigest vs 各节点当前 imageDigest 一致结论，含 consistency）** | 200 / 404 |
| POST | `/api/v1/models/{modelName}/releases/{releaseID}/pause` | **暂停发布（v0.16.0：running→paused，节点边界生效；幂等）** | 200 / 404 / 409 |
| POST | `/api/v1/models/{modelName}/releases/{releaseID}/resume` | **恢复发布（v0.16.0：paused→running，NextBatchAt 保持原节奏）** | 200 / 404 / 409 |
| PATCH | `/api/v1/models/{modelName}/releases/{releaseID}` | **发布运行中可调参数（v0.17.0：batchSize/pauseBetween/failFast 部分更新批边界生效；v0.19.0 起白名单扩展 failureBudget=0 即关闸）** | 200 / 400 / 404 / 409 |
| DELETE | `/api/v1/models/{modelName}/releases/{releaseID}` | **终态发布归档删除（v0.20.0：手动点删单条 succeeded/failed/canceled/rolled_back；非终态 409 与 GC 同源「在途绝不删」）** | 200 / 404 / 409 |
| GET | `/api/v1/deployments` | **全局部署影子查询（v0.18.0：跨模型聚合，model/nodeID 过滤可选，过滤后分页）** | 200 / 400 |
| GET | `/api/v1/models/{modelName}/releases/{releaseID}/snapshot` | **发布审计快照（v0.19.0：头含 events + 逐节点结果 + summary 五计数 + generatedAt 只读全景）** | 200 / 404 |
| GET | `/api/v1/releases` | **全局发布查询（v0.19.0：跨模型聚合，status 多值过滤，limit≤500/offset 分页，X-Total-Count，CreatedAt 降序 tie-break by ID）** | 200 / 400 |
| GET | `/api/v1/models/export` | **模型目录导出（v0.16.0：models+versions 全量快照 JSON，schemaVersion=1）** | 200 |
| POST | `/api/v1/models/import` | **模型目录导入（v0.16.0：幂等 upsert；同 version 跳过、active 直通灾备语义）** | 200 / 400 |
| POST | `/api/v1/models/{modelName}/releases/{releaseID}/retry` | **失败节点重试（v0.20.0：仅终态可重试，克隆新发布 RetryOf 回指原发布；nodeIDs 可选=failed 子集，缺省全部；版本须仍 active）** | **202** / 400 / 404 / 409 / 422 |
| POST | `/api/v1/models/{modelName}/releases/{releaseID}/cancel` | 取消（v0.16.0 起 pending/running/paused） | 200 / 404 / 409 |
| POST | `/api/v1/models/{modelName}/releases/{releaseID}/rollback` | **回滚（异步执行，逆序批量）** | **202** / 404 / 409 / 422 |
| GET | `/api/v1/models/{modelName}/deployments` | 部署影子（版本—节点—时间追踪，F41 台账）；**v0.13.0 支持 `limit`(1-1000)/`offset`(≥0) 分页 + `X-Total-Count` 头，缺省全量** | 200 / 404 |

### 1.2 错误码表（统一约定）

| HTTP 状态码 | 语义 | 典型场景 |
|------------|------|---------|
| `200` | 成功；下发类接口表示**边缘已确认**（Ack ok），响应 `{"status":"ok","acked":true}` | 正常 |
| `202` | **已受理（异步执行，v0.7.0 新增）**：灰度发布创建 / 回滚置位——任务已登记并开始执行，结果以 release 对象回读（状态机推进）为准 | 灰度任务开始执行 |
| `400` | 请求非法：JSON 解析失败 / 缺必填字段 / operation 或 kind 不在白名单 / 资源格式非法或 request>limit（仅 podsync，文案含具体超标字段，如 `CPU request (500m) 不能超过 CPU limit (250m)`） | 参数错误 |
| `404` | 节点未注册或离线（`ErrNodeOffline`）；单资源查询不存在；模型不存在时其子资源一律 404 | 节点/资源不存在 |
| `409` | 冲突：节点资源超卖，边缘准入拒绝（仅 podsync，WBS 6.5）——响应 `{"error":"EDGEFLOW_RESOURCE_EXHAUSTED: ..."}`，拒绝不落盘；模型 API 冲突族（v0.7.0）：模型/版本已存在、删除 active 版本、归档/激活状态机非法、同模型在途发布（含在途 releaseID）、CAS 冲突耗尽、cancel/rollback 目标态不合法 | 已部署 request 求和 + 新请求超出节点容量 × 超卖率 / 状态冲突 |
| `422` | **语义不可执行（v0.7.0 新增）**：发布目标版本非 active / 无 Ready 节点 / 白名单含未知节点 / 无 PrevActive 可回滚 | 业务前置不满足 |
| `429` | 限流：per-IP 请求速率超限（当前仅 `/ocsp` 端点，默认 10 req/s，burst 20；`EDGEFLOW_CLOUDCORE_OCSP_RATE_LIMIT` 可调） | 客户端请求过频 |
| `500` | 内部错误（消息构建失败、发送通道异常等兜底） | 服务端异常 |
| `502` | 边缘明确拒绝（回 error Ack）：消息已送达但处理失败 | 边缘侧校验失败 |
| `504` | 可靠投递确认超时（默认单次 5s × 最多 3 次尝试） | 边缘宕机/链路抖动 |

错误响应统一为 JSON：`{"error":"<机器可读原因>", ...可选字段}`（`http.Error` 输出）。

> 语义区分：**404 = 没送达**（节点不在线，无需重试）；**409 = 送达但超卖拒绝**（仅 podsync，重试无意义，与 502 区分）；**502 = 送达但被拒绝**（重试无意义）；**504 = 可能送达但未确认**（可重试，边缘侧有幂等去重）。

### 1.3 认证与限流（端点安全约定）

- 除 `/ocsp` 外，全部 `/api/v1/*` 端点均经 Bearer Token 认证中间件；`/ocsp` 为唯一免认证端点（OCSP 客户端通常不支持自定义头，且 RFC 6960 要求响应可被离线验证，认证不增加安全性）。
- `/ocsp` 以 per-IP 令牌桶限流替代认证防滥用：默认 10 req/s、burst 20，超限返回 `429`（`{"error":"ocsp rate limit exceeded"}`）；速率可通过 `EDGEFLOW_CLOUDCORE_OCSP_RATE_LIMIT`（每秒次数，burst=2×rate）调整，非法值回退默认。
- `/ocsp` 成功响应带 `Cache-Control: max-age=3600`（nextUpdate≈7 天，1 小时缓存对 good/revoked/unknown 均安全）。

---

## 2. 健康检查

### GET /healthz

探针路径（Helm Chart liveness/readiness 均指向它）。成功返回 200 + 版本信息。

请求：

```bash
curl http://127.0.0.1:8080/healthz
```

响应 `200`：

```json
{"status":"ok","version":{"version":"v0.1.0","gitCommit":"c880bd9","buildTime":"2026-08-14T19:08:17+0800","goVersion":"go1.26.2"}}
```

---

## 3. 节点 API

### 3.1 GET /api/v1/nodes —— 全部节点（NodeInfo 视图）

运行视角：直接返回节点元数据数组（按 NodeID 排序），非 K8s List 风格（与 `edgenodes` 端点互补）。

请求：

```bash
curl http://127.0.0.1:8080/api/v1/nodes
```

响应 `200`（示例，`[]` 表示无节点）：

```json
[
  {
    "nodeID": "edge-node-1",
    "nodeName": "edge-node-1",
    "arch": "arm64",
    "os": "darwin",
    "edgecoreVersion": "version=v0.1.0 gitCommit=... goVersion=go1.26.2",
    "cpu": 8,
    "memory": 0,
    "ip": "127.0.0.1",
    "registeredAt": 1786705914423,
    "lastHeartbeatAt": 1786705914423,
    "status": "Ready"
  }
]
```

字段说明：

| 字段 | 类型 | 说明 |
|------|------|------|
| nodeID | string | 节点唯一 ID（edgecore 的 `EDGEFLOW_EDGECORE_NODE_ID`） |
| nodeName | string | 云端分配的节点名（当前与 nodeID 一致） |
| arch / os | string | edgecore 所在平台（注册时上报） |
| edgecoreVersion | string | edgecore 版本串 |
| cpu / memory | int / uint64 | 节点资源（内存当前恒 0，待采集接入） |
| ip | string | 连接来源 IP |
| registeredAt / lastHeartbeatAt | int64 | 注册 / 最近心跳时间（Unix 毫秒） |
| status | string | `Ready` / `Unknown` / `Offline` |

### 3.2 GET /api/v1/nodes/{nodeID} —— 单节点详情

请求：

```bash
curl http://127.0.0.1:8080/api/v1/nodes/edge-node-1
```

响应：`200` 同上结构；节点不存在 → `404` + `{"error":"node not found","nodeID":"edge-node-1"}`。

### 3.3 GET /api/v1/edgenodes —— 全部节点（EdgeNode CRD 视图）

CRD 对象视角（对标 `kubectl get edgenodes`）：K8s List 风格，`items` 元素即完整 EdgeNode 对象（含 `apiVersion: edgeflow.io/v1alpha1`），可直接当作 CRD 对象消费。

请求：

```bash
curl http://127.0.0.1:8080/api/v1/edgenodes
```

响应 `200`：

```json
{
  "kind": "EdgeNodeList",
  "apiVersion": "edgeflow.io/v1alpha1",
  "items": [
    {
      "apiVersion": "edgeflow.io/v1alpha1",
      "kind": "EdgeNode",
      "metadata": {"name": "edge-node-1", "uid": "...", "creationTimestamp": "2026-08-14T19:11:54+08:00"},
      "spec": {"nodeID": "edge-node-1", "role": "edge"},
      "status": {"phase": "Running", "heartbeatTime": "...", "conditions": [{"type": "Ready", "status": "True"}]}
    }
  ]
}
```

### 3.4 GET /api/v1/edgenodes/{nodeID} —— 单节点 EdgeNode 对象

请求：

```bash
curl http://127.0.0.1:8080/api/v1/edgenodes/edge-node-1
```

响应：`200` 返回单个 EdgeNode 对象；节点不存在 → `404`。

---

## 4. Pod API

### 4.1 GET /api/v1/pods —— 全部 Pod 状态

请求：

```bash
curl http://127.0.0.1:8080/api/v1/pods
```

响应 `200`：

```json
{
  "kind": "PodStatusList",
  "apiVersion": "v1",
  "items": [
    {
      "nodeID": "edge-node-1",
      "podName": "nginx",
      "namespace": "default",
      "phase": "Running",
      "message": "",
      "lastReconcileAt": 1786705732883
    }
  ]
}
```

字段说明：`phase` 取 `Running` / `Stopped` / `Absent` / `Error` / `Unknown`（未知值云端丢弃并告警）；`message` 为附加说明（如错误原因）；`lastReconcileAt` 为边缘最近一次调谐时间（Unix 毫秒）。

### 4.2 GET /api/v1/nodes/{nodeID}/pods —— 单节点 Pod 状态

语义约定：
- 节点不存在（从未注册）→ `404`；
- 节点存在但无 Pod → `200` + 空 `items`（不是 404，客户端可无分支遍历）。

---

## 5. 设备 API

### 5.1 GET /api/v1/devices —— 全部设备状态

设备影子（数字孪生）云端视图：`properties` 为设备实际上报值，`desired` 为云端下发的期望值（device-command 成功后写入；设备上报不会覆盖它）。

请求：

```bash
curl http://127.0.0.1:8080/api/v1/devices
```

响应 `200`（示例：内置模拟传感器 sensor-01）：

```json
{
  "kind": "DeviceStatusList",
  "apiVersion": "v1",
  "items": [
    {
      "nodeID": "edge-node-1",
      "deviceName": "sensor-01",
      "namespace": "default",
      "properties": {"humidity": 46.39, "temperature": 29.38},
      "desired": {"targetTemp": 25},
      "lastReportedAt": 1786705917423
    }
  ]
}
```

字段说明：`properties` / `desired` 均为 `map[string]float64`；`lastReportedAt` 为最近上报时间（Unix 毫秒）。

### 5.2 GET /api/v1/nodes/{nodeID}/devices —— 单节点设备状态

语义与 `nodes/{nodeID}/pods` 一致：节点不存在 → `404`；存在但无设备 → `200` + 空 `items`。

### 5.3 POST /api/v1/nodes/{nodeID}/device-command —— 下发设备指令

通过可靠投递向边缘下发 DeviceCommand 消息（WBS 5.3 端到端入口）。指令由边缘 Mapper 执行（内置 mock_sensor 支持 `targetTemp` / `reset`），执行结果快照写入边缘 Twin 并随周期上报回云端。

请求：

```bash
curl -X POST http://127.0.0.1:8080/api/v1/nodes/edge-node-1/device-command \
  -H 'Content-Type: application/json' \
  -d '{"deviceName":"sensor-01","property":"targetTemp","value":25}'
```

请求体：

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| deviceName | string | ✅ | 目标设备名称（路由到对应 Mapper） |
| namespace | string | 否 | 命名空间（缺省 `default`）；**参与 Mapper 路由**——路由键为 `namespace/deviceName`，同名设备按 ns 隔离，ns 不匹配任何 Mapper 时边缘拒绝 → 502 |
| property | string | ✅ | 目标属性名（如 `targetTemp`） |
| value | float64 | 否 | 期望值 |

响应 `200`（边缘已确认，且期望值已写入云端设备状态存储）：

```json
{"status":"ok","acked":true}
```

错误：`400`（缺 deviceName/property、JSON 非法）、`404`（节点离线）、`502`（边缘拒绝，含 namespace 无对应 Mapper）、`504`（确认超时）、`500`（兜底）。

> **命名空间与装配开关（v0.2.0）**：设备命名空间由 Mapper 侧声明（`DeviceNamespaceResolver` 接口）——Modbus Mapper 三级解析：`WithNamespace` 选项 > 环境变量 `EDGEFLOW_MODBUS_NAMESPACE`（默认 `default`）> `default`；mock_sensor 固定 `default`。`EDGEFLOW_EDGECORE_ENABLE_MAPPER`（默认 `true`，`false`/`0`/`off`/`no` 大小写不敏感关闭）关闭时 edgecore 不装配任何 Mapper，指令仅更新 Twin.Desired（纯影子模式）。

---

## 6. 下发类 API（可靠投递）

三个下发端点共用同一套可靠投递语义：消息进入 CloudHub 发送缓冲 → 边缘 EdgeHub 接收并处理 → 回 Ack（ok/error）→ 云端确认后返回 200。未确认则按 5s 超时重试最多 3 次。

### 6.1 POST /api/v1/nodes/{nodeID}/podsync —— 下发 Pod 配置

请求：

```bash
curl -X POST http://127.0.0.1:8080/api/v1/nodes/edge-node-1/podsync \
  -H 'Content-Type: application/json' \
  -d '{"operation":"add","pod":{"name":"nginx","namespace":"default","image":"nginx:1.25-alpine","replicas":1,"resources":{"cpuRequest":"100m","cpuLimit":"250m","memoryRequest":"64Mi","memoryLimit":"128Mi"}}}'
```

请求体：

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| operation | string | ✅ | `add` / `update` / `delete`（白名单校验） |
| pod.name | string | ✅ | Pod 名称（delete 时作为删除键之一） |
| pod.namespace | string | 否 | 命名空间（缺省 `default`） |
| pod.image | string | add/update 必填 | 容器镜像（delete 不需要） |
| pod.replicas | int | 否 | 副本数（Edged 按此保证多副本） |
| pod.resources | object | 否 | 资源诉求（WBS 6.5 v0.2.0 新增，可选；**云边契约只增不改**，旧客户端不传 = 零值 = 不限制） |
| pod.resources.cpuRequest | string | 否 | CPU 请求量，K8s 风格（如 `"250m"` 或 `"0.25"`）；request>limit → 400 |
| pod.resources.cpuLimit | string | 否 | CPU 上限（如 `"500m"`）；落地为容器 `--cpus` |
| pod.resources.memoryRequest | string | 否 | 内存请求量，K8s 风格（如 `"64Mi"`）；request>limit → 400 |
| pod.resources.memoryLimit | string | 否 | 内存上限（如 `"128Mi"`）；落地为容器 `--memory`（swap 禁用） |

> **资源字段格式与校验（v0.2.0）**：资源量格式解析失败（如 `"NaN"`/`"Inf"`/超范围/前导 `+`）→ 400；request>limit（CPU/内存分别校验）→ 400，文案含具体超标字段；`delete` 操作不携带资源字段，跳过校验。

响应 `200`：

```json
{"status":"ok","acked":true}
```

边缘行为：MetaManager 将 Pod 元数据落盘（SQLite），Edged 调谐启动容器（命名 `edgeflow-<ns>-<name>-<index>`，标签 `edgeflow.pod` / `edgeflow.namespace`）；`delete` 时按 namespace+name 删除元数据，Edged 回收容器。

**资源语义（WBS 6.5，v0.2.0）**：
- 边缘准入 `admitPodResources`：request≤limit 校验 + 超卖率校验（节点容量 × 超卖率 ≥ 已部署 request 求和 + 新请求；副本乘数计入）；**拒绝时不落盘、不建容器**，回 error Ack（带 `EDGEFLOW_RESOURCE_EXHAUSTED` 标记）→ 云端映射 **409**；其余边缘拒绝仍为 502。
- 容器落地：cpuLimit → `--cpus`、memoryLimit → `--memory`（`--memory-swap` 与 memory 同值，禁 swap）；零值字段不传参数（不限制）。
- 漂移检测：调谐循环对健康 Running 容器比对镜像（WBS 6.4）与资源限制（WBS 6.5），任一漂移即 stop+重建（每轮最多重建 1 个的滚动门控）。

### 6.2 POST /api/v1/nodes/{nodeID}/config-sync —— 下发 ConfigMap/Secret 配置

请求：

```bash
curl -X POST http://127.0.0.1:8080/api/v1/nodes/edge-node-1/config-sync \
  -H 'Content-Type: application/json' \
  -d '{"operation":"add","config":{"name":"app-config","namespace":"default","kind":"ConfigMap","data":{"key1":"value1"}}}'
```

请求体：

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| operation | string | ✅ | `add` / `update` / `delete` |
| config.name | string | ✅ | 配置名称 |
| config.namespace | string | 否 | 命名空间（缺省 `default`） |
| config.kind | string | ✅ | `ConfigMap` / `Secret`（白名单校验） |
| config.data | map[string]string | add/update 必填 | 键值数据（Secret 的 value 当前为明文存储，生产需加密，见 PROGRESS.md 待办） |

响应 `200`：`{"status":"ok","acked":true}`。

边缘行为：MetaManager SQLite 存储，键 `configs/<namespace>/<name>`（与 Pod 元数据同库）。

### 6.3 响应语义（三个下发端点通用；409 仅 podsync）

> v0.3.0 加固说明：podsync 400 分支已改 `json.Marshal` 结构化构造（与 409 同构），**响应结构不变**（仍为 `{"error":...}`），仅保证畸形输入也产出合法 JSON（commit `714d5ba`）。

| 状态码 | 响应体（示例） | 含义 |
|--------|---------------|------|
| 200 | `{"status":"ok","acked":true}` | 边缘已确认（Ack ok） |
| 400 | `{"error":"operation and pod.name are required"}` | 参数非法（含白名单校验失败；podsync 另含资源格式非法/request>limit，文案含具体超标字段） |
| 404 | `{"error":"node offline or not registered"}` | 节点未注册/离线 |
| 409 | `{"error":"EDGEFLOW_RESOURCE_EXHAUSTED: ..."}` | **仅 podsync**：边缘超卖准入拒绝（不落盘），错误带资源耗尽标记 |
| 502 | `{"error":"edge rejected ack"}` | 边缘回 error Ack（消息已送达但被拒绝） |
| 504 | `{"error":"ack timeout after retries"}` | 确认超时重试耗尽 |
| 500 | `{"error":"send failed"}` | 其他内部错误 |

### 6.4 资源调度环境变量（edgecore，WBS 6.5 v0.2.0）

| 环境变量 | 默认 | 说明 |
|----------|------|------|
| `EDGEFLOW_EDGECORE_NODE_CPU_MILLI` | 探测值（runtime.NumCPU×1000） | 节点 CPU 容量覆盖（毫核）；非法值回退探测值 |
| `EDGEFLOW_EDGECORE_NODE_MEMORY_BYTES` | 探测值（仅 Linux /proc/meminfo；非 Linux 为 0，需覆盖） | 节点内存容量覆盖（字节）；非法值回退探测值 |
| `EDGEFLOW_EDGECORE_OVERCOMMIT_CPU_RATE` | `1.5` | CPU 超卖率上限（>0）；非有限值（NaN/Inf）回退 1.5 |
| `EDGEFLOW_EDGECORE_OVERCOMMIT_MEMORY_RATE` | `1.5` | 内存超卖率上限；非有限值回退 1.5 |

---

## 7. 模型 API（v0.7.0：模型仓库 / 版本管理 / 灰度发布）

> v0.7.0 新增 17 个端点（**端点总览 14→31**）：模型 5 + 版本 6（CRUD4+activate+archive）+ 发布 5（创建/列表/详情/取消/回滚）+ 部署影子 1。全部注册于既有 `apiMux`（`cmd/cloudcore/main.go` 装配点），自动挂 `auth.Middleware`（Bearer Token，默认 off 向后兼容）+ `ledger.Middleware`（审计台账）——鉴权/审计零新代码；**既有 14 端点一行不改**。数据模型与状态机设计见 ARCHITECTURE.md 决策 R16；实现包 `cloud/pkg/modelrepo`（台账/校验/存储）+ `cloud/pkg/modelrelease`（灰度控制器/算法/部署执行器）。
> 云边协议**无新消息类型**（复用 PodSync/ConfigSync，载荷约定见 §7.4）；**边缘零代码改动**。

### 7.1 对象模型与键空间

| 对象 | 语义 | 关键字段 |
|------|------|---------|
| Model | 模型台账（一级对象，模型名唯一） | name（`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`，禁 `/`）、description（≤256）、type（≤64，开放字符串）、metadata（键≤64/值≤1024）、createdAt/updatedAt（Unix 毫秒） |
| ModelVersion | 版本台账（"Tag 即版本"） | version（字符集同 name，模型内唯一）、mirror（镜像 ref **必带 tag**）、sha256（`^sha256:[0-9a-f]{64}$`，存储统一小写）、sizeBytes（≥0）、archs（白名单 amd64/arm64，空=不限制）、status（draft/active/archived）、metadata（模型参数/阈值，发布时平铺进 config-sync） |
| ModelRelease | 灰度发布任务（异步执行） | id（UUID）、model/version（目标版本，创建时须 active）、target（nodeIDs 白名单 \| percentage 1..100）、targetNodes（**创建时物化的有序节点快照**，运行期不重算）、batchSize（≥1，默认 1）、pauseBetween（≥0ms，默认 0）、failFast（默认 true）、prevActive（回滚目标；无则 ""）、status（pending/running/succeeded/failed/canceled/rolled_back）、nextBatchAt/createdAt/startedAt/finishedAt、rollbackRequested |
| NodeReleaseResult | 逐节点执行结果（release 键下独立键） | nodeID、status（pending/deployed/failed/skipped）、version（该节点被部署到的版本）、reason（failed 原因）、batch（批次序号，1 起）、startedAt/finishedAt |
| DeploymentState | 部署影子（跨发布全局视图） | model、version、mirror、releaseID、updatedAt |

**键空间**（新增前缀 `/edgeflow/models/`，与既有键完全隔离）：`meta/<model>`、`versions/<model>/<version>`、`guards/<model>`（在途发布守卫，值=releaseID）、`releases/<releaseID>`（head，状态机 CAS 键）、`releases/<releaseID>/nodes/<nodeID>`（perNode）、`releases/<releaseID>/lock`（领跑锁租约键，TTL 默认 60s）、`deployments/<model>/<nodeID>`（部署影子）。`/edgeflow/_meta/schemaVersion` **不 bump**。

**版本状态机**（Activate/Archive API 驱动）：

```
draft ──activate──▶ active ──archive──▶ archived
  │                   ▲                    │
  └──delete──▶(删)    │ (激活时自动降级旧 active)   └──delete──▶(删)
```

- activate：仅 draft→active；**自动把当前 active 版本置为 archived**（两键 CAS 序列 + 失败补偿，ARCHITECTURE R16）；archived 不可再激活。
- archive：仅 active→archived；存在指向该版本的 pending/running 发布 → 409。
- delete 版本：仅 draft/archived（active → 409，先归档或删模型）。
- **发布/回滚不改变版本状态**：发布要求目标 active（只读校验）；回滚可部署 archived 的 prevActive（台账状态不变，由调用方按需显式 activate）。

**发布任务状态机**（控制器 + API 协作，head 键 CAS）：

```
pending ─▶ running ─┬─ 全部 deployed ──────────▶ succeeded
                    ├─ fail-fast 中止/存在失败(且跑完) ─▶ failed
                    ├─ cancel（批次边界生效）──────────▶ canceled
                    └─ rollback 置位 → 逆序执行 ───────▶ rolled_back
（成功/失败/取消 均可再 rollback；终态后可再次发布——guard 已释放）
```

### 7.2 端点明细与字段表

**POST /api/v1/models**（其余写端点的可选字段语义一致，不重复列表）：

```json
{"name": "defect-detector", "description": "缺陷检测模型", "type": "detection", "metadata": {"owner": "qa-team"}}
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| name | string | ✅ | `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`；重复 → 409 |
| description | string | 否 | ≤256 字符 |
| type | string | 否 | 开放字符串 ≤64（推荐 classification/detection/segmentation/llm/other） |
| metadata | map[string]string | 否 | 键 ≤64、值 ≤1024；键匹配 `^[A-Za-z0-9._-]+$` |

响应 200：完整 Model 对象（含 createdAt/updatedAt）。PUT 允许改 description/type/metadata（metadata 整表替换）；name/createdAt 不可变。DELETE 前置：无 active 版本、无在途发布（否则 409），级联删除 draft/archived 版本 + 部署影子（非事务，L26）。

**POST /api/v1/models/{modelName}/versions**：

```json
{"version": "v1.2.0", "mirror": "registry.example.com/edgeflow/models/defect-detector:v1.2.0", "sha256": "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", "sizeBytes": 482344960, "archs": ["amd64", "arm64"], "metadata": {"threshold": "0.8", "batchSize": "32"}}
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| version | string | ✅ | 字符集同 name；同模型重复 → 409 |
| mirror | string | ✅ | 镜像 ref，**必须带 tag**；整体匹配 `^[a-z0-9]+((\.|_|-)[a-z0-9]+)*(\:[0-9]{1,5})?(\/[a-z0-9._-]+)+(\:[A-Za-z0-9._-]+)$`（禁 `..`/连续 `/`/空白；tag 由最后一个 `:` 界定） |
| sha256 | string | ✅ | `^sha256:[0-9a-f]{64}$`（大小写不敏感接受，存储统一小写） |
| sizeBytes | int64 | 否 | >=0；缺省 0 |
| archs | []string | 否 | 元素 ∈ {amd64, arm64} 白名单，去重；空 = 不限制（F38 多架构语义） |
| metadata | map[string]string | 否 | 模型参数/阈值；发布时平铺进 config-sync（§7.4） |

响应 200：完整 ModelVersion（status=draft）。（POST 不激活——激活是显式动作。）

**POST /api/v1/models/{modelName}/releases**（创建灰度发布，异步执行）：

```json
{"version": "v1.2.0", "target": {"type": "percentage", "percentage": 25}, "batchSize": 2, "pauseBetween": 30000, "failFast": true}
```

```json
{"version": "v1.2.0", "target": {"type": "nodeIDs", "nodeIDs": ["edge-node-1", "edge-node-2"]}, "batchSize": 1, "pauseBetween": 0, "failFast": true}
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| version | string | ✅ | 目标版本；**须为 active**（否则 422） |
| target.type | string | ✅ | `nodeIDs` / `percentage` 二选一 |
| target.nodeIDs | []string | 条件必填 | 白名单；元素须匹配 `^[A-Za-z0-9._-]+$` 且**全部已注册**（否则 422，响应 `"unknownNodes":[...]`）；允许离线节点入列（运行时按离线处理） |
| target.percentage | int | 条件必填 | 1..100（越界 400）；分母 = 创建时刻 Ready 节点（0 台 Ready → 422） |
| batchSize | int | 否 | >=1，默认 1 |
| pauseBetween | int64 | 否 | 批间暂停毫秒，>=0，默认 0 |
| failFast | bool | 否 | 默认 true |

创建前置校验（依序）：模型/版本存在（404）→ 目标 active（422）→ target 格式（400）→ 白名单节点已注册 / percentage 合法（422/400）→ 物化 TargetNodes → 确定 prevActive → guard CAS + release 键 CAS（同模型在途 → 409 含在途 releaseID；**孤儿 guard 自愈**见 R16）→ perNode 全部 pending 预写。

响应 **202**：完整 ModelRelease 对象（status=pending，targetNodes 已物化，perNode 汇总：pending=N）。

**按比例分母口径与舍入**（创建时刻快照）：

| readyCount | 规则 | 例 |
|---|---|---|
| 0 | **422 拒绝创建**（`no ready nodes`） | — |
| 1 | n = 1（任何 pct 均落该节点） | 1 台 → 1 |
| ≥2 | **n = ceil(readyCount × pct / 100)**，且 1 ≤ n ≤ readyCount | 23×10%→3；3×50%→2；100×1%→1；10×100%→10 |

- 节点选择确定性：Ready 名单按 NodeID 字典序取前 n（跨副本可复现）；archs 非空时先按 node.arch ∈ version.archs 过滤再取前 n（过滤后 0 台 → 422，文案含 `no ready nodes for archs [...]`）；白名单模式不做 arch 过滤。
- **目标集合以创建时快照为准**：运行期节点掉线/新节点加入不重算；不同模式/迁移后的百分比目标集合不跨模式可比（以创建时快照为准，L29 侧注）。

**POST .../releases/{releaseID}/cancel**：响应 200 + release（status=canceled；未执行节点 ≤1 扫描周期补齐 skipped，L27）。终态 release cancel → **409**（`{"error":"release already <status>"}`）。

**POST .../releases/{releaseID}/rollback**（异步，逆序批量）：
- 前置校验：status ∈ {running, succeeded, failed, canceled}（pending/rolled_back → 409）；`prevActive != ""` 且版本存在（否则 **422** `no previous active version`）；**`release.version == 模型当前 active 版本`**（已被更新版本接管 → **409**，文案引导显式 activate 目标旧版本或发起新发布，L27）。
- 通过 → 202 + release（rollbackRequested=true）；控制器逆序逐批执行（批间 pause=0），失败不回滚中止（能回多少回多少，perNode 明细可查，L24）；完成 → rolled_back。
- **执行期复查**（主线 D2/D4）：控制器 runRollback 起始重读版本表——若执行前已被新版本接管或 prevActive 被删 → 中止：head=failed（reason 明确）+ 清除 rollbackRequested（防活锁）+ 未执行节点标 skipped；API 端 202 照旧，结果以 head 终态回读为准。

**GET 列表响应形态**：K8s List 风格（`{"kind":"ModelList","apiVersion":"v1","items":[...]}`；空为 `[]`），对齐 podStatusList/deviceStatusList。**发布详情**额外返回派生汇总：`{"summary":{"total":N,"deployed":N,"failed":N,"pending":N,"skipped":N}}`（现算，非冗余存储）。

> **v0.23.0（CLD-07/08）变更标记**：① summary 自本版起**恒现**于全部发布对象响应（含列表逐条，summary 五计数口径同上；详见 §7.12）；② `GET /api/v1/releases`（§7.9）自裸 items 数组改为 K8s List 包装 `ReleaseList`，兼容性说明见 docs/API-COMPATIBILITY.md v0.23.0 小节。

### 7.3 错误语义（202/422 为 v0.7.0 新增，其余沿用既有约定）

| 状态码 | 语义 | 场景 |
|--------|------|------|
| 200 | 成功（写类端点返回完整对象或 `{"status":"ok"}` 形态） | — |
| **202** | **已受理（异步执行）**：发布创建 / 回滚置位 | 灰度任务开始执行 |
| 400 | 请求非法：JSON 解析失败 / 缺必填 / 字符集或枚举越界 / percentage 越界 / batchSize<1 / 请求体超 1MiB（`request body too large`） | 参数错误 |
| 404 | 模型/版本/发布/部署影子不存在；**模型不存在时其子资源一律 404**；v0.22.0 起 **release 子资源跨模型引用（URL 模型名 ≠ release 归属模型）一律 404**（与"发布不存在"同语义同响应体，见 §7.11） | 资源不存在 |
| 409 | 冲突：模型/版本已存在；删除 active 版本；归档/激活状态机非法；同模型在途发布（含在途 releaseID）；CAS 冲突耗尽；cancel/rollback 目标态不合法；在途发布指向的版本被 archive；回滚被新版本接管 | 状态冲突 |
| 422 | **语义不可执行**：发布目标版本非 active / 无 Ready 节点 / 白名单含未知节点 / 无 PrevActive | 业务前置不满足 |
| 500 | 内部错误兜底（存储/序列化异常） | 服务端异常 |

响应体统一 `{"error":"<机器可读原因>", ...上下文字段}`（409 发布冲突带 `"releaseID"`，422 白名单带 `"unknownNodes":[...]`）。鉴权/审计链与既有端点完全一致（401/审计自动覆盖）。

### 7.4 config-sync 载荷约定（边缘模型版本感知，零边缘代码）

发布器对每目标节点自动执行：① podsync add（Pod 名 `edgeflow-model-<sanitized>`，`sanitize(name)` = 小写 + `.`→`-`；namespace 固定 `edgeflow`；image = 版本镜像；replicas=1——模型实例多副本由用户后续自行 podsync 编排，发布语义 = "该版本上机"）；② config-sync add（ConfigMap，同命名约定，Kind=ConfigMap）。两步均 acked → 部署影子写穿（§7.5）。

**ConfigMap 载荷约定**（`configs/edgeflow/edgeflow-model-<sanitized>`，保留键由发布器保证）：

```json
{
  "model":      "defect-detector",
  "version":    "v1.2.0",
  "mirror":     "registry.example.com/edgeflow/models/defect-detector:v1.2.0",
  "sha256":     "sha256:9f86...",
  "type":       "detection",
  "releasedAt": "1787000000000"
}
```

- `version.metadata` 全部键值**平铺追加**进 data（模型参数随版本走）；与保留键冲突 → 保留键优先 + 控制器日志 Warn。
- 推理容器挂载/读取该 ConfigMap 即得"当前模型版本与参数"；版本切换 = 下一次 config-sync 覆盖（EdgeHub 幂等去重 + MetaManager SQLite 落盘保证重启后仍是新版本元数据）。
- 发布**回滚**同样经此通道把 version 字段改回 prevActive——边缘无状态机依赖，纯声明式收敛。
- 错误映射（perNode.Reason 文案）：`node offline or not registered` / `ack timeout after retries` / `edge rejected ack` / `send failed: <err>`。

### 7.5 部署影子（云端写穿）

- 键：`/edgeflow/models/deployments/<modelName>/<nodeID>`；值 `{"model":...,"version":...,"mirror":...,"releaseID":...,"updatedAt":...}`。
- 写入时机：podsync + config-sync **均 acked 后**；写穿失败 → 日志 Warn（下发已生效，仅影子视图缺该记录，release/perNode 已持久化不受影响）。
- 语义：云端期望态（对标设备 Desired）；`GET /api/v1/models/{name}/deployments` 提供"版本—节点—时间"追踪（F41 台账）；与边缘实际运行版本（PodStatus 上报）分离；重启后从 etcd 恢复（embed/外部），纯内存模式重启丢失（L22）。
- **影子 = 派生台账整值覆盖，无 CAS 需求**——与 Desired（权威期望态，modRevision CAS）的差异为**有意设计**（同 (model,node) 键写者被 guard + release 锁 + 终态释放次序串行化；P9/审稿线索 3 口径，见 ARCHITECTURE R16）。
- 模型删除 → `DeleteRange(/edgeflow/models/deployments/<model>/)` 级联。

---

# 第二部分 CRD 类型定义（apis/edge/v1alpha1）

> 代码位置：`apis/edge/v1alpha1/`（Group `edgeflow.io`，Version `v1alpha1`）。
> 此部分为 M0-2 已定稿内容，随 v0.1.0 一并归档。

### 7.6 v0.16.0 增量：定时维护窗口 / 暂停恢复 / 目录导出导入

**新增端点（4）**：

| 端点 | 语义 |
|------|------|
| `POST /api/v1/models/{m}/releases/{id}/pause` | running→paused；重复幂等返回 200；pending/终态→409。批边界生效，不中断在途下发；paused 保 active 身份续租领跑锁 |
| `POST /api/v1/models/{m}/releases/{id}/resume` | paused→running；非 paused→409。NextBatchAt 保持原值（过期即推进、未到守 PauseBetween） |
| `GET /api/v1/models/export` | models+versions 全量快照 JSON：`{schemaVersion:"1", exportedAt, models[], versions[]}`；Content-Disposition 附件提示 |
| `POST /api/v1/models/import` | 幂等 upsert：模型存在→元数据覆盖（modelsUpdated）；版本同 (model,version)→跳过（versionsSkipped）；active 经 draft+activate 直通灾备语义；响应 `{kind:"importReport", modelsImported, modelsUpdated, versionsImported, versionsSkipped}` |

**创建参数增量**：`notBeforeMs`（Unix 毫秒，opt-in，0=立即）——窗口未到控制器不认领不占领跑锁；校验 ≥0 且 ≥now-5min。

**状态机扩展**：running ⇄ paused（`CanTransitionRelease` 只增扩展）；paused 属 InFlight（占 guard、非终态）；CancelRelease 接受 paused；RequestRollback 拒 paused（先 resume 或 cancel）。

### 7.7 v0.17.0 增量：运行中可调参数 / status 过滤 / dryRun 预检

**新增端点（1）**：

| 端点 | 语义 |
|------|------|
| `PATCH /api/v1/models/{m}/releases/{id}` | 运行中可调执行参数：请求体 `{batchSize?, pauseBetween?, failFast?}`（全指针=部分更新，未提供保持；至少一个必填）。pending/running/paused 可改、终态 409、不存在 404、值非法 400（batchSize≥1 / pauseBetween≥0）。生效为**批边界**语义：控制器每轮扫描重读 head 重切批次，下一批按新参数执行，不中断在途批。CAS 安全（复用 UpdateReleaseHead，冲突重试 ≤3 → 409）。响应 200 + 发布详情（含 summary） |

**既有端点增量（不新增路由）**：

| 端点 | 增量 |
|------|------|
| `GET /api/v1/models/{m}/releases` | 新增 `status` 查询参数：逗号分隔多值过滤（如 `running,pending`），合法枚举=pending/running/paused/succeeded/failed/canceled/rolled_back，含非法值 → 400；先过滤后分页，X-Total-Count 报过滤后总数 |
| `POST /api/v1/models/{m}/releases` | 请求体新增 `dryRun`（bool，缺省 false）：true 时全量执行真实校验链 + guard 等价只读判定，**零落盘零 guard 键零 perNode 预写**；校验失败同真实创建同因同报（400/404/422 族）；通过 → 200 `{kind:"DryRunPreview", wouldCreate, blockReason?, targetNodes, prevActive, inFlightReleaseId?, checkedAt}`。预检结论为 TOCTOU 快照非承诺语义 |

### 7.8 v0.18.0 增量：失败预算自动暂停 / 发布事件时间线 / 全局部署影子

**新增端点（1）**：

| 端点 | 语义 |
|------|------|
| `GET /api/v1/deployments` | 跨模型聚合全部部署影子；可选 `model=`（精确，非法名 400）与 `nodeID=` 过滤；过滤后分页、`X-Total-Count` 报过滤后总数；先 Model 再 NodeID 字典序。既有 per-model 端点不变 |

**既有对象增量**：

- 创建参数 `failureBudget`（int，opt-in；0=禁用行为不变）：批完成后 failed 计数 ≥ 预算且未终态 → 自动暂停（paused + AutoPausedAt + autopause 事件）；resume 后续跑剩余批次。仅 failFast=false 模式下有累计窗口。
- 发布对象新增 `events` 数组（环形上限 32 条丢最旧）：{at, kind, detail?}；kind ∈ created/paused/resumed/cancelled/terminal/autopause/batch_done/rollback_requested（开放集合，消费方容忍未知值）。追加在 UpdateReleaseHead CAS 闭包内并发安全；随 export/import 快照迁移。

### 7.10 v0.20.0 增量：失败节点重试 / 终态发布归档删除 / releaseNotes 元数据

**新增端点（2）**：

| 端点 | 语义 |
|------|------|
| `POST /api/v1/models/{modelName}/releases/{releaseID}/retry` | 失败节点重试（v0.20.0）：仅终态可 retry；克隆新发布 RetryOf 回指原发布；TargetNodes=原发布 failed 子集（`nodeIDs` 可选缩围，不在集合内 400 带 failedNodes）；版本须仍 active（否则 422）；无 failed 节点 422；空 body 合法（缺省全部 failed） |
| `DELETE /api/v1/models/{modelName}/releases/{releaseID}` | 终态发布归档删除（v0.20.0）：仅 succeeded/failed/canceled/rolled_back 可删；非终态 409（与 GC 同源「在途绝不删」）；200 响应被删快照（id/status/retryOf）；删除不可逆 |

**既有语义增量**：

- 创建请求体新增可选 `releaseNotes`（≤1024 字节；超限 400）——头内嵌持久化，list/get/snapshot/global 全读取路径透出；PATCH 白名单不含该字段（创建期定死）；retry 克隆自动继承。
- 校验增量：ValidateCreate 增加 notes 长度与 RetryOf 卫生卡口；原始发布被删后 RetryOf 回指不悬垂（审计线索非外键）。

### 7.9 v0.19.0 增量：failureBudget 运行中可调 / 发布审计快照 / 全局发布查询

**新增端点（2）**：

| 端点 | 语义 |
|------|------|
| `GET /api/v1/models/{modelName}/releases/{releaseID}/snapshot` | 发布审计快照：kind=ReleaseSnapshot + generatedAt + release 头（含 events）+ summary 五计数实时现算（total/deployed/failed/skipped/pending） + nodes 恒非 nil；非承诺语义（generatedAt 后写入不在内）；跨模型引用 404 |
| `GET /api/v1/releases` | 全局发布查询：跨模型聚合；`status=` 七态逗号多值过滤（非法 400）；`limit=` 缺省 100 上限 500、`offset=`≥0；X-Total-Count 报过滤后总数；CreatedAt 降序 tie-break by ID。**v0.23.0（CLD-08）响应形态变更**：自裸 `items` 数组改为 K8s List 包装 `{kind: ReleaseList, apiVersion: edgeflow.io/v1alpha1, items:[发布对象…]}`，items 逐条含 summary（见 §7.12；兼容性说明见 API-COMPATIBILITY v0.23.0 小节） |

**既有端点增量**：

- PATCH 可调参数白名单扩展 `failureBudget`（v0.17.0 端点复用）：批边界生效、改小下一批后立即适用、0=关闸；值域 [0,10000]；终态 409 不变。AutoPause 起算口径="自当下起的剩余批次"。


### 7.11 v0.22.0 增量：release 子资源归属校验统一 / 灰度独占（canary）guard 语义登记

**归属校验统一（CLD-06，描述性——零新增端点，端点总数 42 不变）**：

- 全部 10 个 release 子资源端点（`/api/v1/models/{m}/releases/{id}` 及其
  `/digest`、`/snapshot`、`/cancel`、`/retry`、`/pause`、`/resume`、
  `/rollback` 子路径，PATCH 与 DELETE 本体）现按同一口径校验归属：
  URL 模型名 `{m}` 必须与 release 头的 `model` 字段一致，**跨模型引用
  （模型 m1 的 URL 下引用 m2 的 release id）一律 404**，错误语义与
  "发布不存在"完全一致（`ErrReleaseNotFound` 同因同响应体）——防止
  跨模型 id 枚举与误操作。
- 校验链序：模型存在（404 先行）→ release 头存在（404）→ 归属一致
  （404）。v0.19.0 snapshot / v0.20.0 retry / v0.20.0 DELETE 三个端点
  原本即内联该校验；v0.22.0 把 GET 详情、GET digest、cancel、pause、
  resume、rollback、PATCH 七个端点统一接入同语义 helper（`ownedRelease`），
  行为收敛后全部 10 端点一致。

**灰度独占（canary）guard 语义（CLD-04，既有行为登记——不改端点不改响应结构）**：

- 同模型**同一时刻至多一个在途发布**（guard create-if-absent CAS 独占：
  pending/running/paused 均算在途；终态即释放）。这是灰度发布的独占
  语义：新发布（含 retry 克隆）创建时若同模型已有在途任务 → 409 拒绝，
  防两条灰度批次并发写同一节点的部署状态。
- guard 冲突的响应可承载冲突原因：409 响应体统一为
  `{"error":"<机器可读原因>","releaseID":"<在途发布ID>"}`（§7.3 既定
  响应体约定，`releaseID` 字段即冲突在途任务回指，运维可直接对其实施
  cancel/pause 后重试创建）。
- 相关端点：`POST /api/v1/models/{modelName}/releases`（创建，202 前置
  guard）、`POST .../releases/{id}/retry`（克隆创建）、rollback 置位
  （RequestRollback 同 guard 族）。冲突均在创建/置位瞬间同步返回 409，
  不存在异步竞态窗口。

### 7.12 v0.23.0 增量：响应形态口径统一 / 错误码映射表（CLD-07/08/12 收敛登记）

**响应形态口径统一（CLD-07/08，描述性——零新增端点，端点总数 42 不变）**：

- **summary 恒现（CLD-07）**：`releaseResponse.summary` 去 omitempty，
  发布对象形态（创建 202 响应 / GET 详情 / cancel / rollback / retry
  响应 / 列表逐条）统一携 `summary` 五计数（total/pending/running/
  succeeded/failed）。零值发布（total=0）与字段缺省自此可区分；旧客户端
  按 JSON 宽容语义忽略新增恒现字段，零破坏。
- **全局发布列表包装（CLD-08）**：`GET /api/v1/releases` 自裸 `items`
  数组改为 K8s List 包装 `{"kind":"ReleaseList","apiVersion":
  "edgeflow.io/v1alpha1","items":[...]}`，items 逐条与发布对象形态统一
  （含 summary）。分页语义不变（status 过滤 / limit≤500 / X-Total-Count /
  CreatedAt 降序 tie-break by ID）。老客户端若直接迭代顶层键需适配包装
  （读 `items` 字段）；分页头与排序语义零变化。
- **retry 422 文案细分（CLD-12）**：`POST .../releases/{id}/retry` 对
  终态归档版本的 422 文案携带 `orig.Status`（如
  `retry of release <id> (status=<orig.Status>): target version ... must be
  active`）；状态码与校验链序不变。

**错误码映射表（modelError 权威映射，与 `cmd/cloudcore/model_api.go`
`modelError` 逐条对齐）**：

| HTTP | 触发错误（modelrepo 哨兵/错误类型） | 语义 | 上下文字段 |
|------|--------------------------------------|------|------------|
| 404 | `ErrModelNotFound` / `ErrVersionNotFound` / `ErrReleaseNotFound` | 模型/版本/发布不存在（子资源随模型 404；v0.22.0 跨模型引用同语义） | — |
| 409 | `ReleaseConflictError`（带 InFlight） | 同模型在途发布独占（guard create-if-absent） | `releaseID` = 在途发布 ID |
| 409 | `ErrModelExists` / `ErrVersionExists` | 模型/版本已存在 | — |
| 409 | `ErrModelHasActiveVersion` / `ErrVersionActive` / `ErrVersionNotActive` / `ErrVersionNotDraft` | 状态机非法（拒删 active / 激活·归档·删除前置不满足） | — |
| 409 | `ErrReleaseConflict` / `ErrReleaseTerminal` / `ErrVersionMismatch` / `ErrConcurrentConflict` | 发布冲突族（cancel/rollback 目标态非法、CAS 冲突耗尽、版本被接管/不匹配） | — |
| 422 | `UnknownNodesError`（带 Nodes） | 白名单含未知节点 | `unknownNodes` = [...] |
| 422 | `ErrNoReadyNodes` / `ErrNoPrevActive` | 无 Ready 节点 / 无 PrevActive 可回滚 | — |
| 400 | `ValidateModelName` / `ValidateVersionTag` / `ValidateMirror` / `ValidateSha256` / `ValidateArchs` 等 validate* 纯函数 | 字符集/格式/枚举越界（设计 §8.1） | — |
| 500 | default 兜底 | 存储/序列化/内部异常（日志含原因，响应体仅 `internal error`）；v0.23.0 起 rand 失败（发布 ID 生成）亦显式 500（CHN-16） | — |

> 映射实现唯一权威：`cmd/cloudcore/model_api.go modelError`；本表为
> 文档对齐，若表与代码漂移以代码为准并回改本表。

## A. Group / Version 约定

| 项 | 值 | 说明 |
|----|----|------|
| Group | `edgeflow.io` | 统一 API 分组（对标 KubeEdge 的 `devices.kubeedge.io` / `edge.kubeedge.io`） |
| Version | `v1alpha1` | 初版；v1alpha 阶段不保证 API 兼容，后续可能演进到 v1 |
| apiVersion | `edgeflow.io/v1alpha1` | 由 `SchemeGroupVersion.String()` 生成 |
| Kind | `EdgeNode` / `DeviceModel` / `Device` | 三种资源种类 |

- 代码常量：`GroupName = "edgeflow.io"`、`Version = "v1alpha1"`、`SchemeGroupVersion`（见 `apis/edge/v1alpha1/group_version.go`）
- 命名空间：`DeviceModel` 与引用它的 `Device` 须在同一命名空间（当前由文档约束，后续由校验器强制）
- 时间字段统一使用 RFC3339 格式字符串（如 `2026-08-13T12:00:00Z`），零依赖阶段不引入 `metav1.Time`

## B. EdgeNode（边缘节点）

对标 KubeEdge 的 Node 相关资源（`edge.kubeedge.io`，此处为简化版）。
边缘节点是资源承载者：设备绑定到节点，云端通过 Status 感知节点在线状态。

### B.1 Spec 字段表

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| nodeID | string | 业务必填 | 节点唯一标识（对标 KubeEdge `spec.nodeID`），注册时生成，用于云边通信鉴权 |
| role | string | 否 | 节点角色：`edge`（默认）/ `cloud` |
| addresses | NodeAddress[] | 否 | 网络地址列表 |
| addresses[].type | string | 是 | 地址类型：`InternalIP` / `Hostname` / `DNS` |
| addresses[].address | string | 是 | 地址值，如 `192.168.1.10` |

### B.2 Status 字段表

| 字段 | 类型 | 说明 |
|------|------|------|
| phase | string | 生命周期阶段：`Pending`（默认）/ `Running` / `Offline` / `Unknown` |
| heartbeatTime | string (RFC3339) | 最近一次心跳时间，edgecore 周期性上报 |
| lastSeenTime | string (RFC3339) | 云端最后一次观测到节点的时间 |
| conditions | NodeCondition[] | 健康条件列表 |
| conditions[].type | string | 条件类型，如 `Ready` |
| conditions[].status | string | `True` / `False` / `Unknown` |
| conditions[].reason | string | 机器可读原因（如 `HeartbeatTimeout`） |
| conditions[].message | string | 人类可读说明 |
| conditions[].lastTransitionTime | string (RFC3339) | 条件最近一次状态变化的时间 |
| version | string | edgecore 版本号，便于云端识别旧版本节点 |

### B.3 默认值（SetDefaults）

- `role` 为空 → `edge`
- `phase` 为空 → `Pending`

## C. DeviceModel（设备型号）

对标 KubeEdge DeviceModel（`devices.kubeedge.io/v1alpha2`）。
描述一类设备的"模板"：协议家族 + 属性定义，不含具体设备实例。

### C.1 Spec 字段表

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| protocol | string | 否 | 协议家族：`modbus` / `opcua` / `bluetooth` / `mqtt` 等 |
| properties | DeviceProperty[] | 否 | 属性列表 |
| properties[].name | string | 是 | 属性名（同一型号内唯一） |
| properties[].description | string | 否 | 属性的人类可读说明 |
| properties[].dataType | string | 是 | 数据类型：`int` / `string` / `double` / `float` / `boolean` |
| properties[].accessMode | string | 否 | 访问模式：`ReadOnly` / `ReadWrite`（默认 `ReadWrite`） |
| properties[].defaultValue | string | 否 | 默认值（字符串形式，与 dataType 对应） |
| properties[].minimum | string | 否 | 数值型属性最小值 |
| properties[].maximum | string | 否 | 数值型属性最大值 |
| properties[].unit | string | 否 | 计量单位，如 `celsius` |

### C.2 默认值（SetDefaults）

- 属性 `accessMode` 为空 → `ReadWrite`（默认允许云端下发期望值）

## D. Device（设备实例）

对标 KubeEdge Device（`devices.kubeedge.io/v1alpha2`），数字孪生（Twin）机制的核心：
云端下发期望值（desired），设备上报实际值（reported）。

### D.1 Spec 字段表

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| deviceModelRef | string | ✅ 是 | 引用的 DeviceModel 名称（须同命名空间） |
| nodeName | string | ✅ 是 | 绑定的边缘节点名称（对标 KubeEdge `spec.nodeName`） |
| protocol | ProtocolConfig | 否 | 设备连接信息 |
| protocol.protocolName | string | 否 | 协议名，应与型号的 protocol 一致 |
| protocol.config | map[string]string | 否 | 连接参数（键值对）：串口波特率、MQTT 地址等 |
| properties | DevicePropertySpec[] | 否 | 设备实例上各属性的期望值（属性名须在型号中定义） |
| properties[].name | string | 是 | 属性名 |
| properties[].desired | PropertyValue | 否 | 云端下发的期望值 |
| desired.value | string | 是 | 属性值（字符串形式，与型号 dataType 对应） |
| desired.metadata | map[string]string | 否 | 附加元数据（如采集时间、单位） |

### D.2 Status 字段表

| 字段 | 类型 | 说明 |
|------|------|------|
| twins | TwinProperty[] | 数字孪生属性列表（desired 与 reported 对照） |
| twins[].propertyName | string | 属性名 |
| twins[].desired | PropertyValue | 期望值 |
| twins[].reported | PropertyValue | 设备实际上报值 |
| lastUpdatedTime | string (RFC3339) | 最近一次状态更新 |

### D.3 默认值

- 必填字段（`deviceModelRef` / `nodeName`）**故意不提供默认值**，防止"看起来能跑、实际绑错对象"的隐性错误（有对应测试约束）。

## E. 与 KubeEdge 对标说明

| EdgeFlow | KubeEdge 对应资源 | 差异与简化 |
|----------|-------------------|------------|
| EdgeNode | Node（`edge.kubeedge.io`） | 仅保留 nodeID / role / addresses / status 核心字段；KubeEdge 的 certID 等字段暂不定义 |
| DeviceModel | DeviceModel（`devices.kubeedge.io/v1alpha2`） | 属性定义基本一致；顶层 protocol 字段对标 KubeEdge v1alpha1 的 protocolType 思路 |
| Device | Device（`devices.kubeedge.io/v1alpha2`） | desired / reported / twins 机制一致；**省略 propertyVisitors**（寄存器/地址映射，Mapper 阶段再补）；protocolConfig 用扁平键值对代替 KubeEdge 的结构化配置 |
| 数字孪生 | KubeEdge Twin 机制 | 语义一致：desired 云端下发、reported 设备上报 |
| ObjectMeta | metav1.ObjectMeta | 最小子集（名称/命名空间/标签/注解/UID/版本/创建时间），零依赖实现 |

### E.1 设计决策说明

- **DeviceModel 顶层保留 protocol**：KubeEdge v1alpha2 将协议信息放在 Device 上；EdgeFlow 在型号上声明协议家族（便于按协议类型筛选设备、驱动 Mapper 选型），连接参数仍在 Device 上配置。
- **时间字段用字符串**：零依赖阶段避免引入 `metav1.Time`；接入 Kubernetes 后替换。
- **必填字段不设默认值**：Device 的 `deviceModelRef` / `nodeName` 缺失时保持为空，便于校验器报错。

## F. 示例 YAML（示意，尚未在真实集群验证）

### F.1 EdgeNode

```yaml
apiVersion: edgeflow.io/v1alpha1
kind: EdgeNode
metadata:
  name: edge-node-1
  labels:
    zone: a
spec:
  nodeID: edge-node-1-uuid
  role: edge
  addresses:
    - type: InternalIP
      address: 192.168.1.10
status:
  phase: Running
  heartbeatTime: "2026-08-13T12:00:00Z"
  conditions:
    - type: Ready
      status: "True"
```

### F.2 DeviceModel

```yaml
apiVersion: edgeflow.io/v1alpha1
kind: DeviceModel
metadata:
  name: temp-sensor-model
spec:
  protocol: modbus
  properties:
    - name: temperature
      description: 温度（摄氏度）
      dataType: int
      accessMode: ReadOnly
      minimum: "-10"
      maximum: "100"
      unit: celsius
```

### F.3 Device

```yaml
apiVersion: edgeflow.io/v1alpha1
kind: Device
metadata:
  name: temp-sensor-01
spec:
  deviceModelRef: temp-sensor-model
  nodeName: edge-node-1
  protocol:
    protocolName: modbus
    config:
      serialPort: "1"
      baudRate: "115200"
  properties:
    - name: temperature
      desired:
        value: "30"
status:
  twins:
    - propertyName: temperature
      desired:
        value: "30"
      reported:
        value: "29.5"
  lastUpdatedTime: "2026-08-13T12:00:00Z"
```

---

# 第三部分 已知缺口与后续（v0.1.0 归档说明）

## 8. 后续接入 Kubernetes 需要做的事

1. **引入 k8s.io/apimachinery**，为三个资源实现 `runtime.Object` 接口（`DeepCopyObject()`），并将手写 DeepCopy 替换为 controller-gen 生成版本
2. **添加 kubebuilder marker**（`// +kubebuilder:object:root=true`、`// +kubebuilder:subresource:status`、字段校验 marker），生成 CRD YAML 与 OpenAPI schema（ROADMAP 1.4 完成标准：CRD 可 `kubectl apply`）
3. **ObjectMeta 替换**为 `metav1.ObjectMeta`（UID / ResourceVersion 等由 apiserver 维护）
4. **校验落地**：必填字段、属性名须存在于型号、协议名一致性等，通过 CRD schema validation 或 admission webhook 强制
5. **默认值迁移**：SetDefaults 逻辑迁移为 CRD schema `default` 或 mutating webhook

## 9. 已知限制（v0.1.0 定稿时确认）

> v0.7.0 追加：模型 API 相关已知限制登记见下表新增 3 行（编号以 KNOWN-ISSUES.md §7 为准）。


| 限制 | 影响 | 计划 |
|------|------|------|
| 云端状态**分级持久化**（v0.4.0 起） | 注册元数据（节点台账）与设备 Desired **跨重启保留**（嵌入式 etcd 同步写穿，`EDGEFLOW_CLOUDCORE_ETCD_ENABLED=true` 默认启用）；Pod 状态与上报属性（properties/reported）**重启后短暂清空**（≤1 上报周期，边缘重连后自愈，**非永久丢失**）；心跳/Status 不落盘（重启后待首次心跳翻新） | v0.4.0 起已消除"重启清空"整体性限制；对接 K8s apiserver 后进一步统一 |
| nodeID 字符约束（v0.4.0 新增硬约束） | nodeID 必须匹配 `^[A-Za-z0-9._-]+$`；namespace/podName/deviceName 不得含 `/`。含 `/` 的 nodeID 写入（注册、device-command、GC 级联删除）被**拒绝并告警**（防破坏 etcd 键空间前缀扫描）。现有边缘 nodeID 为 UUID/主机名形态，不受影响 | 协议/部署文档登记（API-SPEC §1、DEPLOYMENT §10） |
| `/api/v1/nodes` 与 `/api/v1/edgenodes` 双视图并存 | 两种响应形态，客户端需按端点区分 | 属设计取舍（运行视角 vs CRD 视角），v0.1.0 保留 |
| device-command 的 value 为 float64 | 非数值属性（string/boolean）暂无法通过本端点下发 | Mapper 扩展时评估 |
| config-sync 的 Secret value 明文传输存储 | 生产环境需加密 | PROGRESS.md 待办 |
| 广播/组播下发（Target="*"）路由层已支持，API 层未暴露 | 无批量下发端点 | 后续版本 |
| （v0.7.0）纯内存模式 release 任务与部署影子**重启丢失**（embed/外部 etcd 持久恢复） | 纯内存（`ETCD_ENABLED=false`）下发布任务/影子为内存态，重启清空明示 | KNOWN-ISSUES L22；生产建议 embed/外部模式 |
| （v0.7.0）半部署状态：podsync 成功、config-sync 失败 → 节点已切镜像未切参数，计 failed | 边缘声明式调谐最终一致；重试发布或回滚收敛 | KNOWN-ISSUES L23；perNode reason 可查 |
| （v0.7.0）回滚部分失败仍置 rolled_back（尽可能回滚，不中止） | 失败节点 perNode 明细 + Warn 日志；人工复核 | KNOWN-ISSUES L24 |

## 10. 归档信息

- 定稿版本：v0.1.0（2026-08-14）
- 评审记录：`docs/REVIEWS.md` §9.2（评审人、已知问题、归档状态）
- 相关文档：`docs/ARCHITECTURE.md`（9.1）、`docs/DEPLOYMENT.md`（9.3）、`docs/HANDOFF.md`（9.4）、`examples/README.md`（9.5 温度传感器 Demo 教程）

---

# 第四部分 共享库与协议包 API 边界（v0.3.0 新增）

> 本节登记 v0.3.0 新增/变更的库级 API 与 `pkg/opcua` 包边界，供 Mapper/上层模块消费方与测试方引用。代码即契约，本节为摘要。

## 11. pkg/log.SetOutput（v0.3.0）

| 签名 | 说明 |
|------|------|
| `func SetOutput(w io.Writer)` | 设置日志输出目标（默认 stderr）。传 `nil` **恢复 stderr**（兼容 stdlib `log.SetOutput` 约定）。供测试捕获输出（如 `bytes.Buffer`），生产代码无需调用 |

- 新增于 commit `714d5ba`；与既有 `SetLevel`/`GetLevel`/`Debugf`（v0.2.0）无交互约束。

## 12. edge/pkg/edgehub Options.BackoffSleepFunc（v0.3.0）

| 字段 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `BackoffSleepFunc` | `func(d time.Duration) bool` | nil=内置默认退避休眠 | 重连退避休眠实现注入点：每次退避被调用一次，返回 false 中止重连；nil 时由客户端内置实现接管（与 v0.2.0 行为逐字节一致）。测试注入计数/加速实现，替代实时时间阈值断言 |

- 新增于 commit `714d5ba`（KNOWN-ISSUES §1 ② 闭环）；`bool` 返回值与客户端 `shuttingDown` 中止语义对齐。

## 13. pkg/opcua 包边界（v0.3.0 M1，UA Binary 协议栈核心）

> 零第三方依赖（纯标准库），OPC UA Part 6（UA Binary）。SecurityPolicy **None 明文**：无认证/完整性，仅限可信隔离网络（封闭 OT 网段/本机模拟），禁止暴露到不可信网络。

### 12.1 导出符号（已实现）

| 类别 | 符号 |
|------|------|
| 连接 | `Dial(endpoint string) (*Conn, error)`、`DialTimeout(endpoint string, timeout time.Duration) (*Conn, error)`——TCP→HEL→ACK 三态协商，返回就绪 Conn；`DefaultDialTimeout`（10s） |
| Conn 方法 | `ReadMessage` / `WriteMessage`（单 chunk 帧级 I/O，长度溢出读体前拒绝）；Conn 为裸传输（ChannelId 恒 0，SecureChannel 未打开） |
| 常量 | 消息类型 `MsgHello`(HEL)/`MsgAcknowledge`(ACK)/`MsgError`(ERR)/`MsgOpenSecureChannel`(OPN)/`MsgSecureMessage`(MSG)/`MsgCloseSecureChannel`(CLO)；chunk `ChunkFinal`(F)/`ChunkIntermediate`(C)/`ChunkAbort`(A)；`DefaultProtocolVersion`/`DefaultSendBufferSize`/`DefaultReceiveBufferSize`/`DefaultMaxMessageSize`/`DefaultMaxChunkCount`/`MinBufferSize`；Status 常量 `StatusGood`/`StatusUncertain`/`StatusBad` 系 |
| 类型 | `NodeId`（+`NewNodeID`/`NewStringNodeID`/`NewByteStringNodeID`/`NewGuidNodeID`，Part 6 Table 5 全形式）、`NodeIdType`、`Variant`（`NewVariant`/`NullVariant`，type-mask，标量+数组）、`DataValue`（双时间戳+皮秒）、`ExtensionObject`、`Guid`、`ByteString`、`DateTime`（`DateTimeFromTime`）、`LocalizedText`（`NewLocalizedText`/`NewLocalizedTextWithLocale`）、`QualifiedName`、`StatusCode`、`Severity`、`MessageHeader`（`EncodeHeader`/`DecodeHeader`）、`Hello`/`Acknowledge`/`ErrorMessage`（`DecodeHello`/`DecodeAcknowledge`/`DecodeError`） |
| 错误 | `ErrChunkingUnsupported`（中间 chunk 拒绝，MaxChunkCount=1）、`ErrMessageTooLarge`、`ErrTooLong`、`ErrInvalidEncoding`、`ErrUnsupportedType` |

### 12.2 明确未实现（后续里程碑，见 docs/KNOWN-ISSUES.md §3）

- Read/Write/Subscribe 等任何服务请求（无服务层）；
- SecureChannel 打开/关闭（OPN/CLO 消息常量已定义，未实现会话层）；
- 安全策略 Sign/SignAndEncrypt（仅 SecurityPolicy None 明文）；
- UA 节点模型/对象树、Discovery 端点；
- ExpandedNodeId 的 namespace-URI/server-index 形式、XmlElement、DiagnosticInfo 完整位域（当前为空骨架）；
- 消息分块（MaxChunkCount=1，中间 chunk 拒绝）；
- Mapper 层客户端 API（后续里程碑）。

### 12.3 互操作状态

- 本轮仅自研 mock 对端回环验证（transport_test 真实 TCP 握手）；**未**与第三方 UA 栈（open62541/node-opcua）互操作验证——下一里程碑安排 cross-check。

> **v0.14.0 更新**（2026-08-27，OPC-UA 里程碑第二阶段）：§12.1 导出符号与 §12.2 未实现清单已大幅演进——新增 SecureChannel 层（`Conn.OpenSecureChannel`/`SecureChannel`）、Session 匿名会话与 Read/Write 服务消息、高层 `Open`/`Client` API、`DiagnosticInfo` 位域完整实现、`ParseNodeID`、服务端互操作面（`Encode*/Decode*` 包装）；详情见下节 §13 与 docs/OPCUA-GUIDE.md。"Mapper 层客户端 API（后续里程碑）"已闭环（mappers/opcua 落地）。

> **v0.15.0 更新**（2026-08-27，OPC-UA 里程碑第三阶段）：Browse 与 Subscription 已闭环（§13.4）；仍未实现：Sign·SignAndEncrypt 安全策略 / 事件订阅 / 第三方栈互操作。全部请求解码器新增“字节全消费”严格校验（试解防误吞），对正确编码的合法对端透明。

## 13. pkg/opcua 包边界（v0.14.0 第二阶段，端到端协议栈）

> 在 v0.3.0 M1 基础上补齐服务层与客户端 API，零第三方依赖保持；SecurityPolicy None 边界不变。文档：docs/OPCUA-GUIDE.md。

### 13.1 新增导出符号（v0.14.0）

| 类别 | 符号 |
|------|------|
| SecureChannel | `(*Conn).OpenSecureChannel(timeout) (*SecureChannel, error)`（OPN→返回就绪通道，channelId 生效）、`SecureChannel.Close()`（CLO+TCP 关闭）、`ChannelID()/TokenID()/RequestID()` |
| 安全头 | `AsymmetricSecurityHeader` / `SymmetricSecurityHeader` / `SequenceHeader`（Encode/Decode 导出包装于 server_api.go）、`SecurityPolicyNoneURI`、`DefaultRequestedLifetime` |
| OPN | `OpenSecureChannelRequest/Response`、`ChannelSecurityToken`、`SecurityTokenRequestTypeIssue`（Decode/Encode 导出） |
| 服务消息 | `CreateSessionRequest/Response`、`ActivateSessionRequest/Response`（匿名：`AnonymousIdentityToken()`）、`CloseSessionRequest/Response`、`ReadValueId`/`ReadRequest/Response`、`WriteValue`/`WriteRequest/Response`、`RequestHeader`/`ResponseHeader`；TypeId 常量 `ServiceCreateSessionRequest`(461) 等（概念标识，不落线上） |
| 客户端 | `Open(endpoint, timeout) (*Client, error)`（Dial→OPN→CreateSession→ActivateSession 全链路）、`(*Client).Read([]NodeId) ([]DataValue, error)`（批量读，Results 一一对应）、`(*Client).Write(NodeId, Variant) (StatusCode, error)`（单点写）、`(*Client).Close()`（CloseSession→CLO→TCP）、`DefaultClientTimeout`（5s） |
| 类型补全 | `DiagnosticInfo`（位域完整实现含递归；Variant 内嵌 0x19 保持 ErrUnsupportedType）、`MaxArrayLength` |
| 配置解析 | `ParseNodeID(s string) (NodeId, error)`（五形式，与 NodeId.String() 互逆；纯数字宽容 ns=0） |
| 服务状态码 | `StatusBadNodeIdUnknown`/`StatusBadAttributeIdInvalid`/`StatusBadNothingToDo`、`AttributeIdValue`(13) |
| 服务端互操作面 | server_api.go 的 `Encode*/Decode*` 导出包装（供 pkg/opcuasim/服务器适配使用） |

### 13.2 v0.14.0 行为要点

- Open 全链路任一步失败即清理已建资源并返回错误；传输层失败由上层 Mapper 重连（与 Modbus withConn 同构）
- Read 结果按请求顺序一一对应；节点不存在返回 BadNodeIdUnknown 的 DataValue（不报错，调用方按 Status 过滤）
- Write 返回服务端结果状态码；Good=写入被接受（Mapper 另做回读验证）
- MSG 响应按 SequenceHeader.RequestId 关联（读帧上限 32 防死循环）

### 13.3 设备接入层（mappers/opcua + pkg/opcuasim）

- `mappers/opcua`：`New(endpoint, opts...)`（WithPoints/WithDeviceName/WithNamespace/WithTimeout/WithLedger）、`ParseNodes(s)`（EDGEFLOW_OPCUA_NODES 解析：逗号分隔 name=nodeId，ns= 前缀条目退化为 nodeId 字符串）、Collect 批量读点转换契约（数值/Bool/String-ParseFloat 支持，其余跳过+Warn）、HandleCommand 写点回读验证（容差 1e-6）
- env opt-in：`EDGEFLOW_OPCUA_ENDPOINT`（非空注册）/`EDGEFLOW_OPCUA_NODES`/`EDGEFLOW_OPCUA_DEVICE_NAME`（默认 opcua-device-01）/`EDGEFLOW_OPCUA_NAMESPACE`
- `pkg/opcuasim`：模拟服务器（默认 127.0.0.1:14840，6 点位动态模型；入口 hack/opcua-sim；API New/Start/Stop/NodeTable/WithStep/WithSeed/WithMaxConns/WithEndpointURL）


## 7.7 v0.13.0 增量：deployments 分页与节点 DTO 字段

### deployments 列表分页（A′）

`GET /api/v1/models/{modelName}/deployments` 新增 query 参数（与 releases/versions/models 同构）：

| 参数 | 类型 | 语义 | 非法值 |
|---|---|---|---|
| `limit` | int | 返回条数上限，∈[1,1000]；缺省 = 全量 | 400 |
| `offset` | int | 跳过的条数，≥0；缺省 0；超界 → 空数组（200） | 400 |

响应头 `X-Total-Count` 恒写（分页前全量条数）。缺省（无参数）= 与旧行为逐字节一致。

### 节点 DTO 字段（C，L16）

- `GET /api/v1/nodes` / `GET /api/v1/nodes/{nodeID}`：`NodeInfo` 新增可选字段 `offlineAt`（最近标记离线时刻，毫秒时间戳；在线/未知 = 省略）。
- `GET /api/v1/edgenodes` / `GET /api/v1/edgenodes/{nodeID}`：`status.lastOfflineTime`（RFC3339；在线 = 省略）。
- 瞬态内存数据不落盘；重启后离线/Unknown 节点显示为重启时刻（L16 重启重置语义如实外露）。
- 三模式（纯内存/embed/外部）统一。

### DeleteModel GC-on 级联（B）

`DELETE /api/v1/models/{modelName}` 在 `EDGEFLOW_CLOUDCORE_RELEASE_GC_ENABLED=1` 时级联清理该模型全部终态发布（头键 + 逐节点/lock + 内存缓存）；默认关闭 = L31 审计口径不变。端点语义不变（200/404/409 前置守卫不变）。


### 13.4 v0.15.0 增量：Subscription 订阅推送与 Browse 浏览发现

服务 TypeId 均 OPC Foundation UA-Nodeset v1.04 官方 NodeIds.csv 核验。

| 类别 | 符号 |
|------|------|
| 订阅消息 | `CreateSubscriptionRequest/Response`(787/790)、`CreateMonitoredItemsRequest/Response`(751/754)、`PublishRequest/Response`(826/829)、`DeleteSubscriptionsRequest/Response`(847/850)、`SubscriptionAcknowledgement`、`NotificationMessage`、`MonitoredItemNotification`、`NotificationData`（DataChange=811 / StatusChange=820 / EventList=916 占位跳过） |
| 订阅 API | `(*Client).Subscribe(nodes, publishingIntervalMs) (<-chan PublishResult, error)`、`(*Client).PubAck()`、`(*Client).DeleteSubscription()`；`PublishResult{KeepAlive/DataChange/StatusChange}` |
| Browse 消息 | `BrowseRequest/Response`(527/530)、`ViewDescription`、`BrowseDescription`、`ReferenceDescription`、`ExpandedNodeId`（最小形式）、`BrowseResult` |
| Browse API | `(*Client).Browse(node) ([]BrowsedNode, error)` |
| 泵模式 | Client 首次 Subscribe 启动唯一读 goroutine：帧按 RequestId 三级路由（waiter 表→pending 兜底→在途 Publish）；未启用时行为与 v0.14.0 一致 |
| 试解校验 | 全部 `Decode*Request` 导出解码器强制字节全消费（trailing bytes → ErrInvalidEncoding）——分派链防误吞 |

**opcuasim 扩展**：订阅引擎（步进评估变化推送/KeepAlive 空通知/信封队列 ≤32 store-and-forward/悬挂 Publish 回填 RequestId/DeleteSubscriptions 清理）+ 两级 Browse 目录（Objects i=85 → opcua-sim ns=2;i=5000 → 6 变量）。

**mappers/opcua 扩展**：`EDGEFLOW_OPCUA_SUBSCRIPTION=on` 订阅采集模式（supervisor goroutine 消费推送→缓存快照→Collect 短路；HandleCommand 回读后刷新缓存；gap/断线重建订阅；缺省 off 轮询逐字节不变）+ `hack/opcua-browse` 点位发现 CLI。
