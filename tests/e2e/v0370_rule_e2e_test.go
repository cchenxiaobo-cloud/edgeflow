package e2e

// v0.37.0 端到端：规则引擎全链与数据治理。
//
// 用例一（规则全链）：POST /api/v1/rules 创建恒真规则 →
// POST /api/v1/nodes/{nodeID}/rules/sync 下发 → 边缘采集触发 →
// RuleEvent 上行 → GET /api/v1/rules/events 可见；firing 防重
// （多次上报不重复触发，事件计数稳定）。
//
// 用例二（治理拦截）：PUT /api/v1/rules/governance 下发全越界 range
// 策略 → sync → 后续采集值被拦截（不写影子）→ 云端可见值冻结
// （多次读取恒定）。
//
// 链路：cloudcore（真实进程）+ edgecore（真实进程，上报周期 300ms）+
// mock_sensor（sensor-01，温度/湿度波动）。
import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

// v0370Do 发送 HTTP 请求（可带 JSON body），断言期望状态码，返回响应体。
func v0370Do(t *testing.T, method, url string, body any, want int) string {
	t.Helper()
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化请求体失败: %v", err)
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s 失败: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("%s %s 状态码 = %d，期望 %d，响应: %s", method, url, resp.StatusCode, want, buf.String())
	}
	return buf.String()
}

// ruleEventItem 是 /api/v1/rules/events 的元素（云端 Event 子集）。
type ruleEventItem struct {
	RuleID         string  `json:"ruleId"`
	DeviceName     string  `json:"deviceName"`
	Property       string  `json:"property"`
	Value          float64 `json:"value"`
	Severity       string  `json:"severity"`
	Message        string  `json:"message"`
	TriggeredAt    int64   `json:"triggeredAt"`
	RuleSetVersion int64   `json:"ruleSetVersion"`
}

// ruleEventList 是 /api/v1/rules/events 的响应形态。
type ruleEventList struct {
	Items []ruleEventItem `json:"items"`
}

// v0370waitDeviceProp 轮询直到目标设备属性在云端可见，返回最新值。
func v0370waitDeviceProp(t *testing.T, base, nodeID, deviceName, property string, timeout time.Duration) float64 {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var list deviceStatusList
		getJSON(t, base+"/api/v1/devices", &list)
		for _, it := range list.Items {
			if it.NodeID == nodeID && it.DeviceName == deviceName {
				if v, ok := it.Properties[property]; ok {
					return v
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("等待 %s.%s 上报超时（%v）", deviceName, property, timeout)
	return 0
}

// TestV0370RuleE2E 验证 创建规则 → 下发 → 边缘触发 → 事件查询可见 全链。
func TestV0370RuleE2E(t *testing.T) {
	buildBinaries(t)
	root := repoRoot(t)

	cloud, httpPort, hubPort := startCloudcore(t, root)
	_ = cloud
	base := fmt.Sprintf("http://127.0.0.1:%d", httpPort)
	nodeID := "e2e-v037-1"
	startEdgecore(t, root, nodeID, hubPort)
	waitNodeRegistered(t, base, nodeID)
	t.Logf("节点 %s 已注册", nodeID)

	// 1. 创建恒真规则（temperature > -999 恒满足，forSeconds=0 立即触发）
	rule := map[string]any{
		"ruleId":     "e2e-always",
		"name":       "E2E 恒真规则",
		"deviceName": "sensor-01",
		"property":   "temperature",
		"condition":  map[string]any{"type": "threshold", "op": "gt", "value": -999},
		"action": map[string]any{
			"type": "event", "severity": "critical", "message": "e2e 触发 ${device} 温度 ${value}",
		},
	}
	v0370Do(t, "POST", base+"/api/v1/rules", rule, http.StatusCreated)
	t.Logf("规则已创建")

	// 2. 规则包下发（边缘 Ack 后返回 200）
	v0370Do(t, "POST", base+"/api/v1/nodes/"+nodeID+"/rules/sync", nil, http.StatusOK)
	t.Logf("规则包已下发")

	// 3. 等待规则事件经边→云到达并出现在查询端
	var got ruleEventList
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		getJSON(t, base+"/api/v1/rules/events?ruleId=e2e-always", &got)
		if len(got.Items) >= 1 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(got.Items) < 1 {
		t.Fatalf("等待规则事件超时（30s）")
	}
	ev := got.Items[0]
	if ev.RuleID != "e2e-always" || ev.DeviceName != "sensor-01" || ev.Property != "temperature" {
		t.Fatalf("事件字段不符: %+v", ev)
	}
	if ev.Severity != "critical" || ev.RuleSetVersion < 1 || ev.TriggeredAt <= 0 {
		t.Fatalf("事件严重级/版本/时间不符: %+v", ev)
	}
	if ev.Message == "" || !containsSub(ev.Message, "e2e") {
		t.Fatalf("事件消息不符: %q", ev.Message)
	}
	t.Logf("规则事件已到达云端: %+v", ev)

	// 4. firing 防重：恒真条件下不停触发被抑制，事件计数在多个上报周期后保持稳定
	time.Sleep(1500 * time.Millisecond) // 覆盖 ≥4 个 300ms 上报周期
	var again ruleEventList
	getJSON(t, base+"/api/v1/rules/events?ruleId=e2e-always", &again)
	if len(again.Items) != len(got.Items) {
		t.Fatalf("firing 防重失效：事件计数从 %d 变为 %d", len(got.Items), len(again.Items))
	}
	t.Logf("firing 防重验证通过（事件计数稳定为 %d）", len(again.Items))
}

// TestV0370GovernanceE2E 验证 治理策略下发 → 越界值被拦截 → 云端可见值冻结。
func TestV0370GovernanceE2E(t *testing.T) {
	buildBinaries(t)
	root := repoRoot(t)

	cloud, httpPort, hubPort := startCloudcore(t, root)
	_ = cloud
	base := fmt.Sprintf("http://127.0.0.1:%d", httpPort)
	nodeID := "e2e-v037-2"
	startEdgecore(t, root, nodeID, hubPort)
	waitNodeRegistered(t, base, nodeID)

	// 1. 等温湿度首轮上报
	_ = v0370waitDeviceProp(t, base, nodeID, "sensor-01", "temperature", 60*time.Second)
	_ = v0370waitDeviceProp(t, base, nodeID, "sensor-01", "humidity", 60*time.Second)
	t.Logf("治理前上报已可见")

	// 2. 下发全越界 range 策略（温湿度所有实际值都在 [1000,2000] 之外 → 全部拦截）
	policies := map[string]any{
		"policies": []any{
			map[string]any{"deviceName": "sensor-01", "property": "temperature",
				"range": map[string]any{"min": 1000, "max": 2000}},
			map[string]any{"deviceName": "sensor-01", "property": "humidity",
				"range": map[string]any{"min": 1000, "max": 2000}},
		},
	}
	v0370Do(t, "PUT", base+"/api/v1/rules/governance", policies, http.StatusOK)
	v0370Do(t, "POST", base+"/api/v1/nodes/"+nodeID+"/rules/sync", nil, http.StatusOK)
	t.Logf("治理策略已下发（全越界拦截）")

	// 3. 多次读取：治理生效后（Ack 完成）采集值不再写入影子 → 云端可见值恒定
	time.Sleep(1 * time.Second)
	r1t := v0370waitDeviceProp(t, base, nodeID, "sensor-01", "temperature", 30*time.Second)
	r1h := v0370waitDeviceProp(t, base, nodeID, "sensor-01", "humidity", 30*time.Second)
	time.Sleep(1 * time.Second)
	r2t := v0370waitDeviceProp(t, base, nodeID, "sensor-01", "temperature", 30*time.Second)
	r2h := v0370waitDeviceProp(t, base, nodeID, "sensor-01", "humidity", 30*time.Second)
	time.Sleep(1 * time.Second)
	r3t := v0370waitDeviceProp(t, base, nodeID, "sensor-01", "temperature", 30*time.Second)
	r3h := v0370waitDeviceProp(t, base, nodeID, "sensor-01", "humidity", 30*time.Second)

	if r1t != r2t || r2t != r3t {
		t.Fatalf("治理拦截失效：温度未冻结（%v → %v → %v）", r1t, r2t, r3t)
	}
	if r1h != r2h || r2h != r3h {
		t.Fatalf("治理拦截失效：湿度未冻结（%v → %v → %v）", r1h, r2h, r3h)
	}
	t.Logf("治理拦截验证通过（温度 %v / 湿度 %v 三次读取恒定）", r3t, r3h)
}

// containsSub 子串判定（本文件私有，避免与其他 e2e 文件命名冲突）。
func containsSub(s, sub string) bool {
	return len(s) >= len(sub) && bytes.Contains([]byte(s), []byte(sub))
}
