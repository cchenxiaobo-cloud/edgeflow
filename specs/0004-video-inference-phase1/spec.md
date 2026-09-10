# Spec 0004：视频流管理与边缘推理对接（阶段一）

- 版本归属：v0.31.0（基线 b356e8f = v0.30.0）
- 规范依据：用户方向「视频流管理模块，对接边缘视频推理需求」；DEVELOPMENT-SPEC 新增 FR-S1-07（S1 数据采集延伸）；沿用 mapper 框架（edge/pkg/mapper）与配置文件化先例（FR-S1-06）
- 分段声明：本 spec 为**阶段一**——帧源抽象 + 合成源 + HTTP 推理服务对接 + video mapper + 留痕/事件上行 + edgecore 装配。RTSP/GB28181 实源拉流（需第三方库或外部进程桥，与零依赖硬约束冲突，需单独裁定）、云端 VideoStream 管理面（CRUD/快照/回放，契约扩容）、GPU 推理运行时集成为**阶段二及后续**，不承诺版本。

## 用户故事与验收

### US-1 帧源抽象与合成源（pkg/video）
- `Frame{Seq, TsMs, Width, Height, JPEG}`；`FrameSource` 接口 `Next(ctx)`；`SyntheticFrameSource`（Option：fps、宽高、移动热区）确定性出帧、JPEG 编码（stdlib image/jpeg），`image.Decode` 可往返验证。
- 阶段一边界登记：RTSP 实源不实现；`source.type` 仅接受 `synthetic`，其余显式报错（为阶段二留扩展点，不接受未知值静默降级）。
- 验收：单测——帧序列 seq/ts 单调、尺寸一致、JPEG 往返解码、合成图案热区随 seq 移动。

### US-2 推理服务对接（pkg/video）
- `Inferencer` 接口 + `HTTPInferencer`：POST JSON（deviceName/frameSeq/tsMs/width/height/imageBase64）→ 响应 JSON（detections[{label,score,bbox[x,y,w,h]}]）；超时可配；非 200/坏 JSON 显式报错；帧图 JPEG base64。
- 验收：单测（httptest stub）——成功往返字段保真、超时报错、5xx 报错、坏 JSON 报错。

### US-3 背压与 video mapper（mappers/video）
- latest-wins 帧槽：推理慢于出帧时丢旧帧保最新，丢帧计数暴露（`framesDropped`）；`InferenceResult` 数字指标面（framesTotal/inferTotal/inferFailTotal/detectionsLast/avgScoreLast/frameSeqLast/fpsEma）。
- `videoMapper` 实现 DeviceMapper：配置文件（JSON，字段 deviceName/namespace/source/inference/ledger/eventbus）；Start/Stop 幂等；HandleCommand `property=stream`（value 1/0 启停）其余拒绝；Collect 返回指标面（复用影子上报链，Mapper 不感知上报链路）。
- 验收：单测——stub source+inferencer 下指标累计正确、丢帧统计、启停幂等、未知 property 拒绝。

### US-4 结果留痕与事件上行（阶段一限定）
- 详细推理结果 JSON 留痕：`metamanager.Ledger.SaveOp`（Direction="inference"，RegAddr=帧 seq，Message=截断 JSON），ledger 未注入则跳过（warn 一次）。
- 事件上行 opt-in：`eventbus.EventBus` 注入时 Publish `edgeflow/video/{device}/inference`（QoS 由总线默认），未注入不发。
- 验收：单测——ledger 留痕记录字段、eventbus 注入 stub 收到 payload、未注入零副作用。

### US-5 edgecore 装配与 e2e
- 环境变量 `EDGEFLOW_VIDEO_MAPPER_CONFIG` 指向配置文件时装配 video mapper（无配置零行为，兼容冻结：不注册、不启动、无告警噪音——沿 mock_sensor 门控先例但为 opt-in 文件触发）。
- e2e：配置 → edgecore 装配 → stub 推理服务 → 断言影子 Reported 指标与 ledger 留痕。
- 验收：e2e 1 例全链路；默认（无环境变量）路径回归零影响。

## 冻结与兼容
- 契约 42 端点不动（阶段一云侧零改动）；v0240–v0300 冻结测试零改动；新增文件物理隔离（pkg/video、mappers/video、装配段）。
- MapperRegistry/DeviceMapper 接口零改动（video 实现既有接口，含可选 DeviceNameResolver）。

## 实现偏差登记（as-built）
- **台账方向白名单适配**：metamanager.SaveOp 仅接受 Direction up/down，原设计 "inference" 非法；实现改用 DirUp（推理结果属上报语义），RegAddr=frame:帧序，Result 对齐 ok/error 约定。
- **frameSeqLast 兑底**：recordResult 在结果 seq 为 0（服务端省略/直接构造结果）时以帧序兑底；HTTPInferencer 正常路径已回填。
- **framesDropped 实时读槽**：指标面的丢弃计数实时读自 LatestSlot（唯一真相源），不在 metrics 结构内冗余副本。
- **eventbus 注入面**：mapper 依赖 EventPublisher 小接口（Publish 同签名），*eventbus.EventBus 天然实现，装配层零适配；未注入零副作用。
- **SourceConfig.Synth 字段名**：配置文件 source.synthetic 嵌套合成源参数；source.type 仅 synthetic，未知值显式拒绝（US-1 边界）。
- **复核 P1-1 修复（Stop 重启序列死锁）**：初版 stopOnce（一次性）与 Start 重建 stopCh 组合，stream 0→1→0→1 序列下第二次 Stop 永久阻塞（wg.Wait 不返回，HandleCommand 同步内联会卡死设备指令面）。修复 = 生命周期重构：每次 run 独立局部 WaitGroup + runDone 通道（Stop 关 stopCh 后等 runDone；running 状态保证同一 stopCh 只 close 一次），重启序列无共享残留；新增 TestV0310MapperRestartSequence 回归锚（3 轮启停 + 卡死超时守护 + 循环真停断言）。
- **复核 P1-2 修复（槽丢弃计数失真）**：初版 Take 不清槽，Put 覆盖已消费帧也计 dropped（常态 framesDropped≈framesTotal-1，与 US-3 背压语义矛盾）。修复 = Take 成功后锁内清槽；仅覆盖未消费帧计丢弃；新增 TestV0310SlotConsumeThenOverwrite 语义锚。
- **复核 P2 顺手项**：200 响应体加 4MB LimitReader + 删除非 200 分支死存储；recordResult 增加 res==nil 防御（计 inferFail）。未修登记项：出帧错误后 streamOn 降级语义（produceLoop 静默退出而 streamOn 仍=1）归阶段二实源接入（§32）。
