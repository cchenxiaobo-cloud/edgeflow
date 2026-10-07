# EdgeFlow v0.45.0 规格（spec 0018）——模型面扩展 + 困难样本回传 + 推理运行时

版本：v0.45.0（发展规划 G23 + G25 + G24）。状态：as-built 回填中。
基线：v0.44.0（cd396d3）；契约基线 72 端点 / 16 消息。

## 1. 背景与目标

v0.7.0 建成模型仓库（模型/版本/发布/部署影子 17 端点，镜像即模型），v0.16–v0.23 建成
灰度发布与预算控制。本版把模型面从「视觉镜像托管」扩展到「时序/多模态元数据 + 业务场景
关联」，补齐 AI 数据闭环的「困难样本回传」缺口，并落「加速卡探测 + 推理运行时对接规范」
（G24）。全部设计遵循：零第三方依赖、契约只增不改、默认零行为、复用既有通道。

## 2. 用户故事

### US-1 时序/多模态模型元数据（G23）
作为平台运营者，我在创建/更新模型与版本时声明 Modality（vision|time-series|multimodal，
开放字符串白名单校验）与时序约定键（input.features / input.window / input.rate，
Metadata 平铺），发布时随既有 metadata 平铺进 config-sync 下发，边缘消费方按需解析。
- 校验：modality ∈ 白名单或空（缺省 vision 兼容存量）；约定键值格式宽松（键存在即记录，
  值类型 string）。
- 兼容：存量模型零改动（Metadata 增量键，契约"只增不改"）。

### US-2 模型-业务场景动态关联（G23）
作为运营者，我把模型关联到业务场景对象（摄像头/设备）：Metadata 约定键
scene.bindings = JSON 数组（[{kind:"camera"|"device", name:"..."}]），发布平铺下发；
边缘 mappers/video 消费 camera 绑定（v0.45 演示：告警联动快照已具备，绑定键为后续
自动化消费留位）。云端提供 GET /api/v1/models/{modelName}/scenes 查询（解析 bindings
键返回结构化列表；未配置返回空数组）。

### US-3 困难样本收集与回传（G25，边缘 opt-in）
作为运维者，我开启 EDGEFLOW_EDGECORE_HARDSAMPLE=on 后：v0.40 告警触发时，边缘按
策略捕获关联设备快照帧（JPEG）作为困难样本（kind=hard-sample），低带宽抽样（每告警
最多 1 张，EDGEFLOW_EDGECORE_HARDSAMPLE_MAX_PER_ALARM 可调 1–5），本地 spool 缓存
（复用 mediaup 断网留存），经 v0.39 补传队列恢复后补传（零新队列，至少一次语义）。
关闭时零行为（默认 off，与波形通道同款 opt-in）。
- 帧来源：复用既有采样管道快照能力（JPEG 单帧）；无帧源时登记跳过（不阻塞告警链）。

### US-4 困难样本云端接收与检索（G25）
云端 mediastore 接受 kind=hard-sample（Validate 白名单扩容），对象落
objects/hardsample/ 前缀（与 snapshot/segment 隔离）；不挂接 videostream 索引
（非流媒资）。新增检索端点：
- GET /api/v1/hardsamples?nodeID=&deviceName=&alarmId=&limit=（默认 limit=50，
  ≤200）→ {items:[{mediaId,nodeId,deviceName,alarmId,capturedAt,bytes,sha256}],count}；
- GET /api/v1/hardsamples/{mediaId}/content → JPEG 字节流（200；404 未知）。
契约 72→74。

### US-5 加速卡探测与上报（G24，边缘 opt-in）
作为运维者，我在边缘注入 EDGEFLOW_EDGECORE_ACCELS=env 清单（逗号分隔，
如 "gpu:cuda-12.4,npu:rockchip-9996"，模拟验证通道；真实探测=读类设备文件/proc
接口——实现为可插拔探测器，env 优先、真实探测次之、缺省空）；注册时经
RegisterPayload.Accels（[]string omitempty，先例 Compression）上报；云端 NodeInfo
快照与 GET /api/v1/nodes/{nodeID} 返回扩展 accels 字段（旧边缘无此字段 → null，
兼容）。发布白名单校验复用 archs 语义（加速需求匹配为后续迭代，本期仅登记能力）。

### US-6 推理运行时对接规范与指南（G24，文档交付）
docs/INFERENCE-GUIDE.md：加速卡探测说明（env/真实探测/上报链路）、外部推理运行时
对接规范（HTTP 推理契约沿用 v0.7 裁决；运行时发现约定：容器镜像标签
runtime.http-inference）、加速卡集成指南（设备插件形态/NPU 厂商 SDK 边界/验证清单）。
训练闭环接口留位：模型版本 Metadata 约定键 train.dataset-ref / train.job-id
（仅约定，不做训练服务）。

## 3. 非功能与兼容

- 契约：72→74 端点（hardsamples 2）；消息 16 维持（复用 MediaUpload/Register）。
- 默认零行为：HARDSAMPLE 默认 off；Accels 缺省空（不上报不下发）；modality 缺省
  vision 兼容存量；scenes 端点未配置返回空数组。
- 冻结带（v0240–v0350）零改动；v0.39–v0.44 测试零改动（行为演进走新测试文件）。
- go.mod 零变化；零新依赖。

## 4. 边界（KNOWN-ISSUES §46 登记）

- 困难样本仅 JPEG 快照帧 + 告警上下文元数据（无原始波形/视频段——量大，后续按需）；
- 抽样策略每告警计数在内存（重启清零，最多多传一张，无害）；
- 加速探测本期 env 注入 + 可插拔探测器骨架（真实 GPU/NPU 枚举=平台适配工作，指南给清单）；
- 训练闭环仅元数据键留位；多模态仅元数据白名单（不做托管/推理）；
- 发布白名单不校验加速匹配（仅登记能力，匹配校验后续）；
- scenes 查询是 bindings 键的结构化视图（非独立资源，无 CRUD）。

## 5. 测试锚（as-built 回填）

- cloud/pkg/modelrepo/v0450_metadata_test.go：1 例（modality 白名单表驱动：
  无键兼容/空值/三合法值/非法值/大小写）。
- cloud/pkg/mediastore/v0450_test.go：2 例（hard-sample 落盘+前缀隔离+过滤
  列表+AlarmID 元数据+快照隔离；未知 kind 仍拒绝）。
- cmd/edgecore/v0450_test.go：6 例（accel env 解析/采集器开关与 max 回退/
  每告警 admit/无帧安全/空快照防御/多联动顺序与 nil 安全）。
- cmd/cloudcore（既有守卫同步）：TestModelAPIRouteCount 28→29 端点
  （模型族 7→8）。
- tests/e2e/v0450_model_sample_e2e_test.go：1 例
  （TestV0450ModelSampleInferenceE2E：US-5 加速上报 + US-2 场景关联 +
  US-4 检索空态 + US-3 全链铁证——告警→捕获→入队→补传→检索→sha256 一致）。
- 合计 10 例（单测 9 + e2e 1；grep -c '^func Test' 口径）。
