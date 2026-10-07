# EdgeFlow 推理运行时对接指南（v0.45.0，spec 0018 US-6 / G24）

面向：需要在 EdgeFlow 边缘节点接入推理服务 / 加速卡的集成者与运维者。
本文是对接规范与集成指南（文档交付物），不含运行时代码（推理面沿用 v0.7.0 HTTP 契约）。

## 1. 推理形态与契约（沿用裁决）

- **HTTP 推理服务**（v0.7.0 起的唯一推理契约，零变化）：
  - 请求：`POST <inferUrl>`，JSON `{"imageBase64":"...", ...}`（JPEG 帧 base64）；
  - 响应：`{"detections":[{"label":"...","score":0.0-1.0,"bbox":[x1,y1,x2,y2]}]}`；
  - 超时/背压由 video mapper 配置（inference.timeoutMs）。
- 视频链路：RTSP/合成帧源 → 解码 → 推理 → 指标/告警/媒资（v0.42–v0.44 已交付）。
- 时序链路：mock/Modbus/REST/OPC-UA/MQTT 采集 → 规则评估 → 告警（v0.37–v0.41）；
  时序模型推理运行时为**外部服务**（本版只约定元数据与发现，不内置运行时）。

## 2. 加速卡能力探测与上报（v0.45.0）

### 2.1 能力清单注入（当前通道）
```bash
# 环境变量（逗号分隔 <类型>:<标识>；非法条目剔除并 Warn）
export EDGEFLOW_EDGECORE_ACCELS="gpu:cuda-12.4,npu:rockchip-9996"
```
- 上报链：edgecore 启动 → Register 消息 `accels` 字段 → 云端 NodeInfo.Accels
  快照 → `GET /api/v1/nodes/{nodeID}` 返回 `accels: [...]`。
- 缺省空 = 不上报（零行为；旧云端忽略未知字段，旧边缘字段省略——双向兼容）。
- 契约只增不改：RegisterPayload.Accels `omitempty`（cloudhub/server.go、
  edgehub/client.go 双侧同步）。

### 2.2 真实设备枚举（可插拔探测器，后续接入）
探测器接口边界（本版登记，未内置实现）：
- GPU：NVIDIA `nvidia-smi --query-gpu=name,driver_version --format=csv`；
  按驱动/CUDA 版本生成 `<类型>:<标识>`。
- NPU：厂商运行时 CLI（如 rockchip `rknn_server`、华为 `npu-smi info`）；
  枚举失败仅降级为空清单（不上报），不阻塞注册。
- FPGA：`lspci | grep -i <vendor>` 类设备存在性探测。
- 约定：探测结果只进 Accels 清单（能力登记），不做运行时绑定/调度（后续）。

### 2.3 验证清单
1. env 注入 → 边缘日志无 `[accel] 忽略非法条目` 警告；
2. 注册后 `GET /api/v1/nodes/{nodeID}` 含 accels；
3. 不设置 env → 节点快照无 accels 字段（零行为）；
4. 混入非法条目（无 `:`）→ 剔除 + Warn，其余条目正常上报。

## 3. 外部推理运行时对接规范

### 3.1 镜像标签发现约定
- 推理服务镜像统一打标 `runtime.http-inference`（镜像 label 或 tag 尾缀）；
- 云端模型仓库按 `Metadata["modality"]` + 镜像标签筛选可部署目标（人工核对；
  自动调度后续）。

### 3.2 端口与环境约定
- 推理服务监听容器内 `:8080`（HTTP，与平台侧管理端口区分）；
- 超时建议：视觉推理 ≤2s（video mapper inference.timeoutMs 对齐）；
- 加速卡挂载（部署侧，PlatformDeployed 形态）：
  - NVIDIA：`nvidia.com/gpu` 资源 Limit（device plugin 标准形态）；
  - NPU：厂商 device plugin / 挂载 `/dev/<npu>*` 设备文件 + 运行时库卷。
- 探活：`GET /healthz` 返回 200（部署影子状态机 Ready 判定依赖）。

### 3.3 模型元数据约定（v0.45.0 键）
| 键 | 语义 | 示例 |
|---|---|---|
| `modality` | 模态（vision 缺省/time-series/multimodal） | `time-series` |
| `input.features` | 时序输入特征名（逗号分隔） | `temperature,vibration` |
| `input.window` | 时序窗口长度（样本数） | `64` |
| `input.rate` | 时序采样率（Hz） | `10` |
| `scene.bindings` | 场景绑定 JSON 数组 | `[{"kind":"device","name":"sensor-01"}]` |
| `train.dataset-ref` | 训练数据集引用（留位） | `ds://sensor-01-2026Q3` |
| `train.job-id` | 训练任务 ID（留位） | `job-123` |

校验：`modality` 白名单强校验（400）；其余键格式宽松（存在即记录，发布平铺下发）。

## 4. 困难样本回传（数据闭环入口）

- 开关：`EDGEFLOW_EDGECORE_HARDSAMPLE=on`（默认 off）；
- 抽样：`EDGEFLOW_EDGECORE_HARDSAMPLE_MAX_PER_ALARM=1`（1–5）；
- 检索：`GET /api/v1/hardsamples?nodeID=&deviceName=&alarmId=&limit=`
  （limit 默认 50、上限 200）；
- 内容：`GET /api/v1/hardsamples/{mediaId}/content`（JPEG；404 未知/409 未完成）；
- 语义：告警新 episode 触发捕获；无视频帧源时跳过（告警链不受阻）；
  断网留存由补传队列兜底（至少一次）。
- 训练平台对接（后续）：按 `train.dataset-ref` 拉取样本集（本版仅检索面）。

## 5. 已知边界（KNOWN-ISSUES §46）

- 加速探测当前为 env 注入 + 探测器骨架（真实枚举=平台适配工作）；
- 发布白名单不校验加速匹配（Accels 仅能力登记）；
- 训练闭环仅元数据键留位；多模态仅元数据白名单。
