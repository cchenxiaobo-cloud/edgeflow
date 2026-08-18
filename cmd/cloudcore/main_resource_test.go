package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"edgeflow/cloud/pkg/cloudhub"
	"edgeflow/cloud/pkg/registry"
	"edgeflow/pkg/protocol"
	"edgeflow/pkg/resource"
)

// resourceExhaustedAckErr 模拟边缘超卖拒绝：Ack code=error 的 message
// 以 resource.ErrResourceExhausted 为前缀（cloudhub.ErrAckFailed 包装）。
func resourceExhaustedAckErr() error {
	return fmt.Errorf("%w: node=node-1 msgID=m1 message=%q",
		cloudhub.ErrAckFailed, resource.ErrResourceExhausted+": 超出节点资源上限: CPU: 请求 250m + 已有 5800m = 6050m > 上限 6000m")
}

// TestSyncPodResourceExhausted 验证：边缘超卖拒绝（error Ack 带
// ErrResourceExhausted 标记）→ 云端返回 409，语义为「超出节点资源」，
// 区别于通用边缘拒绝 502。
func TestSyncPodResourceExhausted(t *testing.T) {
	srv := newPodSyncServer(t, registry.New(), func(context.Context, string, *protocol.Message, cloudhub.ReliableOptions) error {
		return resourceExhaustedAckErr()
	})
	resp := postPodSync(t, srv, "node-1", `{"operation":"add","pod":{"name":"nginx","image":"nginx:1.25","resources":{"cpuRequest":"250m"}}}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409（超出节点资源）", resp.StatusCode)
	}
	body := readBody(t, resp)
	for _, want := range []string{resource.ErrResourceExhausted, "exceeds node resources", "超出节点资源"} {
		if !strings.Contains(body, want) {
			t.Errorf("409 响应缺少 %q: %s", want, body)
		}
	}
}

// TestSyncPodAckFailedStill502 验证：不带资源耗尽标记的 error Ack 仍是 502
// （通用边缘拒绝语义不变，既有断言不破坏）。
func TestSyncPodAckFailedStill502(t *testing.T) {
	srv := newPodSyncServer(t, registry.New(), func(context.Context, string, *protocol.Message, cloudhub.ReliableOptions) error {
		return fmt.Errorf("%w: node=node-1 msgID=m1 message=%q", cloudhub.ErrAckFailed, "some other edge error")
	})
	resp := postPodSync(t, srv, "node-1", `{"operation":"delete","pod":{"name":"nginx"}}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502", resp.StatusCode)
	}
}

// TestSyncPodRequestExceedsLimit 验证：request > limit 云端前置校验 400，
// 不触发可靠投递。
func TestSyncPodRequestExceedsLimit(t *testing.T) {
	srv := newPodSyncServer(t, registry.New(), func(context.Context, string, *protocol.Message, cloudhub.ReliableOptions) error {
		t.Error("request>limit 不应触发可靠投递")
		return nil
	})
	cases := []string{
		`{"operation":"add","pod":{"name":"a","image":"nginx","resources":{"cpuRequest":"1","cpuLimit":"250m"}}}`,
		`{"operation":"add","pod":{"name":"b","image":"nginx","resources":{"memoryRequest":"128Mi","memoryLimit":"64Mi"}}}`,
	}
	for _, body := range cases {
		resp := postPodSync(t, srv, "node-1", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("request>limit 状态码 = %d，期望 400（body=%s）", resp.StatusCode, body)
		}
		if got := readBody(t, resp); !strings.Contains(got, "invalid resources") {
			t.Errorf("400 响应文案不符: %s", got)
		}
	}
}

// TestSyncPodInvalidResourceFormat 验证：非法资源格式云端前置校验 400。
func TestSyncPodInvalidResourceFormat(t *testing.T) {
	srv := newPodSyncServer(t, registry.New(), func(context.Context, string, *protocol.Message, cloudhub.ReliableOptions) error {
		t.Error("非法资源格式不应触发可靠投递")
		return nil
	})
	cases := []string{
		`{"operation":"add","pod":{"name":"a","image":"nginx","resources":{"cpuRequest":"abc"}}}`,
		`{"operation":"add","pod":{"name":"b","image":"nginx","resources":{"memoryLimit":"12XB"}}}`,
		`{"operation":"add","pod":{"name":"c","image":"nginx","resources":{"cpuRequest":"-1"}}}`,
	}
	for _, body := range cases {
		resp := postPodSync(t, srv, "node-1", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("非法格式状态码 = %d，期望 400（body=%s）", resp.StatusCode, body)
		}
		if got := readBody(t, resp); !strings.Contains(got, "invalid resources") {
			t.Errorf("400 响应文案不符: %s", got)
		}
	}
}

// TestSyncPodValidResources 验证：合法资源字段（request ≤ limit）通过校验，
// 消息正常下发且 resources 字段原样透传（契约只增不改）。
func TestSyncPodValidResources(t *testing.T) {
	var gotMsg *protocol.Message
	srv := newPodSyncServer(t, registry.New(), func(_ context.Context, _ string, msg *protocol.Message, _ cloudhub.ReliableOptions) error {
		gotMsg = msg
		return nil
	})
	body := `{"operation":"add","pod":{"name":"nginx","namespace":"default","image":"nginx:1.25",` +
		`"resources":{"cpuRequest":"250m","cpuLimit":"500m","memoryRequest":"64Mi","memoryLimit":"128Mi"}}}`
	resp := postPodSync(t, srv, "node-1", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200: %s", resp.StatusCode, readBody(t, resp))
	}
	if gotMsg == nil {
		t.Fatal("reliableSend 未收到消息")
	}
	var sent podSyncRequest
	if err := json.Unmarshal(gotMsg.Payload, &sent); err != nil {
		t.Fatalf("解析下发 payload 失败: %v", err)
	}
	if sent.Pod.Resources.CPURequest != "250m" || sent.Pod.Resources.CPULimit != "500m" ||
		sent.Pod.Resources.MemoryRequest != "64Mi" || sent.Pod.Resources.MemoryLimit != "128Mi" {
		t.Errorf("resources 字段未原样透传: %+v", sent.Pod.Resources)
	}
}

// TestSyncPodRequestEqualsLimit 验证边界：request == limit 通过（400 边界）。
func TestSyncPodRequestEqualsLimit(t *testing.T) {
	srv := newPodSyncServer(t, registry.New(), func(context.Context, string, *protocol.Message, cloudhub.ReliableOptions) error {
		return nil
	})
	body := `{"operation":"add","pod":{"name":"nginx","image":"nginx:1.25",` +
		`"resources":{"cpuRequest":"500m","cpuLimit":"500m","memoryRequest":"64Mi","memoryLimit":"64Mi"}}}`
	resp := postPodSync(t, srv, "node-1", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("request==limit 状态码 = %d，期望 200: %s", resp.StatusCode, readBody(t, resp))
	}
}

// TestSyncPodDeleteSkipsResourceCheck 验证：delete 不带 resources 也通过
// （资源校验只针对 add/update）。
func TestSyncPodDeleteSkipsResourceCheck(t *testing.T) {
	srv := newPodSyncServer(t, registry.New(), func(context.Context, string, *protocol.Message, cloudhub.ReliableOptions) error {
		return nil
	})
	resp := postPodSync(t, srv, "node-1", `{"operation":"delete","pod":{"name":"nginx"}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete 状态码 = %d，期望 200: %s", resp.StatusCode, readBody(t, resp))
	}
}

// TestValidateResourcesUnit 验证 validateResources 纯函数各分支。
func TestValidateResourcesUnit(t *testing.T) {
	valid := podResources{CPURequest: "250m", CPULimit: "1", MemoryRequest: "64Mi", MemoryLimit: "1Gi"}
	if err := valid.validateResources(); err != nil {
		t.Errorf("合法资源应通过: %v", err)
	}
	empty := podResources{}
	if err := empty.validateResources(); err != nil {
		t.Errorf("零值资源应通过（不限制）: %v", err)
	}
	bad := []podResources{
		{CPURequest: "1", CPULimit: "250m"},
		{MemoryRequest: "2Gi", MemoryLimit: "1Gi"},
		{CPURequest: "abc"},
		{CPULimit: "xyz"},
		{MemoryRequest: "12XB"},
		{MemoryLimit: "-1"},
	}
	for _, r := range bad {
		if err := r.validateResources(); err == nil {
			t.Errorf("非法资源应拒绝: %+v", r)
		}
	}
}
