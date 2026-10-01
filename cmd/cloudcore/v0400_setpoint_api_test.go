// v0400_setpoint_api_test.go：设定值通道 API 与投递 flush 单测（spec 0013 US-4/US-5/US-6）。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"edgeflow/cloud/pkg/cloudhub"
	"edgeflow/cloud/pkg/setpointstore"
	"edgeflow/pkg/protocol"
)

// newSetpointTestAPI 构造设定值 API 与测试路由（redeliverMs 设大禁用意外重投，
// 重投行为在专用用例中验证）。
func newSetpointTestAPI(t *testing.T, approvalOn bool) (*http.ServeMux, *setpointAPI) {
	t.Helper()
	st := setpointstore.NewStore(nil)
	api := &setpointAPI{store: st, approvalForced: approvalOn, redeliverMs: 1 << 62}
	mux := http.NewServeMux()
	api.Register(mux)
	return mux, api
}

// TestSetpointAPINodeNotFound 覆盖建单节点存在性校验（spec 0013 US-4，复核 P1-1）。
func TestSetpointAPINodeNotFound(t *testing.T) {
	st := setpointstore.NewStore(nil)
	api := &setpointAPI{store: st, nodeExists: func(nodeID string) bool { return nodeID == "node-1" }}
	mux := http.NewServeMux()
	api.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body := map[string]any{"deviceName": "d", "property": "p", "value": 1.0, "operator": "op"}
	code, _ := v0400Do(t, "POST", srv.URL+"/api/v1/nodes/nope/setpoints", body)
	if code != 404 {
		t.Fatalf("未知节点建单应 404: %d", code)
	}
	code, _ = v0400Do(t, "POST", srv.URL+"/api/v1/nodes/node-1/setpoints", body)
	if code != 200 {
		t.Fatalf("存在节点建单应 200: %d", code)
	}
}

// TestSetpointAPICreateAndList 覆盖建单两路径与列表。
func TestSetpointAPICreateAndList(t *testing.T) {
	mux, api := newSetpointTestAPI(t, false)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body := map[string]any{"namespace": "default", "deviceName": "sensor-01",
		"property": "targetTemp", "value": 25.5, "operator": "op-1"}
	code, resp := v0400Do(t, "POST", srv.URL+"/api/v1/nodes/node-1/setpoints", body)
	if code != 200 {
		t.Fatalf("建单应 200: %d %s", code, resp)
	}
	var sp setpointstore.Setpoint
	if err := json.Unmarshal([]byte(resp), &sp); err != nil || sp.State != setpointstore.StatePendingSend {
		t.Fatalf("默认应 pending-send: %s", resp)
	}
	// 单笔 requireApproval → pending-approval。
	body["requireApproval"] = true
	code, resp = v0400Do(t, "POST", srv.URL+"/api/v1/nodes/node-1/setpoints", body)
	if code != 200 {
		t.Fatalf("审批建单应 200: %d", code)
	}
	var sp2 setpointstore.Setpoint
	_ = json.Unmarshal([]byte(resp), &sp2)
	if sp2.State != setpointstore.StatePendingApproval {
		t.Fatalf("requireApproval 应 pending-approval: %s", resp)
	}
	// 缺 operator → 400。
	code, _ = v0400Do(t, "POST", srv.URL+"/api/v1/nodes/node-1/setpoints",
		map[string]any{"deviceName": "d", "property": "p"})
	if code != 400 {
		t.Fatalf("缺 operator 应 400: %d", code)
	}
	// 列表。
	code, resp = v0400Do(t, "GET", srv.URL+"/api/v1/setpoints?state=pending-send", nil)
	if code != 200 {
		t.Fatalf("列表应 200: %d", code)
	}
	var listResp struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal([]byte(resp), &listResp)
	if listResp.Count != 1 {
		t.Fatalf("state 过滤不符: %s", resp)
	}
	_ = api
}

// TestSetpointAPIApprovalEndpoints 覆盖审批端点与 409/400。
func TestSetpointAPIApprovalEndpoints(t *testing.T) {
	mux, _ := newSetpointTestAPI(t, false)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 未知建单 → 404。
	code, _ := v0400Do(t, "POST", srv.URL+"/api/v1/setpoints/nope/approval",
		map[string]string{"action": "approve", "operator": "op"})
	if code != 404 {
		t.Fatalf("未知建单应 404: %d", code)
	}
	// 非法 action → 400。
	code, _ = v0400Do(t, "POST", srv.URL+"/api/v1/setpoints/nope/approval",
		map[string]string{"action": "hmm", "operator": "op"})
	if code != 400 {
		t.Fatalf("非法 action 应 400: %d", code)
	}
}

// fakeReliable 记录可靠投递调用并按脚本返回错误。
type fakeReliable struct {
	mu      sync.Mutex
	calls   []string          // msg.ID 列表
	lastMsg *protocol.Message // 最近一次投递的消息（负载契约断言用）
	script  []error           // 逐次返回（耗尽后重复最后一条）
}

func (f *fakeReliable) send(ctx context.Context, nodeID string, msg *protocol.Message, opts cloudhub.ReliableOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, msg.ID)
	cp := *msg
	f.lastMsg = &cp
	if len(f.script) == 0 {
		return nil
	}
	err := f.script[0]
	if len(f.script) > 1 {
		f.script = f.script[1:]
	}
	return err
}

// lastPayload 返回最近一次投递的负载（无则 nil）。
func (f *fakeReliable) lastPayload() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastMsg == nil {
		return nil
	}
	return f.lastMsg.Payload
}

// lastID 返回最近一次投递的消息 ID（无则空串）。
func (f *fakeReliable) lastID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastMsg == nil {
		return ""
	}
	return f.lastMsg.ID
}

// callCount 返回累计投递次数。
func (f *fakeReliable) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// TestSetpointFlushOnce 覆盖投递轮：成功 sent / 离线留单 / AckFailed 终态 / 同 ID 重发。
func TestSetpointFlushOnce(t *testing.T) {
	ctx := context.Background()
	st := setpointstore.NewStore(nil)
	fr := &fakeReliable{}
	api := &setpointAPI{store: st, reliableSend: fr.send}

	// 建单 1：直接成功。
	sp1, _ := st.Create(ctx, setpointstore.CreateInput{NodeID: "node-1", DeviceName: "d",
		Property: "p", Value: 1, Operator: "op", NowMs: 1000})
	api.flushOnce(ctx)
	got, _ := st.Get(sp1.SetpointID)
	if got.State != setpointstore.StateSent {
		t.Fatalf("投递后应 sent: %+v", got)
	}
	if len(fr.calls) != 1 || fr.calls[0] != sp1.MsgID {
		t.Fatalf("应使用建单 MsgID: %v", fr.calls)
	}
	// 消息负载契约：class=setpoint + setpointId。
	raw := fr.lastPayload()
	if raw == nil {
		t.Fatalf("应记录消息负载")
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("解析负载失败: %v", err)
	}
	if payload["class"] != "setpoint" || payload["setpointId"] != sp1.SetpointID {
		t.Fatalf("负载契约不符: %v", payload)
	}

	// 建单 2：离线留单（恢复重投语义）。
	sp2, _ := st.Create(ctx, setpointstore.CreateInput{NodeID: "node-2", DeviceName: "d",
		Property: "p", Value: 2, Operator: "op", NowMs: 1100})
	fr.mu.Lock()
	fr.script = []error{cloudhub.ErrNodeOffline, nil} // 首轮离线、次轮恢复
	fr.mu.Unlock()
	api.flushOnce(ctx)
	got2, _ := st.Get(sp2.SetpointID)
	if got2.State != setpointstore.StatePendingSend {
		t.Fatalf("离线应留单 pending-send: %+v", got2)
	}
	// 恢复后重投成功（同 MsgID）。
	api.flushOnce(ctx)
	got2, _ = st.Get(sp2.SetpointID)
	if got2.State != setpointstore.StateSent {
		t.Fatalf("恢复后应 sent: %+v", got2)
	}
	if len(fr.calls) < 3 || fr.calls[len(fr.calls)-1] != sp2.MsgID {
		t.Fatalf("重投应保持同 MsgID: %v", fr.calls)
	}

	// 建单 3：边缘拒绝（AckFailed）→ failed 终态。
	sp3, _ := st.Create(ctx, setpointstore.CreateInput{NodeID: "node-3", DeviceName: "d",
		Property: "p", Value: 3, Operator: "op", NowMs: 1200})
	fr.mu.Lock()
	fr.script = []error{cloudhub.ErrAckFailed}
	fr.mu.Unlock()
	api.flushOnce(ctx)
	got3, _ := st.Get(sp3.SetpointID)
	if got3.State != setpointstore.StateFailed {
		t.Fatalf("AckFailed 应 failed: %+v", got3)
	}
	if got3.Error == "" {
		t.Fatalf("failed 应记录原因")
	}
}

// TestParseSetpointFlushSec 覆盖 env 解析回退。
func TestParseSetpointFlushSec(t *testing.T) {
	t.Setenv(envSetpointFlushSec, "")
	if got := parseSetpointFlushSec(); got != defaultSetpointFlushSec {
		t.Fatalf("缺省应 %d: %d", defaultSetpointFlushSec, got)
	}
	t.Setenv(envSetpointFlushSec, "5")
	if got := parseSetpointFlushSec(); got != 5 {
		t.Fatalf("应取 5: %d", got)
	}
	t.Setenv(envSetpointFlushSec, "0")
	if got := parseSetpointFlushSec(); got != defaultSetpointFlushSec {
		t.Fatalf("非法应回退: %d", got)
	}
	_ = errors.New
}

// TestSetpointFlushRedeliversSent 覆盖 sent 未回告重投闭环（复核 P1-2）：
// 回告丢失的 sent 建单在窗口后被重投（新投递 ID 绕开边缘 MsgID 去重短路），
// 重复应用幂等，回告到达后 applied 终态；终态后不再重投。
func TestSetpointFlushRedeliversSent(t *testing.T) {
	ctx := context.Background()
	st := setpointstore.NewStore(nil)
	fr := &fakeReliable{}
	api := &setpointAPI{store: st, reliableSend: fr.send, redeliverMs: 0} // 0 = 立即进入重投窗口

	sp, _ := st.Create(ctx, setpointstore.CreateInput{NodeID: "node-9", DeviceName: "d",
		Property: "p", Value: 9, Operator: "op", NowMs: 1000})
	api.flushOnce(ctx) // 首投 → sent
	got, _ := st.Get(sp.SetpointID)
	if got.State != setpointstore.StateSent || got.Attempts != 0 {
		t.Fatalf("首投后应 sent/Attempts=0: %+v", got)
	}
	time.Sleep(5 * time.Millisecond) // 越过重投窗口判定（SentForRedelivery 严格大于）
	// 回告丢失（不调 ApplyResult）→ 下一轮重投：新投递 ID、Attempts=1、保持 sent。
	api.flushOnce(ctx)
	got, _ = st.Get(sp.SetpointID)
	if got.State != setpointstore.StateSent || got.Attempts != 1 {
		t.Fatalf("重投后应 sent/Attempts=1: %+v", got)
	}
	if got2 := fr.lastID(); got2 != sp.MsgID+"-r1" {
		t.Fatalf("重投应派生新 ID %q: %q", sp.MsgID+"-r1", got2)
	}
	// 再投一次（重投仍未回告）：ID 递增 -r2。
	time.Sleep(2 * time.Millisecond)
	api.flushOnce(ctx)
	if got3 := fr.lastID(); got3 != sp.MsgID+"-r2" {
		t.Fatalf("二次重投应 -r2: %q", got3)
	}
	// 回告到达 → applied 终态；此后不再重投。
	if _, err := st.ApplyResult(ctx, sp.SetpointID, true, "", 5000); err != nil {
		t.Fatalf("回告闭环失败: %v", err)
	}
	before := fr.callCount()
	api.flushOnce(ctx)
	if fr.callCount() != before {
		t.Fatalf("终态后不应重投")
	}
	if _, err := st.Get(sp.SetpointID); err != nil {
		t.Fatalf("查询失败: %v", err)
	}
}
