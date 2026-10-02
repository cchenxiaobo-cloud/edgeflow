# Spec 0016：视频管理面——VideoStream 模型与媒资回放（G21）

- 版本归属：v0.43.0（基线 667a679 = v0.42.0）
- 规范依据：《EdgeFlow 后续开发计划（对齐智能边缘计算平台解决方案）》v0.43 定义
  （差距 G21 视频管理面：CRUD/状态/快照预览/告警片段回放 + 契约扩容 + 节点设备
  关联 + 录像留存边界）+ 用户方向「持续迭代开发」。
- 裁定结论（本 spec 前置决策）：
  - 媒资上行通道 = **既有上送补传队列**（v0.39 UplinkQueue）：新消息类型
    `MediaUpload`（分片，单条 ≤64KB 上限内），断网补传 100% 复用队列留存/重放/
    优先级/至少一次语义；云端按 (mediaId, chunkSeq) 幂等落盘。
  - 片段格式 = **MJPEG（JPEG 序列拼接）**——由视频管道既有 JPEG 帧直接拼接，
    零转码零新依赖；快照 = 单帧 JPEG。平台片段级留存（NVR 利旧为主，大录像不落平台）。
  - 云端存储：videostream（流索引，etcd 写穿，alarmstore/rulestore 同构）+
    mediastore（二进制分片组装与对象存储，目录存放 + etcd 元数据索引）。
  - 契约扩容：+8 端点（61→69）、+1 消息类型（15→16）；契约测试与三处文档计数全量对齐。
  - 默认零行为：media 采集未配置时 mapper 零变化；云侧新端点/新 handler 不影响既有面；
    零第三方依赖（宪法 II）；冻结测试（v0240–v0350）零改动；go.mod 零变化。
  - 告警关联留位：片段元数据含 alarmId 可空字段（本版无视频推理→告警源，登记 KI §44）。

## 用户故事与验收

### US-1 媒资分片上行（pkg/mediaup，边缘侧）
- `Clip`：deviceName / snapshot(JPEG) / segment(MJPEG 拼接) / frameCount / detections /
  whenMs / alarmId(留位)。
- `Uploader`：spool 目录落盘（`<mediaId>.bin` + `.meta.json`）→ 构建分片
  （原始 ≤32KB/片，base64 后 <64KB 队列上限）→ 经 Enqueuer 接口入队 + Notify
  唤醒补传 worker；发送与重试由既有 worker 承担（至少一次）。
- Janitor（周期）：全部分片离队（Ack 后删行）→ 删本地副本；超龄（默认 24h）
  未传完 → 清理并计数（dropped）；磁盘水位防御。
- mediaId 唯一：`m-<kind>-<whenMs>-<seq>`（进程内原子序号）。
- 验收（单测）：分片构建（界长/序号/总量/sha256）；spool 写读；Janitor
  完成清理与超龄清理；Enqueuer 失败不丢（本地留存重试语义——由队列重放）。

### US-2 边缘采集装配（mappers/video 扩展 + cmd/edgecore）
- 配置块 `"media": {"enabled", "segmentFrames"(默认 8，界长 64), "minIntervalMs"(默认 3000)}`。
- 帧环形缓冲（出帧循环写入）；推理循环检出 detections≥1 且距上次触发 ≥minIntervalMs
  → 组装 Clip（快照=当前帧；片段=最近 N 帧拼接）→ `MediaSink` 回调（未注入零行为）。
- cmd/edgecore：video media.enabled 且补传开启 → 创建 Uploader
  （EDGEFLOW_MEDIA_SPOOL_DIR，默认 data/media）注入 mapper；补传关闭时告警并跳过。
- 验收（单测 + e2e）：环形缓冲边界（不足 N 帧）+ 节流间隔 + 无 sink 零行为 +
  端到端采集上云（见 US-5）。

### US-3 云端媒资存储（cloud/pkg/mediastore）
- 分片落盘 `<dir>/incoming/<mediaId>/<seq>.part`；组齐（收到全部 seq）→ 顺序拼接
  → sha256 校验 → `<dir>/objects/<mediaId>.bin`；元数据 etcd 写穿
  （/edgeflow/media/<mediaId>，含 state=complete 与描述字段）；Load 恢复索引。
- 幂等：重复分片覆盖；完成后重复分片忽略；校验失败拒绝且保留分片（可重传修复）。
- 目录：EDGEFLOW_CLOUDCORE_MEDIA_DIR（默认 data/media）。
- 验收（单测）：分片乱序到达组装/重复幂等/sha 校验失败/元数据恢复/并发分片安全。

### US-4 云端 VideoStream 资源模型（cloud/pkg/videostream + API）
- Stream：name/nodeId/deviceName/sourceType/status/description/snapshotMediaId+
  snapshotAt/lastSeenAt/segments[]（界长 200）/createdAt/updatedAt；etcd 写穿。
- 媒资到达自动 Ensure（无则创建）+ 快照替换 / 片段追加（去重）；Delete 不删媒资
  文件（留存边界——KI §44 登记）。
- API（+8 端点，契约 61→69）：
  - GET/POST `/api/v1/videostreams`（列表/创建；dup→409）
  - GET/PUT/DELETE `/api/v1/videostreams/{name}`（详情/更新/删除；404 语义）
  - GET `/api/v1/videostreams/{name}/snapshot`（image/jpeg 字节）
  - GET `/api/v1/videostreams/{name}/segments`（列表）
  - GET `/api/v1/videostreams/{name}/segments/{mediaID}`（回放字节 video/x-mjpeg）
- cloudhub：`SetMediaUploadHandler`（依赖注入，锁外回调；未注册仅日志）。
- 验收（单测）：CRUD/409/404/Ensure 自动建流/快照替换/片段界长/字节回放端点。

### US-5 断网补传端到端（tests/e2e）
- 链路：cloudcore（真实进程）+ edgecore（EDGEFLOW_VIDEO_MAPPER_CONFIG media on +
  EDGEFLOW_EDGECORE_UPLINK=on + EDGEFLOW_MEDIA_SPOOL_DIR）+ 本地推理 stub（检出恒真）
  + 合成源（320×240@5fps）。
- 场景：在线采集上云（快照+片段经 API 可回放、字节校验）→ 停云断网（edge 持续
  采集积压）→ 同端口/同数据目录重启云 → 补传完成（**断网窗口内 capturedAt 的
  新片段到达并完整回放**——重放铁证）。
- 验收断言：快照 JPEG SOI/EOI + 尺寸；片段 JPEG 序列逐帧 SOI/EOI + 帧数==frameCount
  + sha256；断网窗片段 capturedAt 落窗 + 重启后才可见。

## 兼容与冻结
- 契约：扩容 +8 端点/+1 消息（61→69 / 15→16），契约测试同步更新；其余既有面零改动。
- 冻结带 v0240–v0350 测试零改动；go.mod/go.sum 零变化；零新依赖。
- 默认零行为：media 未配置 → mapper/edgecore 零变化；云端新 handler 未注入 → 消息仅
  日志；新端点只增不改。
- 幂等与至少一次：分片级重复覆盖；媒体级 sha256 校验；不承诺恰好一次。

## 测试锚（as-built 回填）
- pkg/mediaup/mediaup_test.go：3 例（分片重组一致性/Janitor 完成与超龄/载荷校验）。
- mappers/video/v0430_test.go：2 例（采集触发与零行为）。
- cloud/pkg/videostream/videostream_test.go：2 例（CRUD/Ensure 挂接）。
- cloud/pkg/mediastore/mediastore_test.go：4 例（乱序组装/幂等/sha 失败/缺失）。
- cmd/cloudcore/video_api_test.go：2 例（CRUD/快照与片段回放端点）。
- edge/pkg/metamanager/v0430_uplink_ids_test.go：1 例（PendingUplinkIDs）。
- tests/e2e/v0430_video_manage_e2e_test.go：1 例（断网补传回放全链）。
- 合计 15 例（单测 14 + e2e 1；grep -c '^func Test' 口径）。

## 边界登记（随文档落 KI §44）
- 片段=MJPEG 拼接（无转码/无 MP4 封装）；快照=触发帧单帧；
- 平台片段级留存（NVR 利旧为主）；流/媒体删除不联动删文件（保留策略后续）；
- 上传至少一次（云端幂等消化重复）；无分片级断点续传协议（重传=覆盖）；
- 告警关联字段留位（视频推理→告警源不在本版）；多轨/音频/字幕不做；
- 磁盘水位：spool/媒资目录超龄清理策略（界内简单实现，水位告警后续）。
