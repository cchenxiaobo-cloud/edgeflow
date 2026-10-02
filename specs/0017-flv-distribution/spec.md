# Spec 0017：流媒体分发子集 + 告警片段服务（G22）

- 版本归属：v0.44.0（基线 33d5316 = v0.43.0）
- 规范依据：《EdgeFlow 后续开发计划（对齐智能边缘计算平台解决方案）》v0.44 定义
  （差距 G22：HTTP-FLV/WS-FLV 分发最小面 + 告警片段服务 + HLS 评估登记）。
- 裁定结论（本 spec 前置决策）：
  - **「H.264 透传」输入裁定**：v0.43 媒资为 MJPEG（FLV 不支持该 codec）——FLV 面
    输入为 H.264 AnnexB（无转码，纯封装）；帧源可插拔（FrameProvider）：本版交付
    合成 H.264 流形态（复用 v0.42 RTSP 模拟器的 SPS/PPS/IDR 生成形态）用于验收
    演示；生产帧源（边缘推流/云端 RTSP 拉流）登记 KI §45 后续。
  - FLV 封装器零依赖字节级实现（pkg/flvremux）：header/tag/AVC sequence header
    （AVCDecoderConfigurationRecord）/AVCC NALU（AnnexB→AVCC）/onMetaData 最小面；
    CompositionTime 最小实现 0。
  - 分发端点 = 云端流面（HTTP-FLV chunked + WS-FLV 二进制透传），非 JSON API——
    以「流端点」登记进契约（含状态码与认证面）。
  - 告警片段服务 = 云端联合检索（零边缘改动）：alarm(DeviceName+RaisedAt) ×
    videostream 片段索引时间窗 → 片段列表；回放复用 v0.43 端点。
  - HLS 评估落档：需 TS 封装/切片器/m3u8——工程量与 FLV 正交，登记 KI §45 不入版。
  - 默认零行为：无注册帧源的流 → 分发端点 404；零新依赖；冻结测试零改动；go.mod 零变化。

## 用户故事与验收

### US-1 FLV 透传封装器（pkg/flvremux，零依赖）
- FLV header（"FLV"+version+flags+9）+ PreviousTagSize0；Video Tag（type 9：
  FrameType|CodecID=AVC → packetType 0=sequence header/1=NALU；CompositionTime 3 字节）；
  Script Tag（type 18 onMetaData，AMF0 最小字段）。
- AnnexB→AVCC：起始码扫描（00 00 01 / 00 00 00 01）→ 4 字节大端长度前缀 NALU。
- AVCDecoderConfigurationRecord：configurationVersion/AVCProfileIndication/兼容字节/
  Level（取自 SPS 前字节）/lengthSizeMinusOne=3/SPS+PPS 打包。
- 时戳：毫秒递增（由 Remuxer 输入携带）；同 PTS 帧序保序输出。
- 验收（单测）：header/tag 字节断言、AnnexB→AVCC（含起始码两形态/多 NALU）、
  seq header 结构、onMetaData 可解析形态、畸形输入防御（无 SPS/PPS 拒绝）。

### US-2 分发端点（云端流面）
- 帧源注册表：stream name → FrameProvider（并发安全注册/注销；演示源由装配/测试
  注入）。
- `GET /media/streams/{name}/live.flv`：HTTP chunked（Content-Type video/x-flv）；
  首 flush = FLV header + onMetaData + sequence header；后续按帧输出 NALU tag。
- `GET /media/streams/{name}/live.ws`：WS 升级后二进制帧透传 FLV 字节流（无子协议）。
- 无注册源 → 404；客户端断开 → 取消拉流（ctx）；源注销 → 流终止。
- 验收（单测）：注册→拉流→解析回 FLV 结构（header/tag 往返）→ 断开；404；
  并发拉流（≥2 reader）。

### US-3 告警片段服务（云端联合检索）
- `GET /api/v1/alarms/{alarmID}/segments`：alarm → (DeviceName, RaisedAt) →
  videostream 检索该流 RaisedAt 前后窗片段（默认 [RaisedAt-5s, RaisedAt+60s]）
  → SegmentRef 列表（含回放 URL 路径）。
- 告警不存在 404；无关联流/无窗内片段 → 空列表（200）。
- 验收（单测）：窗口命中/越窗排除/多片段时序/告警缺失 404/无流空列表。

### US-4 e2e 全链（tests/e2e）
- FLV 拉流演示：真实 cloudcore + 注册合成 H.264 帧源 → HTTP-FLV 拉流 → 解析
  FLV 结构断言（header/seq header/onMetaData/≥N video tags/时戳递增）+ WS-FLV 拉流。
- 告警→片段→播放全链：创建告警（v0.40 管道或直接注入 alarmstore 语义）→
  注入窗内媒资（v0.43 管道）→ GET /alarms/{id}/segments 命中 → 片段回放字节校验。
- 验收：两链全绿（对齐 dev-plan「FLV 拉流播放演示」「告警→片段→播放全链」）。

### US-5 契约与文档
- 契约扩容：+2 JSON 端点（/media/streams/{name}/live.flv、live.ws 以流端点登记）
  +1 告警片段端点（69→72）；消息零新增（16 维持）。
- HLS 评估登记 KI §45；VIDEO-GUIDE/ROADMAP/KI/README/Chart 同步。

## 兼容与冻结
- 既有 69 端点行为零变化（只增）；冻结带测试零改动；go.mod 零变化；零新依赖。
- 默认零行为：无注册帧源 → 404；告警窗无片段 → 空列表。

## 测试锚（as-built 回填）
- pkg/flvremux/flvremux_test.go：4 例（header/seq header 字节断言+onMetaData/
  AnnexB→AVCC 往返+帧型位/起始码两形态切分/空流拒绝）。
- cmd/cloudcore/media_stream_api_test.go：5 例（无源 404/live.flv 拉流解析与
  时戳/live.ws 拉流/写超时豁免防回归/告警片段检索窗口语义与 404/空列表）。
- tests/e2e/v0440_flv_distribution_e2e_test.go：2 例（TestV0440FLVDistributionE2E
  拉流演示/TestV0440AlarmSegmentsE2E 告警→片段→播放运行时链路）。
- 合计 11 例（单测 9 + e2e 2；grep -c '^func Test' 口径）。

## 边界登记（随文档落 KI §45）
- FLV 仅 H.264 视频透传（音频 AAC 不做；无转码）；推流协议面（RTMP 推流/边缘转发）
  不在本版——生产帧源登记后续；WS-FLV 无子协议协商；HLS（TS/切片/m3u8）评估登记；
- 直播时移/拖动不做；CompositionTime 最小实现 0（B 帧重排不做）；
- 告警检索窗默认 [RaisedAt-5s, +60s]（可配后续）；多告警合并片段去重不做。
