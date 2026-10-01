// 上行补传可视化 API（v0.39.0，spec 0012 US-7）：2 个端点（契约扩容轮）。
//
//	GET /api/v1/uplink/overview        各节点上行状态（边侧上报 + 云端计数）
//	GET /api/v1/nodes/{nodeID}/uplink  单节点上行状态（无任何数据 → 404）
//
// 数据源：edgecore 周期上报的 UplinkReport（内存覆盖式缓存，含 lastReportTs）
// + CloudHub 接收计数（received/duplicated，去重后）；注册到 apiMux →
// auth/audit 链自动覆盖。
package main

import (
	"net/http"
	"sort"
	"sync"

	"edgeflow/cloud/pkg/cloudhub"
)

// uplinkNodeState 是单节点上行状态（合并边侧上报与云端计数）。
type uplinkNodeState struct {
	NodeID       string `json:"nodeId"`
	Depth        int    `json:"depth"`        // 边侧积压（最近上报）
	Dropped      int64  `json:"dropped"`      // 边侧累计丢弃
	Sent         int64  `json:"sent"`         // 边侧累计上送
	OldestTs     int64  `json:"oldestTs"`     // 最老积压条目入队时间（毫秒）
	LastReportTs int64  `json:"lastReportTs"` // 最近上报时间（毫秒）
	Received     int64  `json:"received"`     // 云端接收（去重后）
	Duplicated   int64  `json:"duplicated"`   // 云端重复丢弃
}

// uplinkReportSnapshot 是单节点最近一次上报的快照。
type uplinkReportSnapshot struct {
	Depth    int
	Dropped  int64
	Sent     int64
	OldestTs int64
	LastTs   int64
}

// uplinkState 缓存边侧上报（覆盖式，最近一次生效）。
type uplinkState struct {
	mu      sync.Mutex
	reports map[string]uplinkReportSnapshot
}

// newUplinkState 构造上报缓存。
func newUplinkState() *uplinkState {
	return &uplinkState{reports: make(map[string]uplinkReportSnapshot)}
}

// update 覆盖式记录一次上报。
func (u *uplinkState) update(nodeID string, p cloudhub.UplinkReportPayload, nowMs int64) {
	u.mu.Lock()
	u.reports[nodeID] = uplinkReportSnapshot{
		Depth: p.Depth, Dropped: p.Dropped, Sent: p.Sent, OldestTs: p.OldestTs, LastTs: nowMs,
	}
	u.mu.Unlock()
}

// snapshot 返回上报缓存副本。
func (u *uplinkState) snapshot() map[string]uplinkReportSnapshot {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make(map[string]uplinkReportSnapshot, len(u.reports))
	for k, v := range u.reports {
		out[k] = v
	}
	return out
}

// uplinkAPI 是上行可视化端点处理器集合（依赖注入，模式与 ruleAPI 一致）。
type uplinkAPI struct {
	hub   *cloudhub.Server
	state *uplinkState
}

// Register 注册 2 条上行路由到 apiMux。
func (a *uplinkAPI) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/uplink/overview", a.overview)
	mux.HandleFunc("GET /api/v1/nodes/{nodeID}/uplink", a.nodeUplink)
}

// merge 合并两路数据源：节点集合 = 上报 ∪ 接收计数。
func (a *uplinkAPI) merge() map[string]uplinkNodeState {
	out := make(map[string]uplinkNodeState)
	if a.hub != nil {
		for node, c := range a.hub.RuleEventStats() {
			st := out[node]
			st.NodeID = node
			st.Received = c.Received
			st.Duplicated = c.Duplicated
			out[node] = st
		}
	}
	if a.state != nil {
		for node, r := range a.state.snapshot() {
			st := out[node]
			st.NodeID = node
			st.Depth = r.Depth
			st.Dropped = r.Dropped
			st.Sent = r.Sent
			st.OldestTs = r.OldestTs
			st.LastReportTs = r.LastTs
			out[node] = st
		}
	}
	return out
}

// overview 处理 GET /api/v1/uplink/overview：各节点上行状态列表（按 nodeId 排序）。
func (a *uplinkAPI) overview(w http.ResponseWriter, r *http.Request) {
	m := a.merge()
	list := make([]uplinkNodeState, 0, len(m))
	for _, v := range m {
		list = append(list, v)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].NodeID < list[j].NodeID })
	writeJSON(w, http.StatusOK, map[string]any{"nodes": list})
}

// nodeUplink 处理 GET /api/v1/nodes/{nodeID}/uplink：单节点状态；
// 无任何数据（未上报且无接收计数）→ 404。
func (a *uplinkAPI) nodeUplink(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("nodeID")
	st, ok := a.merge()[nodeID]
	if !ok {
		writeErr(w, http.StatusNotFound, "节点无上行数据: "+nodeID, nil)
		return
	}
	writeJSON(w, http.StatusOK, st)
}
