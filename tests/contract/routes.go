// Package contract 定义 EdgeFlow API 兼容性契约（ROADMAP WBS 10.3
// 「无兼容矩阵文档/测试（audit-m35 G4）」的可执行测试侧）。
//
// 本文件是契约的唯一事实源（source of truth）：
//   - ContractEndpoints：cloudcore HTTP 端点契约（14 条），与
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

// ContractEndpoints 是 cloudcore HTTP 端点契约表（31 条 = 既有 14 + v0.7.0 模型 API 17）。
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
	{Method: "POST", Path: "/api/v1/models/{modelName}/releases/{releaseID}/cancel", Note: "取消发布（pending/running）"},
	{Method: "POST", Path: "/api/v1/models/{modelName}/releases/{releaseID}/rollback", Note: "回滚发布（202 异步，逆序批量）"},
	{Method: "GET", Path: "/api/v1/models/{modelName}/deployments", Note: "部署影子（版本—节点—时间台账）"},
}

// MessageType 描述一种云边通道消息类型契约。
type MessageType struct {
	Name  string // 消息类型字符串值（即 protocol.Type* 常量的值）
	InDoc bool   // 是否出现在 docs/API-COMPATIBILITY.md §2 消息矩阵
	Note  string // 方向与用途说明
}

// ContractMessageTypes 是云边通道消息类型契约，逐项绑定 protocol.Type* 常量。
//
// 与 docs/API-COMPATIBILITY.md §2 矩阵对应（10 种活跃类型）。任务书口径为
// 「9 种消息」，实际矩阵含 Ack 共 10 行——契约以文档为准（文档一致性测试
// 会双向校验，若任务口径与文档再出现分歧会在测试中显式暴露）。
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
	{Name: protocol.TypeAck, InDoc: true, Note: "双向：通用确认（可靠投递）"},
	{Name: protocol.TypeNodeJob, InDoc: false, Note: "云→边：任务分发（已关闭：v0.1.0 范围外，保留协议占位）"},
	{Name: protocol.TypeNodeJobResult, InDoc: false, Note: "边→云：任务结果（已关闭：v0.1.0 范围外，保留协议占位）"},
}
