package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestV0590ForwardsTestE2E（v0.59.0，spec 0032）：测试投递端点两段——
// US-1 创建 webhook 规则 → POST test 同步返回 delivered:true，接收端收到
// event=test 合成事件 → US-2 台账含 test 条目 + 禁用规则后 test 400。
// 复用 v0520 云端环境（FORWARD_FILE + RBAC on）；无 edgecore（纯管理面）。
func TestV0590ForwardsTestE2E(t *testing.T) {
	buildBinaries(t)
	const opToken = "op-pass-1"
	const viewToken = "view-pass-1"

	hooks := make(chan fwdHookReq, 8)
	rcv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req := fwdHookReq{Body: body, Sig: r.Header.Get("X-EdgeFlow-Signature")}
		_ = json.Unmarshal(body, &req.Payload)
		w.WriteHeader(http.StatusOK)
		hooks <- req
	}))
	defer rcv.Close()

	cloudDataDir = filepath.Join(t.TempDir(), "etcd")
	httpPort, hubPort := reservePort(t), reservePort(t)
	p := v0520StartCloudcore(t, httpPort, hubPort, scenarioUsers(t),
		"EDGEFLOW_CLOUDCORE_FORWARD_FILE="+filepath.Join(t.TempDir(), "forward.json"))
	defer p.stop()
	base := "http://127.0.0.1:" + strconv.Itoa(httpPort)

	// 创建 webhook 规则
	code, body := v0490TermDo(t, http.MethodPost, base+"/api/v1/forwards", opToken, map[string]any{
		"name": "e2e-test-hook", "targetUrl": rcv.URL, "sinkType": "webhook",
		"events": []string{"alarm.raised"}, "enabled": true,
	})
	if code != http.StatusCreated {
		t.Fatalf("创建规则 → %d body=%s", code, body)
	}
	var created struct {
		ID       string `json:"id"`
		SinkType string `json:"sinkType"`
	}
	json.Unmarshal([]byte(body), &created)
	if created.ID == "" || created.SinkType != "webhook" {
		t.Fatalf("创建响应不符: %s", body)
	}

	// ─── US-1 同步测试投递 ───
	code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/forwards/"+created.ID+"/test", opToken, nil)
	if code != http.StatusOK {
		t.Fatalf("US-1 test → %d body=%s", code, body)
	}
	var testResp struct {
		Delivered  bool   `json:"delivered"`
		Attempts   int    `json:"attempts"`
		Status     string `json:"status"`
		DeliveryID string `json:"deliveryId"`
	}
	json.Unmarshal([]byte(body), &testResp)
	if !testResp.Delivered || testResp.Status != "delivered" || testResp.Attempts != 1 {
		t.Fatalf("US-1 测试投递结果不符: %s", body)
	}
	hook := waitFwdEvent(t, hooks, "test", 10*time.Second)
	if hook.Payload.AlarmID != "test-"+created.ID {
		t.Fatalf("US-1 合成事件不符: %+v", hook.Payload)
	}

	// ─── US-2 台账含 test 条目 + 禁用后 test 400 ───
	code, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/forwards/"+created.ID+"/deliveries", viewToken, nil)
	if code != http.StatusOK || !strings.Contains(body, `"event":"test"`) {
		t.Fatalf("US-2 台账应含 test 条目: %d %s", code, body)
	}
	dis := false
	if code, _ = v0490TermDo(t, http.MethodPut, base+"/api/v1/forwards/"+created.ID, opToken,
		map[string]any{"enabled": dis}); code != http.StatusOK {
		t.Fatalf("US-2 禁用 → %d", code)
	}
	if code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/forwards/"+created.ID+"/test", opToken, nil); code != http.StatusBadRequest {
		t.Fatalf("US-2 禁用后 test → %d（want 400）body=%s", code, body)
	}
	t.Logf("US-2 完成（台账 test 条目 + 禁用 400）")
}
