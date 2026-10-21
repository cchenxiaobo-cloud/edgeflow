// Package hierarchyview 提供区域协同层的只读聚合视图（v0.51.0，spec 0024，G01）。
//
// 三级语义：中心 = 本 cloudcore；区域 = regionstore 树；节点 = registry。
// 聚合口径：
//   - 在线判定：registry NodeInfo.Status 为唯一权威（Ready/Unknown=在线侧，
//     Offline=离线）——本包不自建心跳窗口。
//   - 递归聚合：子区域计数并入父（自底向上）；单父无环由 regionstore 保证
//     （创建/移动防环 + Load 断链）。
//   - 告警计数：alarmstore List 全量拉取（limit 2000）后按节点归属区域聚——
//     超 limit 的偏差登记 KI；活跃 = 非 closed。
//   - 性能口径：实时聚合（假设 ≤ 数百节点/≤2000 告警）；缓存化留 v0.52 规模版。
//
// 并发模型：本包无自身状态（纯函数式聚合），各数据源自带锁；单次调用内
// 多次读非同一快照（计数微偏差可接受，不引入全局快照锁）。
package hierarchyview

import (
	"sort"

	"edgeflow/cloud/pkg/regionstore"
	"edgeflow/cloud/pkg/registry"
	"edgeflow/pkg/alarm"
)

// NodeSource 是节点元数据源（registry 抽象，测试可替身）。
type NodeSource interface {
	List() []registry.NodeInfo
}

// AlarmSource 是告警数据源（alarmstore 抽象，测试可替身）。
type AlarmSource interface {
	List(nodeID, state, severity string, limit int) []alarm.Alarm
}

// View 是层级聚合视图（无状态，依赖注入）。
type View struct {
	regions *regionstore.Store
	nodes   NodeSource
	alarms  AlarmSource
}

// New 构建层级视图。
func New(regions *regionstore.Store, nodes NodeSource, alarms AlarmSource) *View {
	return &View{regions: regions, nodes: nodes, alarms: alarms}
}

// RegionSummary 是区域汇总节点（递归聚合；UnassignedCount 仅根级非零）。
type RegionSummary struct {
	ID               string           `json:"id"`
	Name             string           `json:"name"`
	ParentID         string           `json:"parentId"`
	PolicySet        bool             `json:"policySet"`
	NodeCount        int              `json:"nodeCount"`
	OnlineCount      int              `json:"onlineCount"`
	AlarmActiveCount int              `json:"alarmActiveCount"`
	UnassignedCount  int              `json:"unassignedCount,omitempty"`
	Children         []*RegionSummary `json:"children,omitempty"`
}

// onlineSide 判定在线侧（裁决 5：Ready/Unknown=在线侧，Offline=离线）。
func onlineSide(s registry.NodeStatus) bool { return s != registry.StatusOffline }

// Summary 构建全树形汇总（兄弟按 ID 序；根级含 unassignedCount）。
func (v *View) Summary() *RegionSummary {
	byID := map[string]*RegionSummary{}
	var roots []*RegionSummary
	for _, r := range v.regions.All() {
		s := &RegionSummary{ID: r.ID, Name: r.Name, ParentID: r.ParentID, PolicySet: len(r.Policy) > 0}
		byID[r.ID] = s
		if r.ParentID == "" {
			roots = append(roots, s)
		}
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].ID < roots[j].ID })
	// 挂 children（ParentID 不存在视为根——regionstore 已防御，双保险）
	for _, s := range byID {
		if s.ParentID != "" {
			if p, ok := byID[s.ParentID]; ok {
				p.Children = append(p.Children, s)
			} else {
				roots = append(roots, s)
			}
		}
	}
	for id := range byID {
		sort.Slice(byID[id].Children, func(i, j int) bool { return byID[id].Children[i].ID < byID[id].Children[j].ID })
	}

	// own 计数：节点按归属聚（Assignments 一次快照）
	assign := v.regions.Assignments()
	unassigned := 0
	for _, n := range v.nodes.List() {
		rid, ok := assign[n.NodeID]
		if !ok {
			unassigned++
			continue
		}
		if s, ok := byID[rid]; ok {
			s.NodeCount++
			if onlineSide(n.Status) {
				s.OnlineCount++
			}
		} else {
			unassigned++ // 归属指向已删区域（防御，regionstore 理论上已清理）
		}
	}

	// own 告警计数（活跃=非 closed）
	for _, a := range v.alarms.List("", "", "", alarmScanLimit) {
		if a.NodeID == "" {
			continue
		}
		rid, ok := assign[a.NodeID]
		if !ok {
			continue
		}
		if s, ok := byID[rid]; ok && a.State != alarm.StateClosed {
			s.AlarmActiveCount++
		}
	}

	// 后序递归：子计数并入父
	var rollup func(s *RegionSummary)
	rollup = func(s *RegionSummary) {
		for _, c := range s.Children {
			rollup(c)
			s.NodeCount += c.NodeCount
			s.OnlineCount += c.OnlineCount
			s.AlarmActiveCount += c.AlarmActiveCount
		}
	}
	for _, r := range roots {
		rollup(r)
	}
	// 根汇总节点（虚拟根）：children=根区域列表，聚合全量
	top := &RegionSummary{ID: "", Name: "center", ParentID: "", Children: roots}
	for _, r := range roots {
		top.NodeCount += r.NodeCount
		top.OnlineCount += r.OnlineCount
		top.AlarmActiveCount += r.AlarmActiveCount
	}
	top.UnassignedCount = unassigned
	return top
}

// NodeRow 是区域节点明细行。
type NodeRow struct {
	NodeID   string `json:"nodeId"`
	NodeName string `json:"nodeName"`
	Region   string `json:"region"`
	Online   bool   `json:"online"`
	IP       string `json:"ip,omitempty"`
}

// RegionNodes 返回区域（含子树递归）节点明细；未知区域 ok=false。
// 子树判定：沿每区域父链逐层上溯直至命中起始集合（量级数百，简单正确优先）。
func (v *View) RegionNodes(id string) ([]NodeRow, bool) {
	if _, ok := v.regions.RegionByID(id); !ok {
		return nil, false
	}
	inSubtree := map[string]bool{id: true}
	for _, r := range v.regions.All() {
		cur := r.ID
		for cur != "" {
			if inSubtree[cur] {
				inSubtree[r.ID] = true
				break
			}
			pr, ok := v.regions.RegionByID(cur)
			if !ok {
				break
			}
			cur = pr.ParentID
		}
	}
	assign := v.regions.Assignments()
	var out []NodeRow
	for _, n := range v.nodes.List() {
		rid, ok := assign[n.NodeID]
		if !ok || !inSubtree[rid] {
			continue
		}
		out = append(out, NodeRow{NodeID: n.NodeID, NodeName: n.NodeName, Region: rid, Online: onlineSide(n.Status), IP: n.IP})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out, true
}

// RegionAlarmStat 是单区域告警计数。
type RegionAlarmStat struct {
	RegionID   string `json:"regionId"` // ""=未归属
	RegionName string `json:"regionName,omitempty"`
	Total      int    `json:"total"`
	Raised     int    `json:"raised"`
	Acked      int    `json:"acked"`
	Assigned   int    `json:"assigned"`
	Closed     int    `json:"closed"`
}

// AlarmStatsResponse 是区域告警汇聚响应。
type AlarmStatsResponse struct {
	Regions    []RegionAlarmStat `json:"regions"`
	Unassigned RegionAlarmStat   `json:"unassigned"`
}

// alarmScanLimit 告警全量扫描上限（spec 裁决 3；超限偏差登记 KI）。
const alarmScanLimit = 2000

// AlarmStats 按节点归属区域聚合计数（未归属/未知区域节点入 unassigned）。
func (v *View) AlarmStats() *AlarmStatsResponse {
	assign := v.regions.Assignments()
	stats := map[string]*RegionAlarmStat{}
	get := func(rid string) *RegionAlarmStat {
		if s, ok := stats[rid]; ok {
			return s
		}
		s := &RegionAlarmStat{RegionID: rid}
		if r, ok := v.regions.RegionByID(rid); ok {
			s.RegionName = r.Name
		}
		stats[rid] = s
		return s
	}
	unassigned := &RegionAlarmStat{}
	bump := func(s *RegionAlarmStat, st string) {
		s.Total++
		switch st {
		case alarm.StateRaised:
			s.Raised++
		case alarm.StateAcked:
			s.Acked++
		case alarm.StateAssigned:
			s.Assigned++
		case alarm.StateClosed:
			s.Closed++
		}
	}
	for _, a := range v.alarms.List("", "", "", alarmScanLimit) {
		if a.NodeID == "" {
			continue
		}
		if rid, ok := assign[a.NodeID]; ok {
			bump(get(rid), a.State)
		} else {
			bump(unassigned, a.State)
		}
	}
	resp := &AlarmStatsResponse{Unassigned: *unassigned, Regions: []RegionAlarmStat{}}
	for _, s := range stats {
		resp.Regions = append(resp.Regions, *s)
	}
	sort.Slice(resp.Regions, func(i, j int) bool { return resp.Regions[i].RegionID < resp.Regions[j].RegionID })
	return resp
}
