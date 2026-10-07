# EdgeFlow v0.45.0 发布说明（模型面扩展 + 困难样本回传 + 推理运行时）

- 发布日期：2026-10-03
- 基线：v0.44.0（cd396d3）
- 主题：模型面扩展 + 困难样本回传 + 推理运行时扩展（发展规划 G23 + G25 + G24）——
  模型 Modality 元数据与场景关联（G23）、困难样本收集/回传/检索（G25，复用
  MediaUpload 通道与 v0.39 补传队列，零新队列）、加速卡探测上报（G24，env 注入 +
  可插拔探测器，Register 可选字段 accels）。契约扩容（72→75 端点）；零新依赖；
  默认零行为（HARDSAMPLE/ACCELS 均 opt-in）。

## N1 能力（新增）

### 1. 模型面扩展（G23）
- Modality 元数据约定键 `modality`（vision 缺省 | time-series | multimodal，
  白名单校验 `modelrepo.ValidateModelMetadataExt`，无键兼容存量）；
  时序约定键 `input.features/input.window/input.rate`（宽松记录）；
  训练闭环留位键 `train.dataset-ref/train.job-id`（仅约定）。
- 场景关联：Metadata 约定键 `scene.bindings`（JSON 数组 [{kind,name]}）；
  `GET /api/v1/models/{modelName}/scenes` 结构化查询（未配置空数组；解析失败
  返回空 + Warn）。

### 2. 困难样本回传（G25）
- 边缘采集器 hardSampleCollector（opt-in `EDGEFLOW_EDGECORE_HARDSAMPLE=on`）：
  实现 alarm.Linkage 追加联动（alarmManager.AddLinkage，multiLinkage nil 安全）；
  告警新 episode 时从视频 mapper 环形缓冲捕获最新 JPEG 快照帧；
  低带宽抽样：每告警最多 1 张（`EDGEFLOW_EDGECORE_HARDSAMPLE_MAX_PER_ALARM`
  1–5 可调）；无帧源 → 跳过计数（不阻塞告警链）。
- 帧源装配：mediaSinkHolder.SetSnapSource 独立登记 VideoMapper 为
  LatestSnapshotSource（不被后续 sink=uploader 覆盖——e2e 发现的装配缺陷修复）。
- 上行：mediaup.EnqueueHardSample 专用入口（Kind=hard-sample，单帧单分片），
  复用 spool + 补传队列（断网留存/恢复重放/至少一次，零新队列）。
- 云端：mediastore kind 白名单扩容 + 对象前缀 `objects/hardsample/`（与流媒资
  隔离）+ AlarmID 元数据；AttachMedia 对 hard-sample 跳过流索引挂接；
  检索端点 `GET /api/v1/hardsamples`（nodeID/deviceName/alarmId 过滤，limit
  默认 50 上限 200，完成时间倒序）+ `GET /api/v1/hardsamples/{mediaId}/content`
  （JPEG 字节流；404 未知/409 未完成）。

### 3. 推理运行时扩展（G24）
- 加速卡探测上报：`EDGEFLOW_EDGECORE_ACCELS`（逗号分隔 <类型>:<标识>，
  如 "gpu:cuda-12.4,npu:rockchip-9996"；env 优先，真实设备枚举=可插拔探测器
  后续接入）→ RegisterPayload.Accels（可选字段，先例 Compression）→ 云端
  NodeInfo.Accels 快照 → `GET /api/v1/nodes/{nodeID}` 可查（旧边缘缺省 →
  字段省略，兼容）。
- 对接规范与指南：docs/INFERENCE-GUIDE.md（加速探测链路 / HTTP 推理契约沿用 /
  运行时镜像标签 runtime.http-inference 发现约定 / NPU 厂商 SDK 边界 / 验证清单）。

## N2 兼容与冻结
- **契约扩容**：72→75 端点（scenes 1 + hardsamples 2）；消息 16 维持
  （Register 可选字段 + MediaUpload kind 白名单扩容均为"只增不改"）；
  契约测试（源级扫描 + 运行时探测 + 文档一致性）全量同步。
- **默认零行为**：HARDSAMPLE off 不装配采集器；ACCELS 未设置不携带；
  modality 缺省 vision；scenes 未配置空数组；hard-sample 不挂流索引。
- 冻结带 v0240–v0350 测试零改动；v0.39–v0.44 测试零改动；go.mod 零变化。

## N3 测试（grep 口径）
- cloud/pkg/modelrepo +1（modality 白名单表驱动）。
- cloud/pkg/mediastore +2（hard-sample 落盘/前缀隔离/过滤列表/元数据；
  未知 kind 仍拒绝）。
- cmd/edgecore +6（accel env 解析/采集器开关与 max 回退/每告警 admit/无帧
  安全/空快照防御/多联动顺序与 nil 安全）。
- tests/e2e +1（TestV0450ModelSampleInferenceE2E：US-5 加速上报 + US-2 场景
  关联 + US-4 空态 + US-3 全链铁证——告警触发→采集→入队→补传→检索列表
  过滤→内容 sha256 一致）。
- 既有模型路由计数测试同步（modelAPI 28→29 端点；模型族 7→8）。
- 合计：新增 10 例（单测 9 + e2e 1）全绿。

## N4 边界（登记 KNOWN-ISSUES §46）
- 困难样本仅 JPEG 快照帧（无原始波形/视频段）；抽样计数在内存（重启清零，
  最多多传一张，无害）；
- 帧源弱关联：取最后注册视频 mapper 的最新帧，告警设备与帧流精确映射后续；
- 加速探测 env 注入 + 探测器骨架（真实 GPU/NPU 枚举=平台适配工作，指南给清单）；
- 训练闭环仅元数据键留位；多模态仅元数据白名单；发布白名单不校验加速匹配；
- scenes 查询是 bindings 键的结构化视图（非独立资源，无 CRUD）。

## N5 门禁（2026-10-03 全绿）
- [1] `go vet ./...`：VET OK（处置后复验）。
- [2] 全仓测试（除 e2e/契约）：pkg/mqttsim **TestV0270SimRestartRecovery 偶发
  FAIL 一次**（与本版无关的既有偶发——本版未触碰 mqttsim；单独复跑 1 次定向 +
  3 次全包全绿）。
- [3] `go test -race`（触碰 8 包：pkg/mediaup / cloud/pkg/mediastore /
  cloud/pkg/modelrepo / cloud/pkg/registry / cloud/pkg/cloudhub / cmd/cloudcore /
  cmd/edgecore / mappers/video）：全绿。
- [4] 契约 `go test ./tests/contract/`：13.484s 全绿（75 端点/16 消息）。
- [5] e2e 全量 `go test ./tests/e2e/ -timeout 25m`：**698.471s 全绿（exit=0）**——
  含 v0.39–v0.44 零回归 + 新增 TestV0450ModelSampleInferenceE2E
  （US-5 加速上报 + US-2 场景关联 + US-4 检索空态 + US-3 困难样本全链铁证
  40.52s；单项先跑见 e2e-v0450-run*.log）。
- 门禁日志：`.cluster/edgeflow-v0450/gates.log`（含 [2] 偶发诚实登记）+
  `e2e-final.log`。跑测前后 `lsof -i :12379,:12380` = 0（无 etcd 端口泄漏）。
- e2e 调试期间发现并修复：mediaSinkHolder 帧源被 uploader 覆盖
  （SetSnapSource 独立登记，见 N1-2）。
