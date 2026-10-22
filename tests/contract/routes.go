// Package contract 定义 EdgeFlow API 兼容性契约（ROADMAP WBS 10.3
// 「无兼容矩阵文档/测试（audit-m35 G4）」的可执行测试侧）。
//
// 本文件是契约的唯一事实源（source of truth）：
//   - ContractEndpoints：cloudcore HTTP 端点契约（134 条），与
//     cmd/cloudcore/main.go 的路由注册、docs/API-SPEC.md §1.1 端点总览、
//     docs/API-COMPATIBILITY.md §1 REST API 端点矩阵逐行对应；
//   - ContractMessageTypes：云边通道消息类型契约，直接引用
//     edgeflow/pkg/protocol 的 Type* 常量（常量被删除/改名 → 本文件编译失败，
//     契约违约在 CI 即刻暴露），与 docs/API-COMPATIBILITY.md §2 消息矩阵对应。
//
// 配套测试见 api_contract_test.go：运行时路由探测（真实 cloudcore 子进程）、
// 运行时反向探测（保留前缀路径断言 404，无契约外路由的运行时补充断言）、
// 源码反向断言（无契约外路由）、文档一致性比对。
package contract

import "edgeflow/pkg/protocol"

// Endpoint 描述一条受版本约束的 HTTP 端点契约。
type Endpoint struct {
	Method string // HTTP 方法：GET / POST
	Path   string // 路径模式；{nodeID} 为路径参数占位
	Note   string // 契约说明（与文档矩阵口径一致）
}

// ContractEndpoints 是 cloudcore HTTP 端点契约表（97 条 = 既有 14 + v0.7.0 模型 API 17 + v0.12.0 digest 复核 1 + v0.16.0 pause/resume/export/import 4 + v0.17.0 PATCH 可调参数 1 + v0.18.0 全局部署影子查询 1 + v0.19.0 发布审计快照/全局发布查询 2 + v0.20.0 retry/终态归档删除 2 + v0.37.0 规则管理 API 9 + v0.39.0 上行补传可视化 2 + v0.40.0 告警中心 5 + 设定值通道 3 + v0.43.0 视频管理面 8 + v0.44.0 流媒体分发与告警片段 3 + v0.45.0 模型面扩展与困难样本 3 + v0.46.0 RBAC 与镜像凭证 7 + v0.47.0 应用目录与分发 15 + v0.48.0 日志链路 5 + v0.49.0 远程运维终端 5 + v0.50.0 区域层与标签治理 7 + v0.51.0 区域协同层 5 + v0.53.0 数据转发出口 6 + v0.56.0 审计台账查询 2 + v0.58.0 告警静默窗口 3 + v0.59.0 转发出口测试投递 1）。
//
// ⚠️ 路径以 cmd/cloudcore/main.go 实际注册为准（任务提示 podsync/pod-sync
// 存在歧义：grep 确认代码与两份文档均为 /podsync，无连字符）。
// ⚠️ /ocsp 是 OCSP 协议端点（RFC 6960，WBS 7.1）：请求/响应为 DER 编码，
// 不挂 API Token 认证（响应自带 CA 签名），空 body 返回 400。
var ContractEndpoints = []Endpoint{
	{Method: "GET", Path: "/healthz", Note: "健康检查（探针）"},
	{Method: "GET", Path: "/metrics", Note: "Prometheus 指标（WBS 10.1）"},
	{Method: "GET", Path: "/api/v1/nodes", Note: "全部节点（运行视角 NodeInfo 列表）"},
	{Method: "GET", Path: "/api/v1/nodes/{nodeID}", Note: "单节点详情"},
	{Method: "GET", Path: "/api/v1/edgenodes", Note: "全部节点（CRD 对象视角，K8s List 风格）"},
	{Method: "GET", Path: "/api/v1/edgenodes/{nodeID}", Note: "单节点 EdgeNode 对象"},
	{Method: "GET", Path: "/api/v1/pods", Note: "全部节点 Pod 状态"},
	{Method: "GET", Path: "/api/v1/nodes/{nodeID}/pods", Note: "单节点 Pod 状态"},
	{Method: "GET", Path: "/api/v1/devices", Note: "全部设备状态"},
	{Method: "GET", Path: "/api/v1/nodes/{nodeID}/devices", Note: "单节点设备状态"},
	{Method: "POST", Path: "/api/v1/nodes/{nodeID}/podsync", Note: "可靠下发 Pod 配置（add/update/delete）"},
	{Method: "POST", Path: "/api/v1/nodes/{nodeID}/config-sync", Note: "可靠下发 ConfigMap/Secret 配置"},
	{Method: "POST", Path: "/api/v1/nodes/{nodeID}/device-command", Note: "下发设备指令（期望值）"},
	{Method: "POST", Path: "/ocsp", Note: "OCSP 在线吊销查询（RFC 6960，DER 请求/响应）"},
	// ── v0.7.0 模型仓库/版本管理/灰度发布（17 条，设计定稿 §4.1 表）──
	{Method: "GET", Path: "/api/v1/models", Note: "模型列表（K8s List 风格）"},
	{Method: "POST", Path: "/api/v1/models", Note: "创建模型"},
	{Method: "GET", Path: "/api/v1/models/{modelName}", Note: "模型详情"},
	{Method: "PUT", Path: "/api/v1/models/{modelName}", Note: "更新模型"},
	{Method: "DELETE", Path: "/api/v1/models/{modelName}", Note: "删除模型（无 active 版本/在途发布；级联）"},
	{Method: "GET", Path: "/api/v1/models/{modelName}/versions", Note: "版本列表"},
	{Method: "POST", Path: "/api/v1/models/{modelName}/versions", Note: "创建版本（初始 draft）"},
	{Method: "GET", Path: "/api/v1/models/{modelName}/versions/{version}", Note: "版本详情"},
	{Method: "DELETE", Path: "/api/v1/models/{modelName}/versions/{version}", Note: "删除版本（仅 draft/archived）"},
	{Method: "POST", Path: "/api/v1/models/{modelName}/versions/{version}/activate", Note: "激活版本（draft→active）"},
	{Method: "POST", Path: "/api/v1/models/{modelName}/versions/{version}/archive", Note: "归档版本（active→archived）"},
	{Method: "POST", Path: "/api/v1/models/{modelName}/releases", Note: "创建灰度发布（202 异步）"},
	{Method: "GET", Path: "/api/v1/models/{modelName}/releases", Note: "发布列表"},
	{Method: "GET", Path: "/api/v1/models/{modelName}/releases/{releaseID}", Note: "发布详情（含 perNode 汇总）"},
	{Method: "GET", Path: "/api/v1/models/{modelName}/releases/{releaseID}/digest", Note: "发布 digest 复核（v0.12.0，D-1）"},
	{Method: "POST", Path: "/api/v1/models/{modelName}/releases/{releaseID}/pause", Note: "暂停发布（v0.16.0；running→paused，节点边界生效）"},
	{Method: "POST", Path: "/api/v1/models/{modelName}/releases/{releaseID}/resume", Note: "恢复发布（v0.16.0；paused→running，NextBatchAt 保持原节奏）"},
	{Method: "GET", Path: "/api/v1/models/export", Note: "模型目录导出（v0.16.0；models+versions 全量快照 JSON）"},
	{Method: "POST", Path: "/api/v1/models/import", Note: "模型目录导入（v0.16.0；幂等 upsert，active 直通灾备语义）"},
	{Method: "PATCH", Path: "/api/v1/models/{modelName}/releases/{releaseID}", Note: "发布运行中可调参数（v0.17.0；batchSize/pauseBetween/failFast 部分更新批边界生效；v0.19.0 起白名单扩展 failureBudget=0 即关闸）"},
	{Method: "GET", Path: "/api/v1/models/{modelName}/releases/{releaseID}/snapshot", Note: "发布审计快照（v0.19.0；头含 events+逐节点结果+summary 六计数+generatedAt 只读全景）"},
	{Method: "GET", Path: "/api/v1/deployments", Note: "全局部署影子查询（v0.18.0；跨模型聚合，model/nodeID 过滤可选）"},
	{Method: "GET", Path: "/api/v1/releases", Note: "全局发布查询（v0.19.0；跨模型聚合 status 多值过滤 limit≤500 X-Total-Count CreatedAt 降序 tie-break by ID）"},
	{Method: "POST", Path: "/api/v1/models/{modelName}/releases/{releaseID}/retry", Note: "失败节点重试（v0.20.0；仅终态可重试，克隆新发布 RetryOf 回指原发布，nodeIDs 可选=failed 子集）"},
	{Method: "DELETE", Path: "/api/v1/models/{modelName}/releases/{releaseID}", Note: "终态发布归档删除（v0.20.0；手动点删单条终态，非终态 409 与 GC 同源在途绝不删）"},
	{Method: "POST", Path: "/api/v1/models/{modelName}/releases/{releaseID}/cancel", Note: "取消发布（v0.16.0 起 pending/running/paused）"},
	{Method: "POST", Path: "/api/v1/models/{modelName}/releases/{releaseID}/rollback", Note: "回滚发布（202 异步，逆序批量）"},
	{Method: "GET", Path: "/api/v1/models/{modelName}/deployments", Note: "部署影子（版本—节点—时间台账）"},
	// ── v0.37.0 规则管理 API（9 条：规则 CRUD 5 + 治理策略 2 + 下发 1 + 事件查询 1）──
	{Method: "POST", Path: "/api/v1/rules", Note: "创建规则（v0.37.0；201，重复 ruleId 409）"},
	{Method: "GET", Path: "/api/v1/rules", Note: "规则列表（v0.37.0，K8s List 风格按 ruleId 排序）"},
	{Method: "GET", Path: "/api/v1/rules/{ruleID}", Note: "规则详情（v0.37.0，不存在 404）"},
	{Method: "PUT", Path: "/api/v1/rules/{ruleID}", Note: "更新规则（v0.37.0，不存在 404）"},
	{Method: "DELETE", Path: "/api/v1/rules/{ruleID}", Note: "删除规则（v0.37.0，不存在 404）"},
	{Method: "GET", Path: "/api/v1/rules/events", Note: "规则事件查询（v0.37.0；ruleId/device/limit 过滤，倒序）"},
	{Method: "GET", Path: "/api/v1/rules/governance", Note: "治理策略列表（v0.37.0，含规则包版本）"},
	{Method: "PUT", Path: "/api/v1/rules/governance", Note: "治理策略全量替换（v0.37.0）"},
	{Method: "POST", Path: "/api/v1/nodes/{nodeID}/rules/sync", Note: "规则包下发（v0.37.0；可靠投递，五态语义同 config-sync）"},
	// ── v0.39.0 上行补传可视化 API（2 条）──
	{Method: "GET", Path: "/api/v1/uplink/overview", Note: "上行补传概览（v0.39.0；各节点积压/丢弃/上送/接收计数）"},
	{Method: "GET", Path: "/api/v1/nodes/{nodeID}/uplink", Note: "单节点上行状态（v0.39.0；无数据 404）"},
	// ── v0.40.0 统一告警中心（5 条）──
	{Method: "GET", Path: "/api/v1/alarms", Note: "告警列表（v0.40.0；nodeID/state/severity/limit 过滤）"},
	{Method: "GET", Path: "/api/v1/alarms/stats", Note: "告警统计（v0.40.0；byState/bySeverity/total）"},
	{Method: "POST", Path: "/api/v1/alarms/{alarmID}/ack", Note: "告警确认（v0.40.0；raised → acked，operator 必填）"},
	{Method: "POST", Path: "/api/v1/alarms/{alarmID}/assign", Note: "告警派单（v0.40.0；工单集成点回调）"},
	{Method: "POST", Path: "/api/v1/alarms/{alarmID}/close", Note: "告警闭环（v0.40.0；任意非 closed → closed 终态）"},
	// ── v0.43.0 视频管理面（8 条）──
	{Method: "GET", Path: "/api/v1/videostreams", Note: "视频流列表（nodeID 过滤）"},
	{Method: "POST", Path: "/api/v1/videostreams", Note: "创建视频流（name/nodeId/deviceName 必填）"},
	{Method: "GET", Path: "/api/v1/videostreams/{name}", Note: "视频流详情（含片段索引）"},
	{Method: "PUT", Path: "/api/v1/videostreams/{name}", Note: "更新视频流（deviceName/sourceType/status/description）"},
	{Method: "DELETE", Path: "/api/v1/videostreams/{name}", Note: "删除视频流索引（不删媒资文件）"},
	{Method: "GET", Path: "/api/v1/videostreams/{name}/snapshot", Note: "最新快照（image/jpeg 字节）"},
	{Method: "GET", Path: "/api/v1/videostreams/{name}/segments", Note: "片段索引列表"},
	{Method: "GET", Path: "/api/v1/videostreams/{name}/segments/{mediaID}", Note: "片段回放（video/x-mjpeg 字节）"},
	// ── v0.44.0 流媒体分发 + 告警片段检索（3 条）──
	{Method: "GET", Path: "/media/streams/{name}/live.flv", Note: "HTTP-FLV 拉流（H.264 透传封装，chunked video/x-flv；无注册帧源 404）"},
	{Method: "GET", Path: "/media/streams/{name}/live.ws", Note: "WS-FLV 拉流（WS 二进制透传 FLV 字节流）"},
	{Method: "GET", Path: "/api/v1/alarms/{alarmID}/segments", Note: "告警关联片段检索（DeviceName+RaisedAt 窗 × videostream 索引）"},
	// ── v0.45.0 模型面扩展 + 困难样本（3 条）──
	{Method: "GET", Path: "/api/v1/models/{modelName}/scenes", Note: "模型-业务场景关联查询（scene.bindings 结构化视图；未配置空数组）"},
	{Method: "GET", Path: "/api/v1/hardsamples", Note: "困难样本列表（nodeID/deviceName/alarmId 过滤，limit 默认 50 上限 200）"},
	{Method: "GET", Path: "/api/v1/hardsamples/{mediaId}/content", Note: "困难样本内容（JPEG 字节流；404 未知/409 未完成）"},
	{Method: "GET", Path: "/api/v1/roles", Note: "角色枚举与权限矩阵（v0.46.0；admin-only）"},
	{Method: "GET", Path: "/api/v1/users", Note: "API 凭证列表（脱敏；v0.46.0；admin-only）"},
	{Method: "POST", Path: "/api/v1/users", Note: "创建 API 凭证（明文 token 一次性返回；v0.46.0；admin-only）"},
	{Method: "DELETE", Path: "/api/v1/users/{id}", Note: "撤销 API 凭证（即时生效；v0.46.0；admin-only）"},
	{Method: "GET", Path: "/api/v1/image-auths", Note: "镜像仓库凭证列表（脱敏；v0.46.0；admin-only）"},
	{Method: "PUT", Path: "/api/v1/image-auths/{registry}", Note: "设置/覆盖镜像仓库凭证（v0.46.0；admin-only）"},
	{Method: "DELETE", Path: "/api/v1/image-auths/{registry}", Note: "删除镜像仓库凭证（v0.46.0；admin-only）"},
	{Method: "POST", Path: "/api/v1/applications", Note: "注册应用（v0.47.0；name 唯一 409）"},
	{Method: "GET", Path: "/api/v1/applications", Note: "应用列表（分页 limit/offset + X-Total-Count）"},
	{Method: "GET", Path: "/api/v1/applications/{name}", Note: "应用详情（含版本目录）"},
	{Method: "DELETE", Path: "/api/v1/applications/{name}", Note: "删除应用（有进行中部署 409）"},
	{Method: "POST", Path: "/api/v1/applications/{name}/versions", Note: "登记版本（kind 决定 image/scriptRef 必填）"},
	{Method: "GET", Path: "/api/v1/applications/{name}/versions", Note: "版本列表"},
	{Method: "PUT", Path: "/api/v1/applications/{name}/versions/{version}/package", Note: "上传应用包（≤32MB；sha256 回显）"},
	{Method: "GET", Path: "/api/v1/applications/{name}/versions/{version}/package", Note: "下载应用包（X-Checksum-Sha256）"},
	{Method: "POST", Path: "/api/v1/app-deployments", Note: "创建应用灰度部署计划（分批/停顿）"},
	{Method: "GET", Path: "/api/v1/app-deployments", Note: "部署计划列表"},
	{Method: "GET", Path: "/api/v1/app-deployments/{planID}", Note: "部署计划详情（perNode/批次进度）"},
	{Method: "POST", Path: "/api/v1/app-deployments/{planID}/pause", Note: "暂停部署（running → paused）"},
	{Method: "POST", Path: "/api/v1/app-deployments/{planID}/resume", Note: "恢复部署（paused → running）"},
	{Method: "POST", Path: "/api/v1/app-deployments/{planID}/cancel", Note: "取消部署（逆序卸载已交付）"},
	{Method: "POST", Path: "/api/v1/app-deployments/{planID}/rollback", Note: "回滚部署（恢复目标版本）"},
	// ── v0.40.0 设定值通道（3 条）──
	{Method: "POST", Path: "/api/v1/nodes/{nodeID}/setpoints", Note: "设定值建单（v0.40.0；审批开关/单笔 requireApproval）"},
	{Method: "POST", Path: "/api/v1/setpoints/{setpointID}/approval", Note: "设定值审批（v0.40.0；approve|reject，仅 pending-approval 可审）"},
	{Method: "GET", Path: "/api/v1/setpoints", Note: "设定值列表（v0.40.0；含执行反馈 outcome/error）"},
	// ── v0.48.0 日志链路（5 条，spec 0021 US-3）──
	{Method: "GET", Path: "/api/v1/logs", Note: "日志检索（node/source/level/q/since/until/limit/offset，node 缺省跨节点；v0.48.0）"},
	{Method: "GET", Path: "/api/v1/logs/nodes", Note: "有日志节点清单（含条数与最新时间；v0.48.0）"},
	{Method: "GET", Path: "/api/v1/logs/nodes/{nodeID}", Note: "节点日志概要（sources/levels 计数；v0.48.0）"},
	{Method: "GET", Path: "/api/v1/logs/nodes/{nodeID}/tail", Note: "节点日志尾部 N 条（默认 100 上限 1000；v0.48.0）"},
	{Method: "DELETE", Path: "/api/v1/logs/nodes/{nodeID}", Note: "清空节点日志（轮转/清理入口；v0.48.0）"},
	{Method: "POST", Path: "/api/v1/term/sessions", Note: "创建受控运维会话（nodeId+reason 必填；write=operator+；v0.49.0）"},
	{Method: "GET", Path: "/api/v1/term/sessions", Note: "会话列表（?nodeID= 过滤；v0.49.0）"},
	{Method: "POST", Path: "/api/v1/term/sessions/{id}/exec", Note: "执行白名单命令（同步等 TermResult；越权 403/离线 503；v0.49.0）"},
	{Method: "POST", Path: "/api/v1/term/sessions/{id}/close", Note: "关闭会话（幂等；v0.49.0）"},
	{Method: "GET", Path: "/api/v1/term/audit", Note: "命令级审计查询（X-Total-Count；v0.49.0）"},
	// ── v0.50.0 区域层与标签治理（spec 0023，G03+G04；纯云端管理面）──
	{Method: "POST", Path: "/api/v1/regions", Note: "创建区域（单父树；write=operator+；v0.50.0）"},
	{Method: "GET", Path: "/api/v1/regions", Note: "区域树（含 nodeCount；v0.50.0）"},
	{Method: "PUT", Path: "/api/v1/regions/{id}", Note: "更新区域（改名/描述/移动防环 409；v0.50.0）"},
	{Method: "DELETE", Path: "/api/v1/regions/{id}", Note: "删除区域（空区域约束 409；delete=admin 对齐 v0.46 语义；v0.50.0）"},
	{Method: "POST", Path: "/api/v1/regions/{id}/nodes", Note: "节点批量归属（幂等换区；批量 ≤64；v0.50.0）"},
	{Method: "POST", Path: "/api/v1/tags", Note: "打标/删标（node/device/model 三类锚点；{k:v} upsert {k:null} 删；v0.50.0）"},
	{Method: "GET", Path: "/api/v1/governance/nodes", Note: "区域+标签组合查询（region/tag/tagKey AND 语义；v0.50.0）"},
	// ── v0.51.0 区域协同层（spec 0024，G01；纯云端管理面，消息 19 不变）──
	{Method: "GET", Path: "/api/v1/hierarchy/summary", Note: "三级树形汇总（递归聚合；read=viewer+；v0.51.0）"},
	{Method: "GET", Path: "/api/v1/hierarchy/regions/{id}/nodes", Note: "区域节点明细（含子树；read=viewer+；v0.51.0）"},
	{Method: "GET", Path: "/api/v1/hierarchy/alarms", Note: "区域×状态告警计数（read=viewer+；v0.51.0）"},
	{Method: "GET", Path: "/api/v1/hierarchy/regions/{id}/policy", Note: "区域策略读取（read=viewer+；v0.51.0）"},
	{Method: "PUT", Path: "/api/v1/hierarchy/regions/{id}/policy", Note: "区域策略 upsert/清除（write=operator+；无 DELETE 面；v0.51.0）"},
	// ── v0.52.0 灾备备份管理面（spec 0025；DR-ASSESSMENT 差距 #5 承接）──
	{Method: "POST", Path: "/api/v1/dr/backups", Note: "创建治理面快照备份（DR_DIR 未启用 503；write=operator+；v0.52.0）"},
	{Method: "GET", Path: "/api/v1/dr/backups", Note: "备份清单（{enabled,backups}；read=viewer+；v0.52.0）"},
	{Method: "GET", Path: "/api/v1/dr/backups/{id}", Note: "备份详情（?verify=true 触发 sha256 复核；read=viewer+；v0.52.0）"},
	// ── v0.53.0 数据转发出口（spec 0026，G18 起点）──
	{Method: "GET", Path: "/api/v1/forwards", Note: "转发规则清单（脱敏视图；read=viewer+；v0.53.0）"},
	{Method: "POST", Path: "/api/v1/forwards", Note: "创建转发规则（write=operator+；v0.53.0）"},
	{Method: "GET", Path: "/api/v1/forwards/{id}", Note: "转发规则详情（read=viewer+；v0.53.0）"},
	{Method: "PUT", Path: "/api/v1/forwards/{id}", Note: "部分更新规则（write=operator+；v0.53.0）"},
	{Method: "DELETE", Path: "/api/v1/forwards/{id}", Note: "删除规则（delete=admin 对齐 v0.46 语义；v0.53.0）"},
	{Method: "GET", Path: "/api/v1/forwards/{id}/deliveries", Note: "送达台账（新→旧；read=viewer+；v0.53.0）"},
	// ── v0.56.0 审计台账查询面（spec 0029；只读，记录面 ledger.Middleware 既有）──
	{Method: "GET", Path: "/api/v1/audit/entries", Note: "台账查询（operator/method/pathSub/status/from/to/limit/offset，Ts 降序；read=viewer+；v0.56.0）"},
	{Method: "GET", Path: "/api/v1/audit/stats", Note: "台账聚合（总数/按操作者/按动作/状态桶，同过滤语义；read=viewer+；v0.56.0）"},
	// ── v0.58.0 告警静默窗口（spec 0031；mutestore JSON 快照）──
	{Method: "GET", Path: "/api/v1/alerts/mutes", Note: "静默窗口清单（含 active 实时标记；read=viewer+；v0.58.0）"},
	{Method: "POST", Path: "/api/v1/alerts/mutes", Note: "创建静默窗口（durationSec 1..2592000；write=operator+；v0.58.0）"},
	{Method: "DELETE", Path: "/api/v1/alerts/mutes/{id}", Note: "删除窗口（delete=admin 对齐 v0.46 语义；v0.58.0）"},
	// ── v0.59.0 转发出口增强（spec 0032；sinkType webhook|mqtt + 测试投递）──
	{Method: "POST", Path: "/api/v1/forwards/{id}/test", Note: "同步测试投递（绕过事件过滤；write=operator+；v0.59.0）"},
}

// MessageType 描述一种云边通道消息类型契约。
type MessageType struct {
	Name  string // 消息类型字符串值（即 protocol.Type* 常量的值）
	InDoc bool   // 是否出现在 docs/API-COMPATIBILITY.md §2 消息矩阵
	Note  string // 方向与用途说明
}

// ContractMessageTypes 是云边通道消息类型契约，逐项绑定 protocol.Type* 常量。
//
// 与 docs/API-COMPATIBILITY.md §2 矩阵对应（16 种活跃类型，v0.43.0 起
// +MediaUpload）。历史口径「9 种消息」为 M3 时点，契约以文档为准
// （文档一致性测试会双向校验，口径分歧在测试中显式暴露）。
// NodeJob / NodeJobResult 为已关闭占位（v0.1.0 范围外，见 pkg/protocol/message.go），
// 文档矩阵不收录，用 InDoc=false 显式声明该状态，防止误加回文档。
var ContractMessageTypes = []MessageType{
	{Name: protocol.TypeRegister, InDoc: true, Note: "边→云：节点注册（连接建立后第一条）"},
	{Name: protocol.TypeRegisterAck, InDoc: true, Note: "云→边：注册结果"},
	{Name: protocol.TypeHeartbeat, InDoc: true, Note: "边→云：心跳保活"},
	{Name: protocol.TypeHeartbeatAck, InDoc: true, Note: "云→边：心跳应答"},
	{Name: protocol.TypePodSync, InDoc: true, Note: "云→边：Pod 配置下发（M1/M2）"},
	{Name: protocol.TypeConfigSync, InDoc: true, Note: "云→边：ConfigMap/Secret 下发（M2）"},
	{Name: protocol.TypePodStatus, InDoc: true, Note: "边→云：Pod 状态上报（M1/M2）"},
	{Name: protocol.TypeDeviceReport, InDoc: true, Note: "边→云：设备数据/状态上报（M3）"},
	{Name: protocol.TypeDeviceCommand, InDoc: true, Note: "云→边：设备操作指令（M3）"},
	{Name: protocol.TypeRuleSync, InDoc: true, Note: "云→边：规则包全量下发（v0.37.0）"},
	{Name: protocol.TypeRuleEvent, InDoc: true, Note: "边→云：规则触发事件（v0.37.0）"},
	{Name: protocol.TypeUplinkReport, InDoc: true, Note: "边→云：上行补传队列状态上报（v0.39.0）"},
	{Name: protocol.TypeAlarmEvent, InDoc: true, Note: "边→云：告警事件（v0.40.0）"},
	{Name: protocol.TypeSetpointResult, InDoc: true, Note: "边→云：设定值执行反馈（v0.40.0）"},
	{Name: protocol.TypeMediaUpload, InDoc: true, Note: "边→云：媒资分片上传（快照/片段，v0.43.0）"},
	{Name: protocol.TypeLogSync, InDoc: true, Note: "边→云：批量日志同步（v0.48.0）"},
	{Name: protocol.TypeTermExec, InDoc: true, Note: "云→边：受控命令执行请求（白名单边侧强制；v0.49.0）"},
	{Name: protocol.TypeTermResult, InDoc: true, Note: "边→云：执行结果回传（输出 ≤64KB 截断；v0.49.0）"},
	{Name: protocol.TypeAck, InDoc: true, Note: "双向：通用确认（可靠投递）"},
	{Name: protocol.TypeNodeJob, InDoc: false, Note: "云→边：任务分发（已关闭：v0.1.0 范围外，保留协议占位）"},
	{Name: protocol.TypeNodeJobResult, InDoc: false, Note: "边→云：任务结果（已关闭：v0.1.0 范围外，保留协议占位）"},
}
