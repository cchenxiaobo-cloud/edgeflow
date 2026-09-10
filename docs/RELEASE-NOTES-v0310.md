# EdgeFlow v0.31.0 Release Notes

**日期**：2026-09-10 ｜ **基线**：b356e8f（v0.30.0） ｜ **主题**：视频流管理与边缘推理对接（阶段一，FR-S1-07 分段一）

## 亮点

边缘侧新增视频流管理能力：帧源 → 背压槽 → HTTP 推理服务 → 指标/留痕/事件的最小可用链路。阶段一以内置合成源闭环全链路（确定性可测、零外部依赖），RTSP 实源、云端管理面与 GPU 运行时登记阶段二（KNOWN-ISSUES §32）。

## 新增

- **pkg/video（新包，零第三方依赖）**
  - `Frame`（Seq/TsMs/宽高/JPEG）+ `FrameSource` 帧源抽象；`SyntheticFrameSource` 确定性合成源（灰度渐变背景 + 对角折返热区，同参数同序号逐字节一致，JPEG 编码 image.Decode 可往返）。
  - `Inferencer` 接口 + `HTTPInferencer`：HTTP JSON 推理契约（POST 帧 JPEG base64 + 元数据 → 检测框数组）；超时/非 200/坏 JSON 显式报错；服务端可省略元数据（帧序/时间戳以帧为准回填）。
  - `LatestSlot`：latest-wins 背压帧槽——推理慢于出帧时丢旧帧保最新，丢弃计数实时暴露（不排队堆积、无goroutine 泄漏面）。
- **mappers/video（新 Mapper）**
  - `VideoMapper` 完整实现 DeviceMapper（含 DeviceNameResolver/DeviceNamespaceResolver）；JSON 配置文件（deviceName/source/inference/ledger/eventbus）+ `LoadConfig` 校验（source.type 仅 synthetic，未知值显式拒绝）。
  - 采集推理双循环：produce（出帧入槽）+ infer（取最新帧推理）；`stream` 指令运行中启停（value 1/0），未知属性拒绝；Start/Stop 幂等。
  - 数字指标面经 `Collect()` 汇入既有影子上报链（framesTotal/inferTotal/inferFailTotal/framesDropped/detectionsLast/avgScoreLast/frameSeqLast/fps EMA/streamOn）——Mapper 不感知上报链路，框架约定不变。
  - 推理结果留痕（metamanager 台账：Direction=up、RegAddr=frame:帧序、Result ok/error、Message 截断 JSON 512B）+ 事件上行 opt-in（`edgeflow/video/{device}/inference`，*eventbus.EventBus 经 EventPublisher 接口零适配注入，未注入零副作用）。
- **edgecore 装配**：`EDGEFLOW_VIDEO_MAPPER_CONFIG` 指向配置文件即注册 video mapper；无环境变量零行为（冻结兼容）；文件缺失/非法仅 Warn 跳过，不影响其余 Mapper。

## 质量

- v0310 新测试 12 例全绿：pkg/video 7（合成源序列/确定性/JPEG 往返含热区像素/HTTPInferencer 成功与错误面/背压槽丢帧/JSON 截断）+ mappers/video 5（指标与事件与台账/丢帧与失败计数/stream 指令/配置校验/装配级 e2e）。
- 新包 `-race` 绿；全仓 `go test ./...` 绿；契约 42 端点不变；冻结带（v0240–v0300 测试）零改动；零新依赖（新 import 仅 stdlib + 仓内包）。

## 边界与登记（KNOWN-ISSUES §32）

- RTSP/GB28181 实源拉流（零依赖冲突，需单独裁定）、云端 VideoStream 管理面（契约扩容）、GPU 推理运行时集成 → 阶段二非承诺。
- 台账方向语义映射（推理结果复用 DirUp）、合成源 CPU 渲染性能边界（背压自然限流）见 §32。

## 升级与兼容

- 默认行为零变化：不设置 `EDGEFLOW_VIDEO_MAPPER_CONFIG` 时 edgecore 与 v0.30.0 行为一致；契约 API 零改动。
