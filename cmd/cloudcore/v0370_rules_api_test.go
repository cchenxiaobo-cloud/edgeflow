// v0.37.0 测试锚（云端）：规则管理 API 9 端点行为契约——CRUD 全路径、
// 治理策略替换、事件查询过滤、规则包下发五态语义、保留段路由。
// 依赖既有 doJSON 辅助（model_api_test.go）与真实 registry/rulestore。
package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"edgeflow/cloud/pkg/cloudhub"
	"edgeflow/cloud/pkg/registry"
	"edgeflow/cloud/pkg/rulestore"
	"edgeflow/pkg/protocol"
	"edgeflow/pkg/rules"
)

// v0370newEnv 构造规则 API 测试环境（send 为 nil 时默认投递成功）。
func v0370newEnv(t *testing.T, send func(context.Context, string, *protocol.Message, cloudhub.ReliableOptions) error) (*httptest.Server, *rulestore.Store, *registry.Registry) {
	t.Helper()
	store := rulestore.New(nil)
	reg := registry.New()
	if send == nil {
		send = func(context.Context, string, *protocol.Message, cloudhub.ReliableOptions) error { return nil }
	}
	mux := http.NewServeMux()
	(&ruleAPI{store: store, reg: reg, reliableSend: send}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, store, reg
}

// v0370rule 构造一条测试规则 JSON。
func v0370rule(id string, threshold float64) map[string]any {
	return map[string]any{
		"ruleId":     id,
		"deviceName": "sensor-01",
		"property":   "temperature",
		"condition":  map[string]any{"type": "threshold", "op": "gt", "value": threshold},
		"action":     map[string]any{"type": "event", "severity": "warning"},
	}
}

func TestV0370RuleCRUD(t *testing.T) {
	srv, _, _ := v0370newEnv(t, nil)
	base := srv.URL

	// POST 创建 → 201
	if code, body := doJSON(t, "POST", base+"/api/v1/rules", v0370rule("hot-1", 80)); code != http.StatusCreated {
		t.Fatalf("创建应 201: %d %v", code, body)
	}
	// 重复创建 → 409
	if code, _ := doJSON(t, "POST", base+"/api/v1/rules", v0370rule("hot-1", 80)); code != http.StatusConflict {
		t.Fatalf("重复应 409: %d", code)
	}
	// 列表 → 1 条
	code, body := doJSON(t, "GET", base+"/api/v1/rules", nil)
	if code != http.StatusOK || body["kind"] != "RuleList" {
		t.Fatalf("列表响应不符: %d %v", code, body)
	}
	if items, _ := body["items"].([]any); len(items) != 1 {
		t.Fatalf("列表条数不符: %v", body["items"])
	}
	// 详情 → 200；不存在 → 404
	if code, _ := doJSON(t, "GET", base+"/api/v1/rules/hot-1", nil); code != http.StatusOK {
		t.Fatalf("详情应 200: %d", code)
	}
	if code, _ := doJSON(t, "GET", base+"/api/v1/rules/absent", nil); code != http.StatusNotFound {
		t.Fatalf("不存在应 404: %d", code)
	}
	// 更新 → 200；不存在 → 404；ruleId 与路径不一致 → 400
	if code, _ := doJSON(t, "PUT", base+"/api/v1/rules/hot-1", v0370rule("hot-1", 90)); code != http.StatusOK {
		t.Fatalf("更新应 200: %d", code)
	}
	if code, _ := doJSON(t, "PUT", base+"/api/v1/rules/absent", v0370rule("absent", 90)); code != http.StatusNotFound {
		t.Fatalf("更新不存在应 404: %d", code)
	}
	if code, _ := doJSON(t, "PUT", base+"/api/v1/rules/hot-1", v0370rule("other", 90)); code != http.StatusBadRequest {
		t.Fatalf("不一致应 400: %d", code)
	}
	// 删除 → 200；再删 → 404
	if code, _ := doJSON(t, "DELETE", base+"/api/v1/rules/hot-1", nil); code != http.StatusOK {
		t.Fatalf("删除应 200: %d", code)
	}
	if code, _ := doJSON(t, "DELETE", base+"/api/v1/rules/hot-1", nil); code != http.StatusNotFound {
		t.Fatalf("重复删除应 404: %d", code)
	}
}

func TestV0370RuleCreateValidation(t *testing.T) {
	srv, _, _ := v0370newEnv(t, nil)
	base := srv.URL
	// ruleId 非法（大写）→ 400
	if code, _ := doJSON(t, "POST", base+"/api/v1/rules", v0370rule("BAD", 80)); code != http.StatusBadRequest {
		t.Fatalf("非法 ruleId 应 400: %d", code)
	}
	// 保留字 → 400
	if code, _ := doJSON(t, "POST", base+"/api/v1/rules", v0370rule("governance", 80)); code != http.StatusBadRequest {
		t.Fatalf("保留字应 400: %d", code)
	}
	// 非法 JSON → 400
	if code, _ := doJSON(t, "POST", base+"/api/v1/rules", map[string]any{"ruleId": 123}); code != http.StatusBadRequest {
		t.Fatalf("非法 JSON 应 400: %d", code)
	}
}

func TestV0370Governance(t *testing.T) {
	srv, store, _ := v0370newEnv(t, nil)
	base := srv.URL

	// 初始为空
	code, body := doJSON(t, "GET", base+"/api/v1/rules/governance", nil)
	if code != http.StatusOK || body["kind"] != "GovernancePolicyList" {
		t.Fatalf("治理列表响应不符: %d %v", code, body)
	}
	if items, _ := body["items"].([]any); len(items) != 0 {
		t.Fatalf("初始应为空: %v", body["items"])
	}
	// 替换 → 200，版本递增
	pol := map[string]any{"deviceName": "sensor-01", "property": "temperature",
		"deadband": 0.5, "range": map[string]any{"min": -50, "max": 150}}
	code, body = doJSON(t, "PUT", base+"/api/v1/rules/governance",
		map[string]any{"policies": []any{pol}})
	if code != http.StatusOK {
		t.Fatalf("替换应 200: %d %v", code, body)
	}
	if v, _ := body["version"].(float64); v <= 0 {
		t.Fatalf("版本应递增: %v", body)
	}
	if store.CountRules() != 0 { // 治理不影响规则计数
		t.Fatal("治理替换不应改动规则")
	}
	// 回读 1 条
	_, body = doJSON(t, "GET", base+"/api/v1/rules/governance", nil)
	if items, _ := body["items"].([]any); len(items) != 1 {
		t.Fatalf("回读应为 1 条: %v", body["items"])
	}
	// 重复策略 → 400
	code, _ = doJSON(t, "PUT", base+"/api/v1/rules/governance",
		map[string]any{"policies": []any{pol, pol}})
	if code != http.StatusBadRequest {
		t.Fatalf("重复策略应 400: %d", code)
	}
	// 非法策略（无过滤器）→ 400
	code, _ = doJSON(t, "PUT", base+"/api/v1/rules/governance",
		map[string]any{"policies": []any{map[string]any{"deviceName": "d", "property": "p"}}})
	if code != http.StatusBadRequest {
		t.Fatalf("非法策略应 400: %d", code)
	}
	// 空数组清空 → 200
	if code, _ := doJSON(t, "PUT", base+"/api/v1/rules/governance",
		map[string]any{"policies": []any{}}); code != http.StatusOK {
		t.Fatalf("清空应 200: %d", code)
	}
	_, body = doJSON(t, "GET", base+"/api/v1/rules/governance", nil)
	if items, _ := body["items"].([]any); len(items) != 0 {
		t.Fatalf("清空后应为空: %v", body["items"])
	}
}

func TestV0370EventsQuery(t *testing.T) {
	srv, store, _ := v0370newEnv(t, nil)
	base := srv.URL

	store.AppendEvent(rules.Event{RuleID: "r1", DeviceName: "d1", Property: "t", Value: 1, TriggeredAt: 100})
	store.AppendEvent(rules.Event{RuleID: "r2", DeviceName: "d1", Property: "t", Value: 2, TriggeredAt: 200})
	store.AppendEvent(rules.Event{RuleID: "r1", DeviceName: "d2", Property: "t", Value: 3, TriggeredAt: 300})

	// 全量（倒序：最新在前）
	code, body := doJSON(t, "GET", base+"/api/v1/rules/events", nil)
	if code != http.StatusOK || body["kind"] != "RuleEventList" {
		t.Fatalf("事件列表响应不符: %d %v", code, body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("应 3 条: %d", len(items))
	}
	first, _ := items[0].(map[string]any)
	if first["ruleId"] != "r1" || first["triggeredAt"].(float64) != 300 {
		t.Fatalf("倒序不符: %v", first)
	}
	// 过滤 + limit
	_, body = doJSON(t, "GET", base+"/api/v1/rules/events?ruleId=r1", nil)
	if items, _ := body["items"].([]any); len(items) != 2 {
		t.Fatalf("过滤 r1 应 2 条: %v", items)
	}
	_, body = doJSON(t, "GET", base+"/api/v1/rules/events?device=d2&limit=1", nil)
	if items, _ := body["items"].([]any); len(items) != 1 {
		t.Fatalf("过滤+limit 应 1 条: %v", items)
	}
	// limit 非法 → 400
	if code, _ := doJSON(t, "GET", base+"/api/v1/rules/events?limit=-1", nil); code != http.StatusBadRequest {
		t.Fatalf("非法 limit 应 400: %d", code)
	}
}

func TestV0370Sync(t *testing.T) {
	var gotMsg *protocol.Message
	var gotNode string
	send := func(_ context.Context, nodeID string, msg *protocol.Message, _ cloudhub.ReliableOptions) error {
		gotNode, gotMsg = nodeID, msg
		return nil
	}
	srv, store, reg := v0370newEnv(t, send)
	base := srv.URL
	if err := reg.Register(registry.NodeInfo{NodeID: "node-1", Arch: "amd64"}); err != nil {
		t.Fatal(err)
	}
	// 准备一条规则 + 一条治理策略
	ctx := context.Background()
	if err := store.CreateRule(ctx, *v0370ruleAsRule(t)); err != nil {
		t.Fatal(err)
	}
	if err := store.SetGovernance(ctx, []rules.GovernancePolicy{
		{DeviceName: "sensor-01", Property: "temperature", Deadband: 1},
	}); err != nil {
		t.Fatal(err)
	}
	// 下发 → 200
	code, body := doJSON(t, "POST", base+"/api/v1/nodes/node-1/rules/sync", nil)
	if code != http.StatusOK {
		t.Fatalf("下发应 200: %d %v", code, body)
	}
	if body["status"] != "ok" || body["nodeID"] != "node-1" {
		t.Fatalf("响应不符: %v", body)
	}
	if rulesN, _ := body["rules"].(float64); rulesN != 1 {
		t.Fatalf("rules 计数不符: %v", body)
	}
	// 断言投递消息内容
	if gotNode != "node-1" || gotMsg == nil || gotMsg.Type != protocol.TypeRuleSync {
		t.Fatalf("投递不符: node=%s msg=%v", gotNode, gotMsg)
	}
	var payload RuleSyncPayload
	if err := gotMsg.DecodePayload(&payload); err != nil {
		t.Fatalf("payload 解码失败: %v", err)
	}
	if payload.RuleSet.Version != store.Version() || len(payload.RuleSet.Rules) != 1 ||
		len(payload.RuleSet.Governance) != 1 {
		t.Fatalf("payload 规则包不符: %+v", payload.RuleSet)
	}

	// 节点不存在 → 404
	if code, _ := doJSON(t, "POST", base+"/api/v1/nodes/absent/rules/sync", nil); code != http.StatusNotFound {
		t.Fatalf("节点不存在应 404: %d", code)
	}
}

func TestV0370SyncFailureMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"离线", cloudhub.ErrNodeOffline, http.StatusNotFound},
		{"超时", cloudhub.ErrAckTimeout, http.StatusGatewayTimeout},
		{"拒绝", cloudhub.ErrAckFailed, http.StatusBadGateway},
		{"其他", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			send := func(context.Context, string, *protocol.Message, cloudhub.ReliableOptions) error {
				return tc.err
			}
			srv, _, reg := v0370newEnv(t, send)
			if err := reg.Register(registry.NodeInfo{NodeID: "node-1"}); err != nil {
				t.Fatal(err)
			}
			code, _ := doJSON(t, "POST", srv.URL+"/api/v1/nodes/node-1/rules/sync", nil)
			if code != tc.want {
				t.Fatalf("期望 %d，得到 %d", tc.want, code)
			}
		})
	}
}

func TestV0370ReservedSegmentRouting(t *testing.T) {
	srv, _, _ := v0370newEnv(t, nil)
	base := srv.URL
	// governance / events 为字面段：应命中专用 handler 而非 {ruleID} 详情
	_, body := doJSON(t, "GET", base+"/api/v1/rules/governance", nil)
	if body["kind"] != "GovernancePolicyList" {
		t.Fatalf("governance 段路由不符: %v", body)
	}
	_, body = doJSON(t, "GET", base+"/api/v1/rules/events", nil)
	if body["kind"] != "RuleEventList" {
		t.Fatalf("events 段路由不符: %v", body)
	}
}

// v0370ruleAsRule 返回测试规则（与 v0370rule 同参数）。
func v0370ruleAsRule(t *testing.T) *rules.Rule {
	t.Helper()
	return &rules.Rule{
		RuleID:     "hot-1",
		DeviceName: "sensor-01",
		Property:   "temperature",
		Condition:  rules.Condition{Type: rules.ConditionThreshold, Op: rules.OpGT, Value: 80},
		Action:     rules.Action{Type: rules.ActionEvent, Severity: rules.SeverityWarning},
	}
}
