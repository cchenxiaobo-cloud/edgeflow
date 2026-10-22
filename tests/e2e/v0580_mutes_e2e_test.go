package e2e

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestV0580MutesE2E（v0.58.0，spec 0031）：告警静默窗口三段——
// US-1 创建窗口后真告警在 summary 活跃口径被排除（历史数据 /api/v1/alarms 不动）/
// US-2 admin 删除窗口后活跃口径恢复计数 / US-3 RBAC（viewer POST 403 / 匿名 401 /
// operator DELETE 403 / viewer GET 200）。
// 复用 v0520 云端环境（MUTE_FILE 持久化 + RBAC on）+ 真实 edgecore 告警管线
// （阈值规则→真告警，同场景 B 链路）。
func TestV0580MutesE2E(t *testing.T) {
	buildBinaries(t)
	const opToken = "op-pass-1"
	const viewToken = "view-pass-1"
	const adminToken = "admin-pass-1"
	nodeID := "e2e-v058-1"

	cloudDataDir = filepath.Join(t.TempDir(), "etcd")
	httpPort, hubPort := reservePort(t), reservePort(t)
	p := v0520StartCloudcore(t, httpPort, hubPort, scenarioUsers(t),
		"EDGEFLOW_CLOUDCORE_MUTE_FILE="+filepath.Join(t.TempDir(), "mute.json"))
	defer p.stop()
	base := "http://127.0.0.1:" + strconv.Itoa(httpPort)
	edge := v0400StartEdgecore(t, t.TempDir(), nodeID, hubPort)
	defer edge.stop()
	scenarioWaitRegistered(t, base, nodeID, viewToken)

	// 区域归属 + 阈值规则（真告警链路）
	regionID := scenarioCreateRegion(t, base, opToken, "静默区", nodeID)
	scenarioCreateThresholdRule(t, base, opToken, nodeID, "e2e-mute-hi", "gt", 28, "critical")

	// ─── US-1 静默窗口生效：告警入库但活跃口径排除 ───
	t.Logf("US-1 创建静默窗口 + 真告警活跃口径排除")
	code, body := v0490TermDo(t, http.MethodPost, base+"/api/v1/alerts/mutes", opToken, map[string]any{
		"nodeId": nodeID, "durationSec": 300, "reason": "e2e 检修窗口",
	})
	if code != http.StatusCreated {
		t.Fatalf("US-1 创建窗口 → %d body=%s", code, body)
	}
	var mute struct {
		ID     string `json:"id"`
		Active bool   `json:"active"`
	}
	json.Unmarshal([]byte(body), &mute)
	if mute.ID == "" || !mute.Active {
		t.Fatalf("US-1 创建响应不符: %s", body)
	}

	scenarioCreateThresholdRule(t, base, opToken, nodeID, "e2e-mute-run", "gt", 28, "critical")
	// 真告警入库（历史数据不动：/api/v1/alarms 可见 raised）
	alarms := scenarioAlarmWait(t, base, viewToken, func(list []alarmJSON) bool {
		for _, a := range list {
			if a.NodeID == nodeID && a.State == "raised" {
				return true
			}
		}
		return false
	}, 90*time.Second)
	t.Logf("US-1 真告警已入库（alarmId=%s）", alarms[0].AlarmID)

	// summary 活跃口径排除：区域 alarmActiveCount == 0（静默中）
	waitSummaryCount := func(want func(count int) bool, timeout time.Duration) int {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for {
			code, body := v0490TermDo(t, http.MethodGet, base+"/api/v1/hierarchy/summary", viewToken, nil)
			if code == http.StatusOK {
				var s struct {
					Children []struct {
						Name             string `json:"name"`
						AlarmActiveCount int    `json:"alarmActiveCount"`
					} `json:"children"`
				}
				json.Unmarshal([]byte(body), &s)
				for _, c := range s.Children {
					if c.Name == "静默区" {
						if want(c.AlarmActiveCount) {
							return c.AlarmActiveCount
						}
						break
					}
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("summary 轮询超时（静默区计数未达期望）: %s", body)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	if got := waitSummaryCount(func(c int) bool { return c == 0 }, 15*time.Second); got != 0 {
		t.Fatalf("US-1 静默中活跃计数应 0: %d", got)
	}

	// ─── US-2 admin 删除窗口 → 活跃口径恢复计数 ───
	t.Logf("US-2 删除窗口 → 活跃口径恢复")
	if code, body = v0490TermDo(t, http.MethodDelete, base+"/api/v1/alerts/mutes/"+mute.ID, adminToken, nil); code != http.StatusOK {
		t.Fatalf("US-2 删除 → %d body=%s", code, body)
	}
	if got := waitSummaryCount(func(c int) bool { return c >= 1 }, 15*time.Second); got < 1 {
		t.Fatalf("US-2 删除后活跃计数未恢复")
	}

	// ─── US-3 RBAC：viewer POST 403 / 匿名 401 / operator DELETE 403 / viewer GET 200 ───
	t.Logf("US-3 RBAC 三态 + DELETE=admin")
	if code, _ = v0490TermDo(t, http.MethodPost, base+"/api/v1/alerts/mutes", viewToken, map[string]any{
		"nodeId": nodeID, "durationSec": 60, "reason": "x"}); code != http.StatusForbidden {
		t.Fatalf("US-3 viewer POST → %d（want 403）", code)
	}
	if code, _ = v0490TermDo(t, http.MethodPost, base+"/api/v1/alerts/mutes", "", map[string]any{
		"nodeId": nodeID, "durationSec": 60, "reason": "x"}); code != http.StatusUnauthorized {
		t.Fatalf("US-3 匿名 POST → %d（want 401）", code)
	}
	if code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/alerts/mutes", opToken, map[string]any{
		"nodeId": nodeID, "durationSec": 60, "reason": "op 窗口"}); code != http.StatusCreated {
		t.Fatalf("US-3 operator POST → %d body=%s", code, body)
	}
	var mute2 struct {
		ID string `json:"id"`
	}
	json.Unmarshal([]byte(body), &mute2)
	if code, _ = v0490TermDo(t, http.MethodDelete, base+"/api/v1/alerts/mutes/"+mute2.ID, opToken, nil); code != http.StatusForbidden {
		t.Fatalf("US-3 operator DELETE → %d（want 403）", code)
	}
	if code, _ = v0490TermDo(t, http.MethodDelete, base+"/api/v1/alerts/mutes/"+mute2.ID, viewToken, nil); code != http.StatusForbidden {
		t.Fatalf("US-3 viewer DELETE → %d（want 403）", code)
	}
	if code, _ = v0490TermDo(t, http.MethodDelete, base+"/api/v1/alerts/mutes/"+mute2.ID, adminToken, nil); code != http.StatusOK {
		t.Fatalf("US-3 admin DELETE → %d", code)
	}
	if code, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/alerts/mutes", viewToken, nil); code != http.StatusOK {
		t.Fatalf("US-3 viewer GET → %d body=%s", code, body)
	}
	t.Logf("US-3 完成（清单 viewer 可读，DELETE=admin 三态实测）")
	_ = regionID
}
