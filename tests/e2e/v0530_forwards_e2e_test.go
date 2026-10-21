package e2e

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fwdHookReq 是接收端记录的一次投递。
type fwdHookReq struct {
	Body    []byte
	Sig     string
	Payload struct {
		Event   string `json:"event"`
		AlarmID string `json:"alarmId"`
		NodeID  string `json:"nodeId"`
	}
}

// TestV0530ForwardsE2E（v0.53.0，spec 0026）：数据转发出口五段——
// US-1 全链投递（真告警 raised → webhook + HMAC 签名校验）/ US-2 生命周期投递
// （ack→assign→close 对应三事件）/ US-3 送达台账（delivered 新→旧）/ US-4 读面
// 脱敏与 RBAC（viewer GET 200 无 secret 回显 / viewer POST 403 / 匿名 401 /
// operator PUT 200 / viewer·operator DELETE 403 / admin DELETE 200）/ US-5 删除后
// 详情与台账 404。
// 告警源复用 v0400 管线（edgecore mock 传感器 + 阈值规则）；接收端为测试内 httptest。
func TestV0530ForwardsE2E(t *testing.T) {
	buildBinaries(t)
	usersFile := filepath.Join(t.TempDir(), "users.json")
	preUsers := `[{"id":"boot-admin","hash":"` + v0460Sha256("admin-pass-1") + `","role":"admin"},` +
		`{"id":"op-1","hash":"` + v0460Sha256("op-pass-1") + `","role":"operator"},` +
		`{"id":"view-1","hash":"` + v0460Sha256("view-pass-1") + `","role":"viewer"}]`
	if err := os.WriteFile(usersFile, []byte(preUsers), 0o600); err != nil {
		t.Fatal(err)
	}
	const opToken = "op-pass-1"
	const viewToken = "view-pass-1"
	const adminToken = "admin-pass-1"

	// 接收端：记录投递（body+签名头），恒 200。
	hooks := make(chan fwdHookReq, 32)
	rcv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req := fwdHookReq{Body: body, Sig: r.Header.Get("X-EdgeFlow-Signature")}
		_ = json.Unmarshal(body, &req.Payload)
		w.WriteHeader(http.StatusOK)
		hooks <- req
	}))
	defer rcv.Close()

	// 云端：FORWARD_FILE 持久化 + RBAC on；边端 mock 传感器 + 阈值规则。
	cloudDataDir = filepath.Join(t.TempDir(), "etcd")
	httpPort, hubPort := reservePort(t), reservePort(t)
	p := v0520StartCloudcore(t, httpPort, hubPort, usersFile,
		"EDGEFLOW_CLOUDCORE_FORWARD_FILE="+filepath.Join(t.TempDir(), "forward.json"))
	defer p.stop()
	base := "http://127.0.0.1:" + strconv.Itoa(httpPort)
	nodeID := "e2e-v053-1"
	edge := v0400StartEdgecore(t, t.TempDir(), nodeID, hubPort)
	defer edge.stop()
	// 等节点注册（RBAC on：waitNodeRegistered 不带认证，改带 viewer token 轮询）
	regDeadline := time.Now().Add(60 * time.Second)
	for {
		regCode, regBody := v0490TermDo(t, http.MethodGet, base+"/api/v1/nodes", viewToken, nil)
		if regCode == http.StatusOK && strings.Contains(regBody, nodeID) {
			break
		}
		if time.Now().After(regDeadline) {
			t.Fatalf("节点 %s 60s 未注册: %d %s", nodeID, regCode, regBody)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// ─── US-1 创建规则 + 真告警 raised 投递 ───
	t.Logf("US-1 创建转发规则 + 真告警 raised 投递")
	code, body := v0490TermDo(t, http.MethodPost, base+"/api/v1/forwards", opToken, map[string]any{
		"name": "e2e-hook", "targetUrl": rcv.URL, "secret": "e2e-secret",
		"events":  []string{"alarm.raised", "alarm.acked", "alarm.assigned", "alarm.closed"},
		"enabled": true,
	})
	if code != http.StatusCreated {
		t.Fatalf("US-1 创建规则 → %d body=%s", code, body)
	}
	var created struct {
		ID        string `json:"id"`
		HasSecret bool   `json:"hasSecret"`
		Secret    string `json:"secret"`
	}
	json.Unmarshal([]byte(body), &created)
	if created.ID == "" || !created.HasSecret || created.Secret != "" {
		t.Fatalf("US-1 创建响应不符（secret 应不回显）: %s", body)
	}

	if code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/rules", opToken, map[string]any{
		"ruleId": "e2e-fwd-hi", "name": "E2E 转发告警", "deviceName": "sensor-01", "property": "temperature",
		"condition": map[string]any{"type": "threshold", "op": "gt", "value": 28},
		"action":    map[string]any{"type": "event", "severity": "critical", "message": "fwd 触发 ${value}"},
	}); code != http.StatusCreated {
		t.Fatalf("US-1 建阈值规则 → %d body=%s", code, body)
	}
	if code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/nodes/"+nodeID+"/rules/sync", opToken, nil); code != http.StatusOK {
		t.Fatalf("US-1 规则下发 → %d body=%s", code, body)
	}

	// 等 raised 投递（mock 传感器穿越阈值需要时间；90s 上限对齐 v0400）
	waitHook := func(wantEvent string, timeout time.Duration) fwdHookReq {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for {
			select {
			case h := <-hooks:
				if h.Payload.Event == wantEvent {
					return h
				}
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s 等待事件 %s 超时", "投递接收", wantEvent)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	raised := waitHook("alarm.raised", 90*time.Second)
	mac := hmac.New(sha256.New, []byte("e2e-secret"))
	mac.Write(raised.Body)
	if raised.Sig != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("US-1 HMAC 签名校验失败: %s", raised.Sig)
	}
	if raised.Payload.AlarmID == "" || raised.Payload.NodeID != nodeID {
		t.Fatalf("US-1 payload 不符: %+v", raised.Payload)
	}
	t.Logf("US-1 完成（alarmId=%s）", raised.Payload.AlarmID)

	// ─── US-2 生命周期投递：ack → assign → close（对齐 v0400 合法迁移链）───
	t.Logf("US-2 生命周期投递：acked + assigned + closed")
	if code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/alarms/"+raised.Payload.AlarmID+"/ack", opToken,
		map[string]string{"operator": "op-1"}); code != http.StatusOK {
		t.Fatalf("US-2 ack → %d body=%s", code, body)
	}
	if code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/alarms/"+raised.Payload.AlarmID+"/assign", opToken,
		map[string]any{"operator": "op-1", "assignee": "维护班", "ticketRef": "GD-E2E-FWD"}); code != http.StatusOK {
		t.Fatalf("US-2 assign → %d body=%s", code, body)
	}
	if code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/alarms/"+raised.Payload.AlarmID+"/close", opToken,
		map[string]string{"operator": "op-1"}); code != http.StatusOK {
		t.Fatalf("US-2 close → %d body=%s", code, body)
	}
	waitHook("alarm.acked", 30*time.Second)
	waitHook("alarm.assigned", 30*time.Second)
	waitHook("alarm.closed", 30*time.Second)

	// ─── US-3 送达台账：本告警 3 条生命周期事件 delivered，新→旧 ───
	t.Logf("US-3 送达台账：delivered 新→旧")
	var ledger struct {
		Deliveries []struct {
			Event   string `json:"event"`
			Status  string `json:"status"`
			AlarmID string `json:"alarmId"`
		} `json:"deliveries"`
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/forwards/"+created.ID+"/deliveries", viewToken, nil)
		if code != http.StatusOK {
			t.Fatalf("US-3 台账 → %d body=%s", code, body)
		}
		json.Unmarshal([]byte(body), &ledger)
		mine := 0
		for _, d := range ledger.Deliveries {
			if d.AlarmID == raised.Payload.AlarmID {
				mine++
			}
		}
		if mine >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("US-3 台账 10s 未收齐本告警 3 事件: %s", body)
		}
		time.Sleep(200 * time.Millisecond)
	}
	var mine []string // 本告警事件序列（新→旧）
	for _, d := range ledger.Deliveries {
		if d.AlarmID != raised.Payload.AlarmID {
			continue
		}
		if d.Status != "delivered" {
			t.Fatalf("US-3 存在非 delivered: %+v", d)
		}
		mine = append(mine, d.Event)
	}
	if len(mine) < 3 || mine[0] != "alarm.closed" || mine[1] != "alarm.assigned" || mine[2] != "alarm.acked" {
		t.Fatalf("US-3 台账顺序不符（新→旧应 closed/assigned/acked）: %v", mine)
	}

	// ─── US-4 脱敏与 RBAC（含首个 DELETE=admin 面）───
	t.Logf("US-4 脱敏与 RBAC 三态 + DELETE=admin")
	code, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/forwards", viewToken, nil)
	if code != http.StatusOK || strings.Contains(body, `"secret"`) {
		t.Fatalf("US-4 清单脱敏不符: %d %s", code, body)
	}
	if code, _ = v0490TermDo(t, http.MethodPost, base+"/api/v1/forwards", viewToken, map[string]any{
		"name": "x", "targetUrl": "http://a.com/h", "events": []string{"alarm.raised"}}); code != http.StatusForbidden {
		t.Fatalf("US-4 viewer POST → %d（want 403）", code)
	}
	if code, _ = v0490TermDo(t, http.MethodPost, base+"/api/v1/forwards", "", map[string]any{
		"name": "x", "targetUrl": "http://a.com/h", "events": []string{"alarm.raised"}}); code != http.StatusUnauthorized {
		t.Fatalf("US-4 匿名 POST → %d（want 401）", code)
	}
	dis := false
	if code, _ = v0490TermDo(t, http.MethodPut, base+"/api/v1/forwards/"+created.ID, opToken,
		map[string]any{"enabled": dis}); code != http.StatusOK {
		t.Fatalf("US-4 operator PUT → %d", code)
	}
	if code, _ = v0490TermDo(t, http.MethodDelete, base+"/api/v1/forwards/"+created.ID, viewToken, nil); code != http.StatusForbidden {
		t.Fatalf("US-4 viewer DELETE → %d（want 403）", code)
	}
	if code, _ = v0490TermDo(t, http.MethodDelete, base+"/api/v1/forwards/"+created.ID, opToken, nil); code != http.StatusForbidden {
		t.Fatalf("US-4 operator DELETE → %d（want 403：delete=admin）", code)
	}
	en := true
	if code, _ = v0490TermDo(t, http.MethodPut, base+"/api/v1/forwards/"+created.ID, adminToken,
		map[string]any{"enabled": en}); code != http.StatusOK {
		t.Fatalf("US-4 admin PUT → %d", code)
	}
	if code, _ = v0490TermDo(t, http.MethodDelete, base+"/api/v1/forwards/"+created.ID, adminToken, nil); code != http.StatusOK {
		t.Fatalf("US-4 admin DELETE → %d", code)
	}

	// ─── US-5 删除后详情与台账 404 ───
	if code, _ = v0490TermDo(t, http.MethodGet, base+"/api/v1/forwards/"+created.ID, viewToken, nil); code != http.StatusNotFound {
		t.Fatalf("US-5 删除后详情 → %d（want 404）", code)
	}
	if code, _ = v0490TermDo(t, http.MethodGet, base+"/api/v1/forwards/"+created.ID+"/deliveries", viewToken, nil); code != http.StatusNotFound {
		t.Fatalf("US-5 删除后台账 → %d（want 404）", code)
	}
	t.Logf("US-5 完成（删除后台账/详情 404）")
}
