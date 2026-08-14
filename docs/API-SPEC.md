# EdgeFlow API 规范（M0-2：CRD 类型定义）

> 对应 ROADMAP 模块 1.4「API 规范定义」（CRD 定义部分，OpenAPI schema 待接入 Kubernetes 后由 controller-gen 生成）。
> 代码位置：`apis/edge/v1alpha1/`
> 状态：✅ 完成（2026-08-13）

## 1. Group / Version 约定

| 项 | 值 | 说明 |
|----|----|------|
| Group | `edgeflow.io` | 统一 API 分组（对标 KubeEdge 的 `devices.kubeedge.io` / `edge.kubeedge.io`） |
| Version | `v1alpha1` | 初版；v1alpha 阶段不保证 API 兼容，后续可能演进到 v1 |
| apiVersion | `edgeflow.io/v1alpha1` | 由 `SchemeGroupVersion.String()` 生成 |
| Kind | `EdgeNode` / `DeviceModel` / `Device` | 三种资源种类 |

- 代码常量：`GroupName = "edgeflow.io"`、`Version = "v1alpha1"`、`SchemeGroupVersion`（见 `apis/edge/v1alpha1/group_version.go`）
- 命名空间：`DeviceModel` 与引用它的 `Device` 须在同一命名空间（当前由文档约束，后续由校验器强制）
- 时间字段统一使用 RFC3339 格式字符串（如 `2026-08-13T12:00:00Z`），零依赖阶段不引入 `metav1.Time`

## 2. EdgeNode（边缘节点）

对标 KubeEdge 的 Node 相关资源（`edge.kubeedge.io`，此处为简化版）。
边缘节点是资源承载者：设备绑定到节点，云端通过 Status 感知节点在线状态。

### 2.1 Spec 字段表

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| nodeID | string | 业务必填 | 节点唯一标识（对标 KubeEdge `spec.nodeID`），注册时生成，用于云边通信鉴权 |
| role | string | 否 | 节点角色：`edge`（默认）/ `cloud` |
| addresses | NodeAddress[] | 否 | 网络地址列表 |
| addresses[].type | string | 是 | 地址类型：`InternalIP` / `Hostname` / `DNS` |
| addresses[].address | string | 是 | 地址值，如 `192.168.1.10` |

### 2.2 Status 字段表

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

### 2.3 默认值（SetDefaults）

- `role` 为空 → `edge`
- `phase` 为空 → `Pending`

## 3. DeviceModel（设备型号）

对标 KubeEdge DeviceModel（`devices.kubeedge.io/v1alpha2`）。
描述一类设备的"模板"：协议家族 + 属性定义，不含具体设备实例。

### 3.1 Spec 字段表

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

### 3.2 默认值（SetDefaults）

- 属性 `accessMode` 为空 → `ReadWrite`（默认允许云端下发期望值）

## 4. Device（设备实例）

对标 KubeEdge Device（`devices.kubeedge.io/v1alpha2`），数字孪生（Twin）机制的核心：
云端下发期望值（desired），设备上报实际值（reported）。

### 4.1 Spec 字段表

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

### 4.2 Status 字段表

| 字段 | 类型 | 说明 |
|------|------|------|
| twins | TwinProperty[] | 数字孪生属性列表（desired 与 reported 对照） |
| twins[].propertyName | string | 属性名 |
| twins[].desired | PropertyValue | 期望值 |
| twins[].reported | PropertyValue | 设备实际上报值 |
| lastUpdatedTime | string (RFC3339) | 最近一次状态更新 |

### 4.3 默认值

- 必填字段（`deviceModelRef` / `nodeName`）**故意不提供默认值**，防止"看起来能跑、实际绑错对象"的隐性错误（有对应测试约束）。

## 5. 与 KubeEdge 对标说明

| EdgeFlow | KubeEdge 对应资源 | 差异与简化 |
|----------|-------------------|------------|
| EdgeNode | Node（`edge.kubeedge.io`） | 仅保留 nodeID / role / addresses / status 核心字段；KubeEdge 的 certID 等字段暂不定义 |
| DeviceModel | DeviceModel（`devices.kubeedge.io/v1alpha2`） | 属性定义基本一致；顶层 protocol 字段对标 KubeEdge v1alpha1 的 protocolType 思路（见下） |
| Device | Device（`devices.kubeedge.io/v1alpha2`） | desired / reported / twins 机制一致；**省略 propertyVisitors**（寄存器/地址映射，Mapper 阶段再补）；protocolConfig 用扁平键值对代替 KubeEdge 的结构化配置 |
| 数字孪生 | KubeEdge Twin 机制 | 语义一致：desired 云端下发、reported 设备上报 |
| ObjectMeta | metav1.ObjectMeta | 最小子集（名称/命名空间/标签/注解/UID/版本/创建时间），零依赖实现 |

### 5.1 设计决策说明

- **DeviceModel 顶层保留 protocol**：KubeEdge v1alpha2 将协议信息放在 Device 上；EdgeFlow 在型号上声明协议家族（便于按协议类型筛选设备、驱动 Mapper 选型），连接参数仍在 Device 上配置。
- **时间字段用字符串**：零依赖阶段避免引入 `metav1.Time`；接入 Kubernetes 后替换。
- **必填字段不设默认值**：Device 的 `deviceModelRef` / `nodeName` 缺失时保持为空，便于校验器报错。

## 6. 推断字段与已知缺口

### 6.1 推断 / 自定义字段清单（KubeEdge 无直接对应）

| 字段 | 说明 |
|------|------|
| EdgeNode.Spec.Role | KubeEdge 无此字段（其 cloud/edge 通过组件区分），EdgeFlow 用 role 显式声明 |
| EdgeNode.Spec.Addresses | 借鉴 Kubernetes `corev1.Node.Status.Addresses`，用于云边通信寻址 |
| EdgeNode.Status.Phase | 借鉴 `corev1.Node` 的 phase 思路，简化节点生命周期表达 |
| EdgeNode.Status.Conditions | 借鉴 `corev1.NodeCondition` 最小集 |
| EdgeNode.Status.LastSeenTime | 云端观测时间（心跳超时判定辅助） |
| DeviceModel.Spec.Protocol | 见 §5.1 设计决策 |
| Device.Spec.Protocol.Config | 扁平键值对（KubeEdge 用结构化 protocolConfig，如 serial/mqtt 对象） |

### 6.2 后续接入 Kubernetes 需要做的事

1. **引入 k8s.io/apimachinery**，为三个资源实现 `runtime.Object` 接口（`DeepCopyObject()`），并将手写 DeepCopy 替换为 controller-gen 生成版本
2. **添加 kubebuilder marker**（`// +kubebuilder:object:root=true`、`// +kubebuilder:subresource:status`、字段校验 marker），生成 CRD YAML 与 OpenAPI schema（ROADMAP 1.4 完成标准：CRD 可 `kubectl apply`）
3. **ObjectMeta 替换**为 `metav1.ObjectMeta`（UID / ResourceVersion 等由 apiserver 维护）
4. **校验落地**：必填字段、属性名须存在于型号、协议名一致性等，通过 CRD schema validation 或 admission webhook 强制
5. **默认值迁移**：SetDefaults 逻辑迁移为 CRD schema `default` 或 mutating webhook

## 7. 示例 YAML（示意，尚未在真实集群验证）

### 7.1 EdgeNode

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

### 7.2 DeviceModel

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

### 7.3 Device

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

## 配置下发 API（WBS 6.2，M2 完整化）

### POST /api/v1/nodes/{nodeID}/config-sync
向指定边缘节点下发 ConfigMap/Secret 配置（可靠投递：边缘确认后返回）。

请求体：
```json
{"operation":"add","config":{"name":"app-config","namespace":"default","kind":"ConfigMap","data":{"key1":"value1"}}}
```
- operation：add/update/delete
- kind：ConfigMap/Secret（delete 时可不填）
- data：map[string]string（add/update 必填；Secret 的 value 当前为明文存储，生产环境需加密，见 PROGRESS.md 待办）

响应：200 边缘已确认（Ack ok）；400 参数非法；404 节点离线/未注册；502 边缘拒绝（Ack error）；504 确认超时重试耗尽；500 内部错误。

边缘存储：MetaManager SQLite，key=`configs/<namespace>/<name>`（与 Pod 元数据同库）。
