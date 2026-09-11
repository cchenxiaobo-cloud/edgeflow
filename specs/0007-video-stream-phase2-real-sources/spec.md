# Spec 0007：视频流管理与边缘推理对接（阶段二：实源接入 + 降级语义）

- 版本归属：v0.34.0（基线 4e37c92 = v0.33.0）
- 规范依据：spec 0004 分段声明（RTSP/GB28181 实源拉流「需单独裁定后阶段二交付」）+ KNOWN-ISSUES §32（v0310 复核 P2-1：实源接入时必须补可见降级与 streamOn 语义）+ 用户方向「继续开发后续功能」
- 裁定结论（本 spec 前置决策）：实源接入按**零依赖可行路线**交付——① MJPEG over HTTP 直连（IP 摄像头常见输出，stdlib 可解析）；② 外部进程桥（ffmpeg 等将 RTSP/GB28181 转 MJPEG 字节流经 stdout 接入；桥本身零依赖——exec 为 stdlib，唯一外部依赖是用户自行安装的 ffmpeg，且不引入 Go 模块依赖）。原生 RTSP/RTP 协议栈（需第三方库）与 GB28181 信令（SIP 栈）不实现，登记为后续裁定项。

## 用户故事与验收

### US-1 MJPEG over HTTP 实源（pkg/video）
- `MJPEGConfig{URL, ReconnectMs, TimeoutMs（响应头超时，0=不设）}`；`MJPEGSource` 实现 FrameSource（URL 须为 http(s)://host/...，构造期预检）：
  - HTTP GET（Accept: multipart/x-mixed-replace），按 multipart boundary 分帧（stdlib mime/multipart）；
  - 帧体 JPEG（SOI/EOI 校验 + image.DecodeConfig 取宽高）；坏帧跳过（计数）；
  - 流中断/连接错误：按 ReconnectMs 间隔重连（默认 1000ms；ctx 取消立即退出）；
  - Frame.Seq 内部单调计数；TsMs = 帧接收时刻毫秒。
- 验收：httptest 服务器（标准 multipart/x-mixed-replace 输出）——正常帧序、断开重连（两次连接拼接帧流）、ctx 取消即时返回、非 multipart 显式错误。

### US-2 外部进程桥实源（pkg/video）
- `BridgeConfig{Command, Args[], ReconnectMs}`；`BridgeSource` 实现 FrameSource：
  - exec.Command 启动，stdout 按 JPEG SOI/EOI 定界扫描分帧（跨读块边界状态机）；
  - 进程退出/启动失败 → 显式错误（含退出状态 + stderr 尾部 ≤512B）；ReconnectMs>0 时按间隔重启（默认 0 = 不退避？裁定：默认 1000 重启，0 表示不重启——字符串说清楚）；
  - ctx 取消 → Kill + Wait 回收（无僵尸）。
- 验收：本机 /bin/sh 脚本（printf 预制 MJPEG 字节流）——帧序正确、跨块分帧正确、命令不存在报错、非零退出报错（stderr 尾部在错误消息中）、ctx 取消回收。

### US-3 源工厂与配置扩展
- `pkgvideo.SourceConfig{Type, URL, Command, Args, ReconnectMs, Synth}` + `NewSource(cfg) (FrameSource, error)`：type ∈ {synthetic, mjpeg, bridge}；未知显式拒绝（延续「不做静默降级」纪律）。
- mappers/video 的 SourceConfig 同步扩展（URL/Command/Args/ReconnectMs 透传）+ validate 更新；配置示例：{type: "mjpeg", url: "http://cam.local/video.mjpg"} / {type: "bridge", command: "ffmpeg", args: ["-i","rtsp://...","-f","image2pipe","-vcodec","mjpeg","-"]}。
- 向后兼容：{type:"synthetic"} 配置行为逐字节不变（v0310 测试零改动）。

### US-4 streamOn 降级语义（v0310 P2-1 闭环）
- produceLoop 出帧错误（非 stop 路径）→ ① `sourceErrors++`；② log.Warnf 含错误全文（lastError 诊断面）；③ 自收口：整个 run 停止（produce + infer 循环退出，running=false → Collect 的 streamOn=0）；
- run 收口统一化：stopCh close 走 per-run sync.Once（Stop 与自收口共用，绝不二次 close）；running=false 由收口 goroutine 置（Stop 不再自行置——幂等语义保持）；
- `stream=1` 指令可重启（Start 幂等路径修改为：running=false 后重建 per-run 状态）；sourceErrors 累积不清零（诊断历史），重启成功后 streamOn=1。
- 验收：stub 出错源——错误后 streamOn 降为 0 + sourceErrors≥1 + infer 循环同步收口；重启后再出错可重复；Stop 幂等与 v0310 重启序列锚不变。

### US-5 装配与 e2e
- edgecore 装配路径不变（EDGEFLOW_VIDEO_MAPPER_CONFIG opt-in）；配置 type=bridge 全链路：sh 脚本产帧 → 推理 stub → 影子指标（framesTotal/streamOn/sourceErrors）。
- 验收：e2e ≥1（bridge）；默认（无环境变量）回归零影响。

## 冻结与兼容
- 契约 42 端点零改动（纯边缘侧，云侧零改动）；v0240–v0330 测试文件零改动；
- 零第三方依赖（stdlib：net/http、mime/multipart、os/exec、bufio 等）；
- 默认配置（synthetic）行为与 v0.33.0 逐字节一致；3.1.1/OPC-UA/MQTT 路径零触碰。

## 边界登记（非目标）
- 原生 RTSP/RTP/SRTP 协议栈与 GB28181 SIP 信令不实现（需第三方库或大工程量，后续单独裁定）；
- H.264/H.265 解码不实现（桥接路线由 ffmpeg 解码输出 MJPEG；MJPEG 直连路线天然 JPEG）；
- 云端 VideoStream 管理面（CRUD/快照/回放）仍待契约扩容轮裁定（KI §32 维持）；
- GPU 推理运行时集成仍待后续（HTTP 推理契约已可接任意服务）；
- mTLS/鉴权到摄像头：URL 内嵌凭证支持（http://user:pass@host/）由 stdlib 天然处理，不做额外凭证管理。

## as-built 登记（实现后核实，2026-09-11）

- **US-1** ✅：MJPEGSource——multipart 分帧（stdlib mime/multipart）、坏帧跳过（BadFrames 计数）、断流/网络错误按 ReconnectMs 重连（Reconnects 计数）、配置性错误（非 multipart/HTTP!=200）显式返回不重试；ctx 取消即时（request ctx 传播）。`Seq` 内部单调、TsMs 接收时刻。
- **US-2** ✅：BridgeSource——JPEG SOI/EOI 跨块定界（jpegScanner 状态机）+ 多帧待发队列（开发期修复：一次读块多帧只返回首帧的丢帧缺陷）；「未产帧即退出 → 显式错误（含退出状态+stderr 尾部 512B）、曾产帧后退出 → 重启」判据；stderr 自读管道（修复 shell 包装场景取消延迟 30s→51ms——Kill 只及直接子进程时 exec 内部 stderr 拷贝被 Wait 等待的卡死）；双重取消保障（Kill + stdout 读端关闭，procMu 防竞态）。
- **US-3** ✅：NewSource 三型分发 + 未知拒绝（含 rtsp 仍拒绝——原生协议栈未实现）；mapper SourceConfig 扩展（url/command/args/reconnectMs/timeoutMs）与 validate 三型校验；synthetic 默认路径逐字节不变（v0310 冻结面全绿）。
- **US-4** ✅：produceLoop 源错误 → sourceErrors++ + 全 run 自收口（stopOnce 与 Stop 共用、running 由收口 goroutine 置、Collect streamOn 自然降 0）+ 警告日志；stream=1 重启恢复（回归锚 TestV0340MapperSourceErrorDegrade）；正常停止不计数。
- **US-5** ✅：edgecore 装配路径不变（LoadConfig/NewMapper 接口零改动）；装配级 bridge e2e（真进程桥 + 真 HTTP 推理 stub + 指标断言）；无环境变量零行为。
- **测试锚**：16 例（pkg/video 13 + mappers/video 3）——MJPEG 5（帧序/重连/配置错误/ctx 取消/URL 预检）+ 桥 6（帧序/跨块/命令缺失/非零退出/重启/Close 回收）+ 定界器 1 + 源工厂 1 + Mapper 3（降级/桥 e2e/校验）；v0310 冻结面（14 例）零改动全绿；-race 绿。
- **开发期修复登记**（测试暴露的真实缺陷）见 KNOWN-ISSUES §35。

## 复核处置记录（v0340 复核轮，2026-09-11）

- **P0×0 / P1×1 / P2×6**（独立复核：通过，附窄窗 P1）。处置：
  - **P1-1 桥源停机赛跑回收缺口 → 修复**：出帧循环「帧交付后 select <-stop 退出」不再调 Next，原实现 Wait 唯一入口在 Next 内、watcher 仅 Kill——直接子进程成僵尸至宿主退出。新增 `BridgeSource.Close()`（io.Closer，幂等；takeProc 锁内接管与 waitProc 协调「Wait+close(done) 恰好一次」），mapper produceLoop defer 统一调用（含 Close 场景）。锚：`TestV0340BridgeCloseReaps`（交付间隙 Close <3s 回收 + 幂等 + 释放后可重建）。
  - **P2-1 计数口径 → 修字**：本文件 as-built 与 RELEASE-NOTES/README 统一为实测 16 例（pkg 13 + mappers 3）；v0310 冻结面 14 例。
  - **P2-2 US-1 文字 → 修字**：删「+warn」（实现为计数）、配置清单改实测字段（JPEGQuality/ReadTimeoutMs 未实现）。
  - **P2-3 MJPEG 永久性连接错误 → 修复**：NewSource 构造期 URL 预检（scheme http/https + host 非空），非法即显式拒绝，不进入运行期重连循环。锚：`TestV0340MJPEGBadURLRejected`。
  - **P2-4 jpegScanner 元数据 FFD9 → 登记**（KNOWN-ISSUES §35）：合规编码器熵数据不产裸 FFD9，但元数据段（EXIF 缩略图/COM）可含 → 帧截早、DecodeConfig 失败、静默计 badFrames；主流设备/ffmpeg 输出通常不触发；后续可按段长度解析。
  - **P2-5 日志裸读共享字段 → 修复**：锁内取值后日志行输出（无实竞，保持锁纪律）。
  - **P2-6 stream=1 自收口收尾窗口 → 登记**（KNOWN-ISSUES §35）：stream=1 恰落在自收口完成前毫秒窗口——回执 streamOn=1 但流随后停；窗口极小、补发可恢复。
