# EdgeFlow v0.34.0 发布说明 — 视频流阶段二：实源接入 + 降级语义

发布日期：2026-09-11 ｜ 上游规格：specs/0007-video-stream-phase2-real-sources/spec.md（spec 0004 分段声明的阶段二）

## 亮点

视频流管理模块打通**实源接入**：新增 MJPEG over HTTP 直连源与外部进程桥源（ffmpeg 等
将 RTSP/GB28181 转 MJPEG 字节流接入），并完成 v0.31.0 复核登记的 streamOn 可见降级
语义（源错误不再静默退出）。全程零第三方依赖（stdlib + os/exec；桥接路线唯一外部
依赖是用户自行安装的桥命令，不引入 Go 模块依赖）。

### N1 MJPEG over HTTP 直连源（pkg/video）

- `MJPEGSource`：HTTP GET `multipart/x-mixed-replace` 长连接按 boundary 分帧；
  帧体 JPEG 校验（SOI/EOI + 宽高解码），坏帧跳过计数（`BadFrames()`）。
- 断流/网络错误按 `ReconnectMs`（默认 1000）自动重连（`Reconnects()` 计数）；
  配置性错误（非 multipart、HTTP != 200）显式返回（供上层可见降级）。
- ctx 取消即时退出；`Frame.Seq` 内部单调计数、`TsMs` 接收时刻。
- 覆盖 IP 摄像头常见 MJPEG 输出场景；URL 支持内嵌凭证（http://user:pass@host/）。

### N2 外部进程桥源（pkg/video）

- `BridgeSource`：exec 启动命令（如 `ffmpeg -i rtsp://... -f image2pipe -vcodec mjpeg -`），
  stdout 按 JPEG SOI/EOI 定界分帧（跨读块状态机、多帧待发队列防丢帧）。
- 进程启动失败/未产帧即退出 → 显式错误（含退出状态 + stderr 尾部 ≤512B）；
  曾产帧后退出 → 按 `ReconnectMs` 自动重启（流中断重连语义）。
- ctx 取消：Kill 进程 + 关闭管道读端（shell 包装场景孙进程持管道不阻塞回收）；
  stderr 自读管道——cmd.Wait 不再等待写端全关（修复 shell 包装下取消延迟缺陷）。
- `Close()`（io.Closer，幂等）：出帧循环退出路径统一回收进程（帧交付间隙退出
  不再依赖 Next 内回收——复核 P1 修复，无僵尸残留）；释放后可重建新进程。

### N3 源工厂与配置扩展

- `pkgvideo.NewSource(SourceConfig{Type,...})`：synthetic / mjpeg / bridge 分发；
  未知类型显式拒绝（延续「不做静默降级」纪律）。
- 配置文件扩展：`source.type=mjpeg`（url/reconnectMs/timeoutMs）、
  `source.type=bridge`（command/args/reconnectMs）；synthetic 默认行为逐字节不变。

### N4 streamOn 可见降级语义（v0.31.0 复核 P2-1 闭环）

- 出帧错误（源错误）不再静默退出：`sourceErrors` 计数 + 全 run 自收口
  （infer 循环同步停，`streamOn` 自动降 0）+ 警告日志（错误全文）。
- run 停机统一走 per-run `sync.Once`（Stop 正常停止与源错误自收口共用，
  绝不二次 close）；`stream=1` 指令可重启恢复。
- 正常停止不增 `sourceErrors`；诊断历史保留。

### N5 装配与测试

- edgecore 装配路径不变（`EDGEFLOW_VIDEO_MAPPER_CONFIG` opt-in）。
- 新增 16 例测试：MJPEG 5（帧序/重连/配置错误/ctx 取消/URL 预检）+ 桥 6
  （帧序/跨块/命令缺失/非零退出/重启/Close 回收）+ 定界器 1 + 源工厂 1 + Mapper 3
  （降级语义/桥 e2e/配置校验）。`-race` 绿。

## 变更清单

| 包 | 变更 |
|---|---|
| pkg/video | v0340.go 新增：SourceConfig/NewSource、MJPEGSource（multipart 分帧/重连）、BridgeSource（JPEG 定界/进程管理/双重取消保障）、jpegScanner、tailBuffer |
| mappers/video | SourceConfig 扩展（url/command/args/reconnectMs/timeoutMs）、validate 三型校验、默认源工厂走 NewSource、降级语义（sourceErrors/metrics/stopOnce 收口/Collect） |
| specs/0007 | 新规格（US-1..US-5 + 边界登记 + 阶段二裁定结论） |
| docs | RELEASE-NOTES/README/Chart 0.33.0→0.34.0/DEVELOPMENT-SPEC 缺口表/ROADMAP §30/KNOWN-ISSUES §35 |

## 测试

- 新增 16 例（pkg/video 13 + mappers/video 3）；`-race -count=1` 绿。
- 回归：v0240–v0330 全部零改动绿（含 v0310 视频阶段一冻结面）；契约 42 端点零改动；e2e 包全量绿。

## 升级兼容

默认配置（synthetic）行为与 v0.33.0 逐字节一致；无环境变量零行为；契约 42 端点不变；
MQTT/OPC-UA/3.1.1 路径零触碰。原生 RTSP/RTP 协议栈与 GB28181 信令、云端 VideoStream
管理面（契约扩容）、GPU 运行时集成仍为后续裁定项（spec 0007 边界登记 + KI §35）。
