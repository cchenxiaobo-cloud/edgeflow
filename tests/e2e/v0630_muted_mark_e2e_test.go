package e2e

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestV0630MutedMarkE2E（v0.63.0，spec 0036）：告警 muted 标记与静默统计两段——
// US-1 静默生效期间 /api/v1/alarms 列表 muted:true（告警历史数据不动）/
// US-2 mutes/stats 聚合（total/active/byNode）。复用场景 B 链路（真告警管线）。
func TestV0630MutedMarkE2E(t *testing.T) {
	buildBinaries(t)
	const opToken = "op-pass-1"
	const viewToken = "view-pass-1"
	nodeID := "e2e-v063-1"

	cloudDataDir = filepath.Join(t.TempDir(), "etcd")
	httpPort, hubPort := reservePort(t), reservePort(t)
	p := v0520StartCloudcore(t, httpPort, hubPort, scenarioUsers(t),
		"EDGEFLOW_CLOUDCORE_MUTE_FILE="+t.TempDir()+"/mute.json")
	defer p.stop()
	base := "http://127.0.0.1:" + strconv.Itoa(httpPort)
	edge := v0400StartEdgecore(t, t.TempDir(), nodeID, hubPort)
	defer edge.stop()
	scenarioWaitRegistered(t, base, nodeID, viewToken)

	scenarioCreateRegion(t, base, opToken, "标记区", nodeID)
	scenarioCreateThresholdRule(t, base, opToken, nodeID, "e2e-v063-hi", "gt", 28, "critical")

	// 静默窗口先于告警创建（muted 标记即时生效）
	code, body := v0490TermDo(t, http.MethodPost, base+"/api/v1/alerts/mutes", opToken, map[string]any{
		"nodeId": nodeID, "severity": "critical", "durationSec": 300, "reason": "e2e v063",
	})
	if code != http.StatusCreated {
		t.Fatalf("创建静默 → %d body=%s", code, body)
	}

	// ─── US-1 真告警列表 muted:true ───
	t.Logf("US-1 静默中真告警列表 muted:true")
	alarms := scenarioAlarmWait(t, base, viewToken, func(list []alarmJSON) bool {
		for _, a := range list {
			if a.NodeID == nodeID && a.State == "raised" {
				return true
			}
		}
		return false
	}, 90*time.Second)
	var raisedAlarm alarmJSON
	for _, a := range alarms {
		if a.NodeID == nodeID {
			raisedAlarm = a
			break
		}
	}

	code, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/alarms?nodeID="+nodeID, viewToken, nil)
	if code != http.StatusOK || !strings.Contains(body, `"muted":true`) {
		t.Fatalf("US-1 列表 muted 标记不符: %d %s", code, body)
	}
	if !strings.Contains(body, `"state":"raised"`) {
		t.Fatalf("US-1 告警历史数据应保留: %s", body)
	}
	t.Logf("US-1 完成（muted:true，历史数据保留）")

	// ─── US-2 mutes/stats 聚合 ───
	t.Logf("US-2 静默统计")
	code, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/alerts/mutes/stats", viewToken, nil)
	if code != http.StatusOK {
		t.Fatalf("US-2 stats → %d body=%s", code, body)
	}
	var st struct {
		Total  int64            `json:"total"`
		Active int64            `json:"active"`
		ByNode map[string]int64 `json:"byNode"`
	}
	json.Unmarshal([]byte(body), &st)
	if st.Total != 1 || st.Active != 1 || st.ByNode[nodeID] != 1 {
		t.Fatalf("US-2 统计不符: %s", body)
	}

	// 解除静默 → muted:false
	if code, _ = v0490TermDo(t, http.MethodDelete, base+"/api/v1/alerts/mutes/"+muteIDOf(t, base, "admin-pass-1", 0), "admin-pass-1", nil); code != http.StatusOK {
		t.Fatalf("US-2 删除静默 → %d", code)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/alarms?nodeID="+nodeID, viewToken, nil)
		if code == http.StatusOK && !strings.Contains(body, `"muted":true`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("US-2 解除后 muted 应 false: %s", body)
		}
		time.Sleep(500 * time.Millisecond)
	}
	_ = raisedAlarm
	t.Logf("US-2 完成（解除后 muted:false）")
}

// muteIDOf 取指定节点的第一个生效静默窗口 id。
func muteIDOf(t *testing.T, base, token string, _ int) string {
	t.Helper()
	code, body := v0490TermDo(t, http.MethodGet, base+"/api/v1/alerts/mutes", token, nil)
	if code != http.StatusOK {
		t.Fatalf("清单 → %d body=%s", code, body)
	}
	var list struct {
		Mutes []struct {
			ID     string `json:"id"`
			Active bool   `json:"active"`
		} `json:"mutes"`
	}
	json.Unmarshal([]byte(body), &list)
	for _, m := range list.Mutes {
		if m.Active {
			return m.ID
		}
	}
	t.Fatalf("无生效静默窗口: %s", body)
	return ""
}
