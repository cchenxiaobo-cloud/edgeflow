// 诊断聚合面 API（v0.61.0，spec 0034）。
//
// 单端点（134→135）：
//
//	GET /api/v1/diagnostics/summary  系统健康一屏（viewer+）
//
// 聚合只读：各既有 store 的 Count/List/Stats 快照（节点台账/告警/区域/静默/
// 转发规则/etcd 形态/审计台账文件/进程运行时长），无写面、无新依赖。
// 并发：各 store 自带锁，单次聚合多次读非同一快照（计数微偏差可接受，
// 对齐 hierarchyview 包注释口径）。
package main

import (
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"time"

	"edgeflow/cloud/pkg/alarmstore"
	"edgeflow/cloud/pkg/forwardstore"
	"edgeflow/cloud/pkg/mutestore"
	"edgeflow/cloud/pkg/regionstore"
	"edgeflow/cloud/pkg/registry"
)

// diagStartedAt 进程启动毫秒（包初始化 ≈ cloudcore 主进程起点）。
var diagStartedAt = time.Now().UnixMilli()

// diagDeps 是诊断聚合的数据源集合（main.go 装配注入）。
type diagDeps struct {
	nodes     registry.Store
	alarms    *alarmstore.Store
	regions   *regionstore.Store
	mutes     *mutestore.Store
	forwards  *forwardstore.Store
	auditPath string
}

// registerDiagnosticsAPIs 注册诊断聚合端点。
func registerDiagnosticsAPIs(mux *http.ServeMux, d diagDeps) {
	mux.HandleFunc("GET /api/v1/diagnostics/summary", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().UnixMilli()
		resp := map[string]any{"now": now, "startedAt": diagStartedAt, "uptimeSec": (now - diagStartedAt) / 1000}
		resp["goVersion"] = runtime.Version()
		if bi, ok := debug.ReadBuildInfo(); ok {
			resp["goVersion"] = bi.GoVersion
		}

		// 节点台账（在线判定对齐 v0.51 口径：Ready/Unknown=在线侧）。
		nodes := d.nodes.List()
		nodeTotal, nodeOnline := 0, 0
		for _, n := range nodes {
			nodeTotal++
			if n.Status != registry.StatusOffline {
				nodeOnline++
			}
		}
		resp["nodes"] = map[string]int{"total": nodeTotal, "online": nodeOnline}

		// 告警（活跃 = 非 closed）。
		alarms := d.alarms.Stats()
		active := alarms.Total - alarms.ByState["closed"]
		resp["alarms"] = map[string]int64{"total": alarms.Total, "active": active}

		resp["regions"] = map[string]int{"total": len(d.regions.All())}

		// 静默窗口（生效中条数）。
		mutes := d.mutes.List()
		muteActive := 0
		for _, m := range mutes {
			if m.Active(now) {
				muteActive++
			}
		}
		resp["mutes"] = map[string]int{"total": len(mutes), "active": muteActive}

		// 转发规则。
		fw := d.forwards.List()
		fwEnabled := 0
		for _, f := range fw {
			if f.Enabled {
				fwEnabled++
			}
		}
		resp["forwards"] = map[string]int{"total": len(fw), "enabled": fwEnabled}

		// etcd 形态 + 审计台账文件。
		if os.Getenv("EDGEFLOW_CLOUDCORE_ETCD_ENDPOINTS") != "" {
			resp["etcd"] = "external"
		} else {
			resp["etcd"] = "embedded"
		}
		audit := map[string]any{"path": d.auditPath}
		if fi, err := os.Stat(d.auditPath); err == nil {
			audit["bytes"] = fi.Size()
		}
		resp["audit"] = audit

		writeJSON(w, http.StatusOK, resp)
	})
}
