# 视频接入指南（v0.42.0，视频源型与 RTSP 原生子集）

适用范围：`mappers/video` 视频 Mapper（视频帧源 → 推理服务 → 检测结果 → 告警/上送）与自研 RTSP 客户端（`pkg/rtspclient`）。

## 1. 视频帧源总览

| 源型 | 配置字段 | 出帧方式 | 依赖 | 版本 |
|---|---|---|---|---|
| `synthetic` | `synthetic`（分辨率/场景/目标数） | 内置合成发生器 | 无 | v0.34.0 |
| `mjpeg` | `url`（http(s)://，可含内嵌凭证） | MJPEG over HTTP 直读 + `jpegScanner` 分帧 | 无 | v0.34.0 |
| `bridge` | `command` + `args`（如 ffmpeg） | 外部进程 stdout MJPEG | ffmpeg | v0.34.0 |
| `rtsp` | `url`（rtsp://，可含内嵌凭证）+ `decoder`（H.264→MJPEG 命令） | 自研 RTSP 客户端拉流 → AnnexB → 解码进程 stdin → stdout MJPEG | ffmpeg | v0.42.0 |

未知源型显式拒绝（不静默降级）。三种外部源型（mjpeg/bridge/rtsp）可并存于不同设备（每设备配一个源型）。

## 2. rtsp 源型配置（v0.42.0，spec 0015）

```json
{
  "deviceName": "cam-01",
  "source": {
    "type": "rtsp",
    "url": "rtsp://user:pass@192.168.1.64:554/Streaming/Channels/101",
    "decoder": "ffmpeg -f h264 -i pipe:0 -f mjpeg -q:v 5 pipe:1",
    "reconnectMs": 1000,
    "timeoutMs": 5000
  },
  "inference": { "url": "http://infer-svc:9000/detect", "timeoutMs": 2000 }
}
```

- `url` 必填：`rtsp://[user:pass@]host[:port]/path`；内嵌凭证走 Basic 认证（401 挑战后重试恰一次）；也可用 `rtsp.SetCredentials` 显式提供。
- `decoder` 必填：H.264 AnnexB（stdin）→ MJPEG（stdout）的外部命令。最简可用 ffmpeg：
  `ffmpeg -f h264 -i pipe:0 -f mjpeg -q:v 5 pipe:1`
  （`-f h264` 明确输入格式；**不要**写 `-rtsp_transport`/`-stimeout` 等网络参数——
  管道输入无 RTSP 选项，推流由自研客户端完成。）
- `reconnectMs`：断流重连间隔（默认 1000）；`timeoutMs`：RTSP 信令超时（默认 5000）。
- 凭证与命令中的敏感信息建议由部署侧注入（配置文件权限 0600），不在本仓示例中写真实口令。

### 2.1 管线与时序

```
RTSP 摄像头
  │ TCP + RTSP 信令（OPTIONS→DESCRIBE→SETUP→PLAY）
  │ 媒体：$ 帧（interleaved channel 0）→ RTP → H.264（FU-A/STAP-A）
  ▼
pkg/rtspclient：重组为 AnnexB（00 00 00 01 + NAL）
  │ 写入 decoder stdin
  ▼
外部解码进程（ffmpeg）：H.264 → MJPEG 帧
  │ stdout
  ▼
jpegScanner：按 JPEG marker 结构分帧（SOI…EOI）
  ▼
Frame{w,h,jpeg} → 推理服务 → 检测结果 → 告警/上送
```

### 2.2 源健康与自愈
- 断流检测：RTSP 连接断开或解码进程退出 → 整体销毁重连（`reconnects` 指标累加 + `reconnectMs` 退避）。
- ctx 取消（设备下线/停止）：watcher `Kill` 解码进程 + 关闭 stdin/stdout 管道解除阻塞读；RTSP 连接关闭。
- 畸形处理：非法 NAL type/PT 丢弃计数（不断流）；未闭合 JPEG 帧按 maxPending 16MB 防御丢弃。
- 指标：mapper 既有 `streamOn / framesTotal / framesDropped / fps / inferTotal / inferFailTotal` + RTSP 源新增 `reconnects`。

## 3. 限制（详见 KNOWN-ISSUES §43）
- 仅 RTP/AVP/TCP（interleaved）；**UDP 传输、SRTP、HTTP 重定向不支持**。
- 仅 H.264（FU-A/STAP-A/单 NAL）；**H.265、音频轨不转码**（音频通道数据丢弃计数）。
- SDP 最小面：多轨取首个 video 轨。
- RTSP 连接断开（EOF/重置）由源检测并自动重连（`reconnects` 计数）；「连接存活但推流静默」无主动超时（管道读无 deadline），由 ctx 取消路径兜底回收。
- 自研客户端与 `pkg/rtspclient/sim.go` 模拟服务端（测试面）配套，但模拟服务端不进入产品交付。

## 4. 验证
```bash
# 包级：信令/重组/认证/边界（8 例）
go test ./pkg/rtspclient/
# C6 jpegScanner 修复 + 桥接零回归
go test ./pkg/video/
# rtsp 源型 mapper 全链（模拟服务端→拉流→解码→推理 stub→断流自愈）
go test ./mappers/video/ -run TestV0420 -v
```


## 5. 媒资采集与视频管理面（v0.43.0，spec 0016）

### 5.1 边缘采集配置（video 配置 media 块）
```json
{
  "deviceName": "cam-01",
  "source": { "type": "rtsp", "url": "rtsp://...", "decoder": "ffmpeg -f h264 -i pipe:0 -f mjpeg pipe:1" },
  "inference": { "url": "http://infer-svc:9000/detect" },
  "media": { "enabled": true, "segmentFrames": 8, "minIntervalMs": 3000 }
}
```
- 检出≥1 且距上次触发 ≥ `minIntervalMs` → 采集：快照=当前帧 JPEG；片段=最近
  `segmentFrames` 帧 MJPEG 拼接（帧数 1..64，默认 8）。
- 上传依赖补传开启（`EDGEFLOW_EDGECORE_UPLINK=on`）+ spool 目录
  （`EDGEFLOW_MEDIA_SPOOL_DIR`，默认 data/media）；未满足时不上传（启动 Warn）。
- 断网时媒资入 spool + 持久队列，网络恢复自动补传（至少一次，云端幂等）。

### 5.2 云端管理 API（8 端点，契约 61→69）
- `GET/POST /api/v1/videostreams`（列表/创建）；`GET/PUT/DELETE /api/v1/videostreams/{name}`；
- `GET /api/v1/videostreams/{name}/snapshot`（最新快照 JPEG 字节，附 X-Frame-Count=1）；
- `GET /api/v1/videostreams/{name}/segments`（片段索引）；
- `GET /api/v1/videostreams/{name}/segments/{mediaID}`（片段回放，video/x-mjpeg +
  X-Frame-Count）。
- 媒资到达自动建流（无需先创建）；DELETE 仅删索引不删媒资文件（留存边界 KI §44）。
- 媒资目录：`EDGEFLOW_CLOUDCORE_MEDIA_DIR`（默认 data/media）。

### 5.3 验证
```bash
go test ./pkg/mediaup/ ./cloud/pkg/videostream/ ./cloud/pkg/mediastore/   # 媒资单测
go test ./mappers/video/ -run TestV0430                                   # 采集触发
go test ./tests/e2e/ -run TestV0430VideoManageE2E                        # 断网补传回放
```
