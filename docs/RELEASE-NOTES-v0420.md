# EdgeFlow v0.42.0 发布说明（视频接入增强）

- 发布日期：2026-10-02
- 基线：v0.41.0（6332d40）
- 主题：视频接入增强（发展规划 G20）——自研 RTSP 客户端最小子集（信令/RTP over TCP interleaved/H.264 重组，零依赖）、RTSPSource 帧源（AnnexB→外部解码→出帧，断流自愈）、mapper rtsp 源型（与 mjpeg/bridge 并存）、C6 jpegScanner 段结构修复（元数据段假 EOI）。零新依赖；契约零改动（61 端点/15 消息维持）；桥接路径零回归；rtsp 未配置时零行为。

## N1 能力（新增）

### 1. RTSP 信令客户端（pkg/rtspclient，零依赖）
- 最小子集：OPTIONS（选项协商，Server/Public 记录）/ DESCRIBE（SDP 最小解析：会话级与轨级 a=control，相对路径按基准拼接）/ SETUP（RTP/AVP/TCP;interleaved）/ PLAY / TEARDOWN。
- CSeq 自增；基础认证（401 挑战 → Authorization: Basic 重试恰一次；URL 内嵌凭证或 SetCredentials 显式）；信令读写超时；单连接复用。
- interleaved 多路分派：信令响应与媒体帧同连接——读循环按流首字节分派（`$`=媒体帧 / `RTSP/`=信令）；DESCRIBE 的 SDP 体按 Content-Length 读取。

### 2. RTP over TCP 解复用与 H.264 重组（pkg/rtspclient/rtp.go）
- `$`+channel(1)+length(2) 帧定界；RTP 头解析（V/CC/PT/SEQ/TS/SSRC，X 扩展头与 CSRC 跳过）；SEQ 异常计数（TCP 已保序，仅诊断）。
- H.264 载荷重组（RFC 6184 最小面）：FU-A 分片（S/E 语义，重建 NAL header 取 indicator 高 3 位 + FU header type）、单 NAL 直通、STAP-A 分解；输出 AnnexB（00 00 00 01 + NAL）。
- 畸形载荷/非法 NAL type → 丢弃计数（不断流）；PT 限 96/97（其余计数丢弃）。

### 3. RTSPSource 帧源（pkg/video/v0420.go）
- 管线：RTSP 客户端拉流 → AnnexB → 解码进程 stdin（H.264→MJPEG 外部命令，如 ffmpeg）→ stdout → jpegScanner → Frame。
- 源健康管理：断流自愈（RTSP 断开/解码进程退出 → 整体重连，Reconnects 计数 + 退避）、配置性错误显式拒绝（url/decoder 缺失）、ctx 取消路径（watcher Kill 解码进程 + 关 stdin/stdout 管道解除 Read 阻塞——照抄 BridgeSource 模式）。
- 与 mjpeg/bridge 并存（SourceConfig.Type=rtsp + Decoder 字段；工厂分发）。

### 4. C6 修复：jpegScanner 段结构（pkg/video/v0340.go）
- 裁定：修复（非仅登记）。feed 改 marker 遍历：APPn/COM/DQT/DHT 等带长段按长度字段整段跳过（段内 FFD9 不再误判帧尾）；SOS 后进入熵编码数据，唯一有效结束 = EOI（熵数据内 0xFF 仅允许 0x00 填充与 RSTn）；部分到达（段长延伸出缓冲）等待下一块；maxPending 16MB 防御保持。
- 兼容性：对不含元数据 FFD9 的正常流与旧「首个 FFD9」逻辑等价（v0340 全部既有测试零改动通过验证）。

## N2 兼容与冻结
- **默认零行为**：source.type=rtsp 未配置时不装配（与 mjpeg/bridge 并存）；桥接路径（bridge/mjpeg 源）行为零回归（v0340 测试零改动全绿）。
- 契约零改动：61 端点 / 活跃 15 消息维持（本版为边缘视频接入面，无新端点/消息）。
- v0240–v0350 冻结测试零改动；MQTT/OPC-UA/规则/时序面代码零触碰；go.mod 零变化（RTSP 为自研实现）。

## N3 测试（grep 口径，`grep -c '^func Test'`）
- pkg/rtspclient +9 例（信令全链/401 挑战两路径/interleaved→AnnexB 重组/H.264 三形态与错误路径/SDP 控制路径/URL 解析/Transport 解析/RTP 头扩展与 CSRC 跳过（净载荷断言）/**断流错误上抛**）。
- pkg/video +3 例（**C6 元数据 FFD9**（APP1+COM 段单帧完整切出/双帧流）/ **瞬时失败重试语义**（拨号失败循环重试至 ctx 取消，不复现「单次失败即终止」）/ **URL 预检**（scheme/host 构造期拒绝））。
- mappers/video +2 例（配置校验 rtsp 必填项 / rtsp 源型全链：模拟服务端→拉流→解码脚本出帧→推理 stub 命中→**断流自愈（reconnects 增长 + 帧流恢复，强断言）**）。
- 合计：新增 14 例；冻结带 pkg/video v0340 13 例、mappers/video v0340 3 例全部**零改动**全绿（C6 等价性与桥接/MJPEG 零回归验证；新语义用例放行至 v0420 测试文件——DEV-SPEC「行为演进=新测试文件」）。

## N4 边界（登记 KNOWN-ISSUES §43）
- 仅 RTP/AVP/TCP（interleaved）——UDP 传输、重定向跟随、SRTP 不做；
- H.265/音频轨不做（音频 channel 数据丢弃计数）；SDP 最小面（多轨取首个 video）；
- AnnexB 转发形态不解码（出帧由外部解码命令承担）；**RTSP 连接断开（EOF/重置）由 ReadAnnex 上抛、Next 检测并重连**；「连接存活但推流静默」形态无主动超时检测（读取无 deadline——os.Pipe 限制；ctx 取消路径可解除），登记 KI §43；
- RTSP 模拟服务端（pkg/rtspclient/sim.go）为测试面（mapper 测试与 e2e 复用），非产品交付物；
- GB28181 SIP 信令单独立项评估（不在本版，规划原文）；
- jpegScanner 修复对「元数据含 FFD9」场景改变结果（截早→完整），对正常流等价。

## N5 门禁（2026-10-02 全绿）
- [1] `go vet ./...`：VET OK。
- [2] 全仓测试（除 e2e/契约）：全 ok（仅 hack/ 下无测试文件的 `?` 行）。
- [3] `go test -race`（pkg/rtspclient 2.3s + pkg/video 3.4s + mappers/video 24.4s）：全绿。
  - 门禁首轮在此暴露两个真并发缺陷并已修复：① `close of closed channel`（Wait-goroutine 与
    `waitDecoder` 双关 procEnd）；② `DATA RACE`（`cmd.Wait` 被 startDecoder-goroutine 与
    dropAll/waitDecoder 并发调用）。修复：RTSPSource 改为 BridgeSource 同款 `procMu + takeProc`
    接管模式（Wait 恰好一次）+ watcher 只 Kill 不 Wait；`Next` 句柄判空经 `stdinRef()`
    锁内快照（消除未加锁字段读）。修复后 race 复跑全绿。
- [4] 契约 `go test ./tests/contract/`：11.994s 全绿（零改动验证）。
- [5] e2e 全量 `go test ./tests/e2e/ -timeout 25m`：**624.984s 全绿（exit=0，0 FAIL）**——
  v0.39.0 断网补传 / v0.40.0 告警设定值 / v0.41.0 采集用例零回归。
- 门禁日志：`.cluster/edgeflow-v0420/gates.log`（首轮）+ `gates-rerun.log`（[1][2][3] 复跑）+
  `e2e-final.log`（[5] 完整日志）。跑测前后 `lsof -i :12379,:12380` = 0（无 etcd 端口泄漏）。
- **独立复核（P0=0/P1=4/P2=9）处置后全量复跑**（`gates-rerun2.log`）：[1] VET OK /
  [2] 全仓 ok / [3] race×3 绿（2.268s+3.478s+13.627s）/ [4] 契约 12.404s /
  [5] e2e 全量 **630.362s 全绿（exit=0）**。复核处置批：P1×4 全清（断流错误上抛与重连/
  重试哨兵消费/验收强断言+排空脚本修正+reconnects 接出/冻结测试还原）+ P2×7 固定
  （URL 预检/CSRC 用例修正/多帧 drain/channel 过滤/FU-A 16MB 上限/Kill→Wait/文档修正）；
  mapper E2E 强断言版 PASS 9.52s（rec 0→1、frames 6→21、fps=3.01）；冻结带 v0340
  测试零改动（diff=0）复验。
