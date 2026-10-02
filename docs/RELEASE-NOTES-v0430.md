# EdgeFlow v0.43.0 发布说明（视频管理面）

- 发布日期：2026-10-02
- 基线：v0.42.0（667a679）
- 主题：视频管理面（发展规划 G21）——云端 VideoStream 资源模型（CRUD/状态/快照
  预览/片段回放）、媒资（快照/片段）分片上传与**断网补传**（复用 v0.39 上送队列）、
  与节点/设备关联、录像留存边界（NVR 利旧为主、平台片段级留存）。契约扩容
  （61→69 端点、15→16 消息）；默认零行为；零新依赖。

## N1 能力（新增）

### 1. 媒资上行模型（MediaUpload 分片 + 上送队列）
- 新消息类型 `MediaUpload`（边→云）：mediaId/kind(snapshot|segment)/nodeId/
  deviceName/streamName/capturedAt/contentType/frameCount/totalBytes/sha256/
  chunkSeq/chunkTotal/chunkData（base64）。
- 分片原始 ≤32KB（base64 后 ~44KB，低于补传单条 64KB 上限）；同媒体分片同优先级
  FIFO 保序；断网 100% 复用 v0.39 持久队列（留存/重放/至少一次）；云端按
  (mediaId, chunkSeq) 幂等落盘、组齐后 sha256 校验。入队失败（SQLite 故障等
  极低概率路径）= 本段媒资放弃：仅已入队分片由队列重放、未入队分片不重传
  （spool 孤儿 1h 清理；复核 P2-1 措辞对齐）。

### 2. 边缘媒资采集（pkg/mediaup + mappers/video 扩展）
- `pkg/mediaup`：Clip → spool 落盘（`<mediaId>.bin` + `.meta.json`）→ 分片入队
  （Enqueuer 接口注入）→ Janitor（分片全部离队 → 清理本地副本；超龄 24h 清理；
  孤儿清理；计数可观测）。Uploader 内部异步化（采集不阻塞推理循环）。
- `mappers/video` media 配置块：`{"enabled", "segmentFrames"(1..64 默认 8),
  "minIntervalMs"(默认 3000)}`；帧环形缓冲（界长 64）；检出≥1 且距上次触发 ≥
  间隔 → 组装 Clip（快照=当前帧 JPEG；片段=最近 N 帧 MJPEG 拼接）→ MediaSink。
  未注入出口/未启用零行为。
- `cmd/edgecore` 装配：video media.enabled 且补传开启（`EDGEFLOW_EDGECORE_UPLINK=on`）
  → 创建 Uploader（spool 目录 `EDGEFLOW_MEDIA_SPOOL_DIR`，默认 data/media）经
  holder 延迟注入（mapper 先建、出口后置填充）；停机收口。

### 3. 云端媒资存储（cloud/pkg/mediastore）
- 分片落盘 `<dir>/incoming/<mediaId>/<seq>.part` → 组齐 → 顺序拼接 → sha256 校验
  → `<dir>/objects/<mediaId>.bin`（原子 rename）；元数据 etcd 写穿
  （/edgeflow/media/<mediaId>）；Load 恢复索引。
- 幂等：重复分片覆盖；完成后重复分片忽略；在途元数据一致性拒绝；校验失败保留
  分片（可重传修复）。目录：`EDGEFLOW_CLOUDCORE_MEDIA_DIR`（默认 data/media）。

### 4. 云端 VideoStream 资源模型与 API（cloud/pkg/videostream + 8 端点）
- Stream：name/nodeId/deviceName/sourceType/status/description/snapshotMediaId+
  snapshotAt/lastSeenAt/segments[]（界长 200）/createdAt/updatedAt；etcd 写穿。
- 媒资到达自动 Ensure（无则建流）+ 快照替换 + 片段追加（去重）；Delete 不删
  媒资文件（留存边界——KI §44）。
- 8 端点：video streams CRUD + snapshot 字节（image/jpeg）+ segments 列表 +
  segment 回放（video/x-mjpeg，X-Frame-Count 头）；错误映射 404/409/400。
- cloudhub `SetMediaUploadHandler`（依赖注入 / 锁外回调 / 未注册仅日志零影响）。

## N2 兼容与冻结
- **契约扩容**：61→69 端点（+8 视频管理面）、15→16 消息（+MediaUpload）；
  契约测试（源级/运行时/文档一致性）全量同步；既有 61 行逐字节不变。
- **默认零行为**：video media 未配置 → mapper/edgecore 零变化；补传未开启 → 采集
  不上传（Warn，不阻断）；云端新 handler 未注入 → 消息仅日志。
- 冻结带 v0240–v0350 测试零改动；v0.39–v0.42 测试零改动；go.mod 零变化。
- 升级零迁移：新增 etcd 键空间（/edgeflow/videostreams/*、/edgeflow/media/*）与
  媒资目录；重启自动恢复。

## N3 测试（grep 口径，`grep -c '^func Test'`）
- pkg/mediaup +3 例（分片重组一致性/Janitor 完成与超龄清理/载荷校验矩阵）。
- mappers/video +2 例（采集触发：节流/帧数界长/快照内容/零行为）。
- cloud/pkg/videostream +2 例（CRUD/409/404；Ensure 自动建流/快照替换/片段去重与界长）。
- cloud/pkg/mediastore +4 例（乱序组装/重复幂等与一致性拒绝/sha 失败保留分片/缺失）。
- cmd/cloudcore +2 例（API 8 端点 CRUD 与状态码；快照/片段字节回放与头校验）。
- edge/pkg/metamanager +1 例（PendingUplinkIDs：命中介集/离队空集/幂等）。
- tests/e2e +1 例（TestV0430VideoManageE2E：在线媒资上云 → 停云断网 20s 持续采集
  → 重启云补传重放（窗内 capturedAt 铁证）→ 全部片段回放校验）。
- 合计：新增 15 例（单测 14 + e2e 1）全绿。

## N4 边界（登记 KNOWN-ISSUES §44）
- 片段格式=MJPEG（JPEG 序列拼接）；无转码/无 MP4 封装；
- 平台片段级留存（NVR 利旧为主）；流/媒资删除不联动删文件（保留策略后续）；
- 上传至少一次（云端幂等消化重复）；无分片级断点续传协议（重传=覆盖）；
- 告警关联字段留位（alarmId 可空；视频推理→告警源不在本版）；
- spool/媒资目录超龄清理为界内简单策略（磁盘水位告警后续）；
- 快照=触发帧单帧（高质量单帧留位后续）。

## N5 门禁（2026-10-02 全绿）
- [1] `go vet ./...`：VET OK。
- [2] 全仓测试（除 e2e/契约）：全 ok（仅 hack/ 下无测试文件的 `?` 行）。
- [3] `go test -race`（触碰 9 包：pkg/mediaup / pkg/protocol / mappers/video /
  cloud/pkg/videostream / cloud/pkg/mediastore / cloud/pkg/cloudhub / cmd/cloudcore /
  cmd/edgecore / edge/pkg/metamanager）：首轮 pkg/mediaup 失败——**测试计数器竞态**
  （notify 回调自增变量无同步；实现代码无竞态），修复（atomic + meta 落盘等待硬化）
  后全量复跑 9 包全绿（1.9s/2.8s/14.5s/2.3s/2.9s/8.3s/22.6s/6.1s/9.1s）。
- [4] 契约 `go test ./tests/contract/`：13.048s 全绿（69 端点/16 消息：源级/运行时/
  文档一致性守卫）。
- [5] e2e 全量 `go test ./tests/e2e/ -timeout 25m`：**642.967s 全绿（exit=0，0 FAIL）**——
  含 v0.39 断网补传 / v0.40 告警设定值 / v0.41 采集 / v0.42 视频（既有）零回归 +
  **v0.43 新增 TestV0430VideoManageE2E**（在线媒资 2 段 + 断网补传 6 段窗内回放校验）。
- 单项先行：TestV0430VideoManageE2E 单跑 PASS 41.3s（e2e-v0430-run1.log）。
- 门禁日志：`.cluster/edgeflow-v0430/gates.log`（首轮含 [3] 竞态诚实登记）+
  `gates-rerun.log`（[3] 修复后复跑）+ `e2e-final.log`（[5] 完整日志）。
  跑测前后 `lsof -i :12379,:12380` = 0（无 etcd 端口泄漏）。
