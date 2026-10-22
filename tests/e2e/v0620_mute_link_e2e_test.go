package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestV0620MuteForwardLinkE2E（v0.62.0，spec 0035）：静默联动通知抑制两段——
// US-1 静默窗口生效期间真告警不投递 webhook（活跃口径排除 + 接收端零收包）/
// US-2 删除窗口后新告警正常送达。复用场景 B 链路（真告警管线 + 区域归属）。
func TestV0620MuteForwardLinkE2E(t *testing.T) {
	buildBinaries(t)
	const opToken = "op-pass-1"
	const viewToken = "view-pass-1"
	nodeID := "e2e-v062-1"

	hooks := make(chan fwdHookReq, 8)
	rcv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 4096)
		n, _ := r.Body.Read(body)
		req := fwdHookReq{Body: body[:n], Sig: r.Header.Get("X-EdgeFlow-Signature")}
		_ = json.Unmarshal(req.Body, &req.Payload)
		w.WriteHeader(http.StatusOK)
		hooks <- req
	}))
	defer rcv.Close()

	cloudDataDir = filepath.Join(t.TempDir(), "etcd")
	httpPort, hubPort := reservePort(t), reservePort(t)
	p := v0520StartCloudcore(t, httpPort, hubPort, scenarioUsers(t),
		"EDGEFLOW_CLOUDCORE_FORWARD_FILE="+filepath.Join(t.TempDir(), "v062-files"),
		"EDGEFLOW_CLOUDCORE_MUTE_FILE="+filepath.Join(t.TempDir(), "v062-files"))
	defer p.stop()
	base := "http://127.0.0.1:" + strconv.Itoa(httpPort)
	edge := v0400StartEdgecore(t, t.TempDir(), nodeID, hubPort)
	defer edge.stop()
	scenarioWaitRegistered(t, base, nodeID, viewToken)

	scenarioCreateRegion(t, base, opToken, "联动区", nodeID)
	// 静默窗口（节点级，先于告警创建）
	code, body := v0490TermDo(t, http.MethodPost, base+"/api/v1/alerts/mutes", opToken, map[string]any{
		"nodeId": nodeID, "durationSec": 300, "reason": "e2e 静默联动",
	})
	if code != http.StatusCreated {
		t.Fatalf("创建静默 → %d body=%s", code, body)
	}
	var mute struct {
		ID string `json:"id"`
	}
	json.Unmarshal([]byte(body), &mute)

	// 转发规则 + 阈值规则（真告警管线）
	if code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/forwards", opToken, map[string]any{
		"name": "e2e-link-hook", "targetUrl": rcv.URL, "sinkType": "webhook",
		"events": []string{"alarm.raised", "alarm.acked", "alarm.closed"}, "enabled": true,
	}); code != http.StatusCreated {
		t.Fatalf("创建转发规则 → %d body=%s", code, body)
	}
	scenarioCreateThresholdRule(t, base, opToken, nodeID, "e2e-link-hi", "gt", 28, "critical")

	// ─── US-1 静默中：告警入库（历史不动）但 webhook 零收包 ───
	t.Logf("US-1 静默中真告警不投递")
	alarms := scenarioAlarmWait(t, base, viewToken, func(list []alarmJSON) bool {
		for _, a := range list {
			if a.NodeID == nodeID && a.State == "raised" {
				return true
			}
		}
		return false
	}, 90*time.Second)
	t.Logf("US-1 真告警已入库（alarmId=%s）", alarms[0].AlarmID)

	// summary 活跃口径 0（节点级静默排除）
	waitSummaryCount := func(want func(count int) bool, timeout time.Duration) {
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
					if c.Name == "联动区" && want(c.AlarmActiveCount) {
						return
					}
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("summary 轮询超时: %s", body)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	waitSummaryCount(func(c int) bool { return c == 0 }, 15*time.Second)

	// 接收端零收包（静默窗口内无投递）
	select {
	case h := <-hooks:
		t.Fatalf("US-1 静默中不应有投递: %+v", h.Payload)
	case <-time.After(2 * time.Second):
	}
	t.Logf("US-1 完成（活跃口径 0 + webhook 零收包）")

	// ─── US-2 删除窗口 → 解除后新告警正常送达 ───
	t.Logf("US-2 删除窗口 → 解除后送达")
	if code, body = v0490TermDo(t, http.MethodDelete, base+"/api/v1/alerts/mutes/"+mute.ID, "admin-pass-1", nil); code != http.StatusOK {
		t.Fatalf("US-2 删除静默 → %d body=%s", code, body)
	}
	// 静默解除后，既有 raised 告警已过——用第二阈值规则触发新告警验证投递恢复
	scenarioCreateThresholdRule(t, base, opToken, nodeID, "e2e-link-lo", "lt", 28, "critical")
	_ = waitFwdEvent(t, hooks, "alarm.raised", 90*time.Second)
	t.Logf("US-2 完成（解除后新告警正常送达）")
	_ = viewToken
}
