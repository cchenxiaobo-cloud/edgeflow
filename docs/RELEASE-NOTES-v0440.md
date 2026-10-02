# EdgeFlow v0.44.0 发布说明（流媒体分发子集 + 告警片段服务）

- 发布日期：2026-10-02
- 基线：v0.43.0（33d5316）
- 主题：流媒体分发子集 + 告警片段服务（发展规划 G22）——FLV 透传封装器
  （pkg/flvremux，H.264 AnnexB→FLV 零转码纯封装）、HTTP-FLV/WS-FLV 分发端点
  （云端流面 /media/streams/{name}/live.flv|live.ws）、告警片段检索
  （GET /api/v1/alarms/{alarmID}/segments，DeviceName+RaisedAt 时间窗 × videostream
  索引联合查询）；metrics 统计中间件补 Flush/Hijack/Unwrap 委托（流式响应能力）。
  契约扩容（69→72 端点）；零新依赖；默认零行为。

## N1 能力（新增）

### 1. FLV 透传封装器（pkg/flvremux，零依赖）
- FLV header/PreviousTagSize0/Tag（video 9 + script 18）字节级编码；
- AnnexB→AVCC（起始码 3/4 字节两形态扫描 → 4 字节大端长度前缀）；
- AVC sequence header（AVCDecoderConfigurationRecord：SPS/PPS 打包，
  lengthSizeMinusOne=3）；onMetaData script tag（AMF0 最小字段）；
- FrameType 语义（IDR=0x17 关键帧/其余 0x27）；CompositionTime 最小实现 0（无 B 帧）。
- 演示合成源 SyntheticH264Source（25fps、GOP 可配、SPS/PPS 带外）——**演示/测试面**
  （与 rtspclient/sim.go 同类，KI §45 登记非产品交付物）。

### 2. 云端流面（cmd/cloudcore/media_stream_api.go）
- 帧源注册表 streamRegistry（并发安全；演示源 env opt-in
  `EDGEFLOW_CLOUDCORE_DEMO_H264_STREAMS=逗号分隔流名`）；
- `GET /media/streams/{name}/live.flv`：HTTP chunked（video/x-flv；首 flush =
  FLV header + sequence header + onMetaData；帧循环 Delta 增量推送）；
- `GET /media/streams/{name}/live.ws`：WS 升级后二进制帧透传 FLV 字节流；
- 无注册帧源 → 404（默认零行为）；客户端断开/源终止 → 收口（含日志）；
- **挂载口径**：/media/* 流端点挂根 mux（协议端点面，如 /ocsp——不经 Bearer
  Token）；告警片段检索挂 apiMux（auth/audit 覆盖）。

### 3. 告警片段服务（按告警索引取片段——云端联合检索，零边缘改动）
- `GET /api/v1/alarms/{alarmID}/segments`：alarm(DeviceName+RaisedAt) ×
  videostream 片段索引时间窗（默认 [RaisedAt-5s, +60s]）→ SegmentRef 列表
  （含 sha256）；回放复用 v0.43 端点。
- 告警缺失 404；无关联流/窗内无片段 → 200 空列表。

### 4. metrics 中间件流式响应能力（cloud/pkg/metrics）
- statusRecorder 补 Flush/Hijack/Unwrap 委托——此前包装器未实现 Flusher/Hijacker，
  chunked 流式响应（FLV 拉流）与 WS 升级在统计链下不可用（**本版 e2e 发现并修复**）；
- 纯增量接口（既有计数/审计语义零变化）。

## N2 兼容与冻结
- **契约扩容**：69→72 端点（流媒体分发 2 + 告警片段检索 1）；消息 16 维持；
  契约测试（源级/运行时/文档一致性）全量同步。
- **默认零行为**：未注册演示源 → 分发端点 404；无窗内片段 → 空列表。
- 冻结带 v0240–v0350 测试零改动；v0.39–v0.43 测试零改动；go.mod 零变化
  （websocket 复用 cloudhub 既有依赖）。

## N3 测试（grep 口径）
- pkg/flvremux +4 例（header/seq header 字节断言/AnnexB→AVCC 往返/起始码两形态/
  空流拒绝）。
- cmd/cloudcore +5 例（live.flv 拉流解析与 404/live.ws 拉流/写超时豁免防回归/
  告警片段检索窗口语义/告警缺失 404 与空列表）。
- tests/e2e +2 例（TestV0440FLVDistributionE2E：HTTP-FLV 17s 长读铁证（300 tags/
  时戳推进，>WriteTimeout 15s 窗口）+ WS-FLV 增量帧；TestV0440AlarmSegmentsE2E：
  告警→片段→播放运行时链路）。
- 合计：新增 11 例（单测 9 + e2e 2）全绿。

## N4 边界（登记 KNOWN-ISSUES §45）
- FLV 仅 H.264 视频透传（音频 AAC 不做；无转码）；CompositionTime=0（B 帧重排不做）；
- 生产帧源（边缘推流/云端 RTSP 拉流）不在本版——演示源为测试面；
- WS-FLV 无子协议协商；HLS（TS/切片/m3u8）评估登记不入版；
- 直播时移/拖动不做；告警检索窗固定默认；多告警片段去重不做；
- /media/* 流端点免 Bearer Token（协议端点定位）——防盗链/签名 URL 后续；
- 流式端点长连接豁免：newHTTPServer 的 ReadHeaderTimeout 5s/ReadTimeout 10s/
  WriteTimeout 15s 面向短 JSON 设计——live.flv 按连接清读+写 deadline
  （http.NewResponseController 沿 Unwrap 链穿透），live.ws 于 Hijack 后清底层
  conn deadline；e2e 以 17s 长读（>15s 窗口）为铁证断言（复核发现修复）。

## N5 门禁（2026-10-02 全绿）
- [1] `go vet ./...`：VET OK。
- [2] 全仓测试（除 e2e/契约）：首轮 cmd/edgecore **TestNewAlarmIDUnique 偶发 FAIL**——
  既有 `newAlarmID` 2 字节随机后缀在同毫秒 100 次生成下约 7.5% 生日碰撞（本版未触碰
  告警面；e2e 前置构建时序暴露）→ 修复：后缀扩 4 字节（碰撞概率降至 ~0.01%，格式
  语义不变）→ 复跑 20 次×5 轮 + 全包全绿（gates-rerun.log）。
- [3] `go test -race`（触碰 6 包：pkg/flvremux / cloud/pkg/metrics / cmd/cloudcore /
  cloud/pkg/videostream / cloud/pkg/mediastore / pkg/mediaup）：全绿
  （2.1s/2.4s/21.1s/3.8s/2.2s/2.6s）。
- [4] 契约 `go test ./tests/contract/`：14.321s 全绿（72 端点/16 消息）。
- [5] e2e 全量 `go test ./tests/e2e/ -timeout 25m`：**685.257s 全绿（exit=0，0 FAIL）**——
  含 v0.39 补传/v0.40 告警/v0.41 采集/v0.42 RTSP/v0.43 媒资零回归 + **v0.44 新增
  TestV0440FLVDistributionE2E（HTTP-FLV+WS-FLV 拉流演示）与
  TestV0440AlarmSegmentsE2E（告警→片段→播放）**（单项先跑 PASS 见 e2e-v0440-run*.log）。
- 复核处置（复核员超时前锁定三疑点，逐一处置）：① newHTTPServer 超时掐长连接——
  流端点读+写 deadline 豁免（见 N4）+ 500ms 短超时防回归单测 + e2e 17s 长读铁证；
  ② serveLiveWS 缺 Sync——补（防 Delta 重放头部，与 HTTP-FLV 口径一致）；
  ③ WS 读泵生命周期——核实无泄漏（writer 退出 → defer Close → 读泵随 ReadMessage
  错误退出）。①② 修复后 cmd/cloudcore 5 例 + e2e 双用例复跑全绿。
- 门禁日志：`.cluster/edgeflow-v0440/gates.log`（首轮含 [2] flaky 诚实登记）+
  `gates-rerun.log`（[2] 修复后复跑）+ `e2e-final.log`（[5] 完整日志）。
  跑测前后 `lsof -i :12379,:12380` = 0（无 etcd 端口泄漏）。
