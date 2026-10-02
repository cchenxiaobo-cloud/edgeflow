# Spec 0015：视频接入增强——RTSP 原生子集（G20）

- 版本归属：v0.42.0（基线 6332d40 = v0.41.0）
- 规范依据：《EdgeFlow 后续开发计划（对齐智能边缘计算平台解决方案）》v0.42 定义
  （差距 G20 视频接入增强）+ 用户方向「持续迭代开发」
- 裁定结论（本 spec 前置决策）：
  - 全链交付：pkg/rtspclient（自研 RTSP 信令客户端最小子集 + RTP over TCP
    interleaved 解复用 + H.264 FU-A/STAP-A 重组，零依赖）→ RTSPSource（AnnexB
    转发形态，内嵌既有 BridgeSource 管线由外部 ffmpeg 解码出帧——沿用「本版不做
    解码」的既定边界）→ mapper source.type=rtsp 配置扩展 → C6 jpegScanner 段结构
    修复（元数据段假 EOI）→ e2e（本地 RTSP 模拟服务端全链 + 断流自愈 + 桥接零回归）。
  - RTSP 最小子集边界：仅 RTP/AVP/TCP（interleaved）；不做 UDP/重定向跟随/SRTP/
    H.265/音频轨/多轨；SDP 解析最小面（首个 video 轨）；GB28181 SIP 单独立项评估
    （不在本版）。
  - C6 裁定：修复（非仅登记）——jpegScanner 改 marker 遍历解析（APPn/COM 段按
    长度字段整段跳过；SOS 后唯一有效结束为 EOI），消除元数据段内嵌 FFD9 的假切帧；
    对正常流为等价行为（v0340 测试零改动验证）。
  - 默认零行为：source.type=rtsp 未配置时不装配（与 mjpeg/bridge 并存，配置选源）；
    桥接路径（bridge type）零回归。
  - 零第三方依赖（宪法 II）；冻结测试（v0240–v0350）零改动；go.mod 零变化。

## 用户故事与验收

### US-1 RTSP 信令客户端（pkg/rtspclient）
- 文本协议客户端（net.Conn）：请求行（OPTIONS/DESCRIBE/SETUP/PLAY/TEARDOWN）、
  CSeq 自增、头解析（Session/WWW-Authenticate/Server/Transport）、基础认证
  （401 挑战 → Authorization: Basic base64(user:pass)，URL 内嵌凭证提取）、
  超时控制、单连接复用；响应解析 Status-Line + 头（interleaved 模式无独立体）。
- 选项协商：先 OPTIONS（记录 Server/Public 能力），再 DESCRIBE→SETUP→PLAY。
- SDP 最小解析：m=video 行取首个视频轨、a=control 控制路径（相对/绝对拼接）。
- 验收（单测）：请求编解码/认证挑战流程/SDP 控制路径拼接/超时/畸形响应拒绝。

### US-2 RTP over TCP interleaved 解复用与 H.264 重组
- `$`+channel(1)+length(2) 帧定界；RTP 头解析（V/CC/PT/SEQ/TS/SSRC，X 扩展头与
  CSRC 跳过）；video channel 载荷重组：FU-A（S/E 位拼装完整 NAL）、单 NAL 直通、
  STAP-A 分解；输出 AnnexB 字节流（00 00 00 01 + NAL）。
- 解复用与信令共连接（interleaved）：同一 TCP 上 RTSP 响应与 RTP 数据按 `$`
  前缀分派。
- 验收（单测）：帧定界/头解析/FU-A 分片重组等价单 NAL/STAP-A 分解/乱序容错
  （SEQ 跳过仅计数）/畸形包防御（丢弃计数，不断流）。

### US-3 RTSPSource 与 mapper 配置扩展
- RTSPSource：连接生命周期（OPTIONS→DESCRIBE→SETUP→PLAY）、interleaved 读循环、
  NAL→AnnexB 流 → 内嵌 BridgeSource（Decoder command 由配置提供，H.264→MJPEG
  由外部 ffmpeg 完成）→ Frame；断流自愈（重连退避、reconnects 指标）；构造期
  URL 预检（scheme=rtsp）。
- mapper source.type=rtsp：URL/Decoder command 必填校验；工厂接入；与
  mjpeg/bridge 并存（配置选源）。
- 验收（单测 + e2e）：本地 RTSP 模拟服务端（信令应答 + interleaved RTP 下发 +
  合成 H.264 SPS/PPS/IDR/P 序列 FU-A 分片 + 401 认证挑战 + 会话断流控制）→
  RTSPSource 出帧（jpegDim 合法）→ mapper 全链 → 推理 → 告警；断流自愈
  （服务端重置会话 → 源重连恢复出帧）；桥接路径既有测试零改动全绿。

### US-4 C6 jpegScanner 段结构修复
- feed 改 marker 遍历：APPn/COM 按长度字段整段跳过（段内 FFD9 不误判）；SOS 后
  唯一有效结束 = EOI；EOI 切帧；防御语义保持（maxPending 16MB、EOF 未闭合丢弃）。
- 验收（单测）：含 EXIF 缩略图（内嵌 FFD9）样例切帧完整；COM 注释含 FFD9 样例；
  正常样例与 v0340 既有用例零改动等价；畸形样例防御不变。

## 冻结兼容
- 桥接路径（bridge/mjpeg 源）行为零回归（既有测试零改动全绿）；jpegScanner 对
  正常流等价（v0340 测试零改动全绿）；契约零改动（61 端点/15 消息维持）；默认
  零行为（rtsp 未配置不装配）；v0240–v0350 冻结测试零改动；go.mod 零变化。

## 测试锚（as-built 回填）
- pkg/rtspclient/v0420_test.go：9 例（信令全链/401 重试恰一次/SDP 控制路径/interleaved
  →AnnexB 重组/RTP 头与 CSRC 跳过（净载荷断言）/URL/Transport/断流错误上抛）。
- pkg/video/v0420_test.go：3 例（C6 元数据 FFD9/瞬时失败重试语义/URL 预检）。
- mappers/video/v0420_test.go：2 例（rtsp 配置校验/全链 E2E：出帧→推理→断流自愈强断言）。
- 合计 14 例（`grep -c '^func Test'` 口径）；冻结带 v0340 测试零改动全绿。

## 边界登记（随文档落 KI §43）
- 仅 RTP/AVP/TCP（interleaved）——UDP/重定向跟随/SRTP 不做；H.265/音频轨不做；
  SDP 最小面（多轨取首 video）；AnnexB 转发形态不解码（由外部 ffmpeg 出帧）；
  RTSP 模拟服务端为测试面（非产品交付物）；GB28181 SIP 单独立项评估（不在本版）；
  断流自愈依赖源服务端会话可重建。
