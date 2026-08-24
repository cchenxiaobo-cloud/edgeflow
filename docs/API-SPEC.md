# EdgeFlow API 规范（v0.1.0 定稿）

> - 对应 ROADMAP WBS 9.2「API 文档」，覆盖两部分：**REST API 参考**（cloudcore 对外 HTTP 接口）与 **CRD 类型定义**（`apis/edge/v1alpha1/`）。
> - 状态：✅ **v0.1.0 定稿**（2026-08-14），**v0.2.0 开发轮已更新**（2026-08-18：podsync 资源字段与 409 语义、device-command namespace 路由、资源调度环境变量），**v0.3.0 开发轮已更新**（2026-08-19：syncPod 400 响应 JSON 安全加固说明 + 第四部分共享库/协议包 API 边界），**v0.4.0 开发轮已更新**（2026-08-24：§1 并发语义、§8 已知限制首条改为分级持久化、nodeID 字符约束登记）。评审记录见 `docs/REVIEWS.md`（9.2 评审归档）。
> - 代码位置：cloudcore 路由装配 `cmd/cloudcore/main.go`、设备 API `cmd/cloudcore/device_api.go`、CRD 类型 `apis/edge/v1alpha1/`。
> - 版本策略：v0.1.0 为 MVP 定稿版；后续接入 Kubernetes 后由 OpenAPI schema / CRD 校验取代，见 §7。

---

# 第一部分 REST API 参考（cloudcore）

## 1. 通用约定

| 项 | 约定 |
|----|------|
| Base URL | `http://<cloudcore-ip>:8080`（端口可用 `--port` / `EDGEFLOW_CLOUDCORE_PORT` 覆盖） |
| 数据格式 | JSON；请求 `Content-Type: application/json`；响应同样为 JSON |
| 时间戳 | Unix 毫秒（心跳/上报/注册时间）；CRD 对象内的时间字段为 RFC3339 字符串 |
| List 风格 | 查询类端点采用 K8s List 风格（`kind`/`apiVersion`/`items`），空数据编码为 `[]` 而非 `null` |
| 路径参数 | `{nodeID}` 为边缘节点 ID（edgecore 注册时上报，默认 `edge-<hostname>`）。**v0.4.0 硬约束**：nodeID 必须匹配 `^[A-Za-z0-9._-]+$`，含 `/` 的 nodeID 写入（注册/设备指令/删除）被拒绝并告警（见 §8） |
| 并发语义 | **v0.4.0 起分级持久化**：云端注册元数据与设备 Desired 跨重启保留（嵌入式 etcd 写穿）；Pod 状态与上报属性（properties）为内存态，重启后短暂清空（≤1 上报周期，边缘重连自愈）；边缘侧 MetaManager（SQLite）持久化 |

### 1.1 端点总览

| 方法 | 路径 | 说明 | 主要状态码 |
|------|------|------|-----------|
| GET | `/healthz` | 健康检查（探针用） | 200 |
| GET | `/metrics` | Prometheus 指标（五指标，WBS 10.1） | 200 |
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

### 1.2 错误码表（统一约定）

| HTTP 状态码 | 语义 | 典型场景 |
|------------|------|---------|
| `200` | 成功；下发类接口表示**边缘已确认**（Ack ok），响应 `{"status":"ok","acked":true}` | 正常 |
| `400` | 请求非法：JSON 解析失败 / 缺必填字段 / operation 或 kind 不在白名单 / 资源格式非法或 request>limit（仅 podsync，文案含具体超标字段，如 `CPU request (500m) 不能超过 CPU limit (250m)`） | 参数错误 |
| `404` | 节点未注册或离线（`ErrNodeOffline`）；单资源查询不存在 | 节点不存在 |
| `409` | 冲突：节点资源超卖，边缘准入拒绝（仅 podsync，WBS 6.5）——响应 `{"error":"EDGEFLOW_RESOURCE_EXHAUSTED: ..."}`，拒绝不落盘 | 已部署 request 求和 + 新请求超出节点容量 × 超卖率 |
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

# 第二部分 CRD 类型定义（apis/edge/v1alpha1）

> 代码位置：`apis/edge/v1alpha1/`（Group `edgeflow.io`，Version `v1alpha1`）。
> 此部分为 M0-2 已定稿内容，随 v0.1.0 一并归档。

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

## 7. 后续接入 Kubernetes 需要做的事

1. **引入 k8s.io/apimachinery**，为三个资源实现 `runtime.Object` 接口（`DeepCopyObject()`），并将手写 DeepCopy 替换为 controller-gen 生成版本
2. **添加 kubebuilder marker**（`// +kubebuilder:object:root=true`、`// +kubebuilder:subresource:status`、字段校验 marker），生成 CRD YAML 与 OpenAPI schema（ROADMAP 1.4 完成标准：CRD 可 `kubectl apply`）
3. **ObjectMeta 替换**为 `metav1.ObjectMeta`（UID / ResourceVersion 等由 apiserver 维护）
4. **校验落地**：必填字段、属性名须存在于型号、协议名一致性等，通过 CRD schema validation 或 admission webhook 强制
5. **默认值迁移**：SetDefaults 逻辑迁移为 CRD schema `default` 或 mutating webhook

## 8. 已知限制（v0.1.0 定稿时确认）

| 限制 | 影响 | 计划 |
|------|------|------|
| 云端状态**分级持久化**（v0.4.0 起） | 注册元数据（节点台账）与设备 Desired **跨重启保留**（嵌入式 etcd 同步写穿，`EDGEFLOW_CLOUDCORE_ETCD_ENABLED=true` 默认启用）；Pod 状态与上报属性（properties/reported）**重启后短暂清空**（≤1 上报周期，边缘重连后自愈，**非永久丢失**）；心跳/Status 不落盘（重启后待首次心跳翻新） | v0.4.0 起已消除"重启清空"整体性限制；对接 K8s apiserver 后进一步统一 |
| nodeID 字符约束（v0.4.0 新增硬约束） | nodeID 必须匹配 `^[A-Za-z0-9._-]+$`；namespace/podName/deviceName 不得含 `/`。含 `/` 的 nodeID 写入（注册、device-command、GC 级联删除）被**拒绝并告警**（防破坏 etcd 键空间前缀扫描）。现有边缘 nodeID 为 UUID/主机名形态，不受影响 | 协议/部署文档登记（API-SPEC §1、DEPLOYMENT §10） |
| `/api/v1/nodes` 与 `/api/v1/edgenodes` 双视图并存 | 两种响应形态，客户端需按端点区分 | 属设计取舍（运行视角 vs CRD 视角），v0.1.0 保留 |
| device-command 的 value 为 float64 | 非数值属性（string/boolean）暂无法通过本端点下发 | Mapper 扩展时评估 |
| config-sync 的 Secret value 明文传输存储 | 生产环境需加密 | PROGRESS.md 待办 |
| 广播/组播下发（Target="*"）路由层已支持，API 层未暴露 | 无批量下发端点 | 后续版本 |

## 9. 归档信息

- 定稿版本：v0.1.0（2026-08-14）
- 评审记录：`docs/REVIEWS.md` §9.2（评审人、已知问题、归档状态）
- 相关文档：`docs/ARCHITECTURE.md`（9.1）、`docs/DEPLOYMENT.md`（9.3）、`docs/HANDOFF.md`（9.4）、`examples/README.md`（9.5 温度传感器 Demo 教程）

---

# 第四部分 共享库与协议包 API 边界（v0.3.0 新增）

> 本节登记 v0.3.0 新增/变更的库级 API 与 `pkg/opcua` 包边界，供 Mapper/上层模块消费方与测试方引用。代码即契约，本节为摘要。

## 10. pkg/log.SetOutput（v0.3.0）

| 签名 | 说明 |
|------|------|
| `func SetOutput(w io.Writer)` | 设置日志输出目标（默认 stderr）。传 `nil` **恢复 stderr**（兼容 stdlib `log.SetOutput` 约定）。供测试捕获输出（如 `bytes.Buffer`），生产代码无需调用 |

- 新增于 commit `714d5ba`；与既有 `SetLevel`/`GetLevel`/`Debugf`（v0.2.0）无交互约束。

## 11. edge/pkg/edgehub Options.BackoffSleepFunc（v0.3.0）

| 字段 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `BackoffSleepFunc` | `func(d time.Duration) bool` | nil=内置默认退避休眠 | 重连退避休眠实现注入点：每次退避被调用一次，返回 false 中止重连；nil 时由客户端内置实现接管（与 v0.2.0 行为逐字节一致）。测试注入计数/加速实现，替代实时时间阈值断言 |

- 新增于 commit `714d5ba`（KNOWN-ISSUES §1 ② 闭环）；`bool` 返回值与客户端 `shuttingDown` 中止语义对齐。

## 12. pkg/opcua 包边界（v0.3.0 M1，UA Binary 协议栈核心）

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
