// v0400_test.go：设定值通道存储单测（spec 0013 US-4/US-5）。
package setpointstore

import (
	"context"
	"errors"
	"testing"
)

// testCreateInput 构造建单入参。
func testCreateInput(nodeID string) CreateInput {
	return CreateInput{
		NodeID: nodeID, Namespace: "default", DeviceName: "sensor-01",
		Property: "targetTemp", Value: 25.5, Operator: "op-1", NowMs: 1000,
	}
}

// TestSetpointCreateTwoPaths 覆盖审批两路径与校验。
func TestSetpointCreateTwoPaths(t *testing.T) {
	s := NewStore(nil)
	ctx := context.Background()
	// 直接投递路径。
	sp, err := s.Create(ctx, testCreateInput("node-1"))
	if err != nil {
		t.Fatalf("建单失败: %v", err)
	}
	if sp.State != StatePendingSend {
		t.Fatalf("默认应 pending-send: %s", sp.State)
	}
	if sp.MsgID == "" || sp.SetpointID == "" {
		t.Fatalf("MsgID/SetpointID 应生成: %+v", sp)
	}
	// 审批路径（单笔 requireApproval）。
	in := testCreateInput("node-1")
	in.RequireApproval = true
	sp2, err := s.Create(ctx, in)
	if err != nil {
		t.Fatalf("审批建单失败: %v", err)
	}
	if sp2.State != StatePendingApproval {
		t.Fatalf("requireApproval 应 pending-approval: %s", sp2.State)
	}
	// 校验失败路径。
	bad := testCreateInput("node-1")
	bad.Operator = ""
	if _, err := s.Create(ctx, bad); !errors.Is(err, ErrValidation) {
		t.Fatalf("缺 operator 应 400: %v", err)
	}
	bad2 := testCreateInput("node-1")
	bad2.DeviceName = ""
	if _, err := s.Create(ctx, bad2); !errors.Is(err, ErrValidation) {
		t.Fatalf("缺 deviceName 应 400: %v", err)
	}
}

// TestSetpointApprovalFlow 覆盖审批状态机与非法迁移。
func TestSetpointApprovalFlow(t *testing.T) {
	s := NewStore(nil)
	ctx := context.Background()
	sp, _ := s.Create(ctx, func() CreateInput {
		in := testCreateInput("node-1")
		in.RequireApproval = true
		return in
	}())
	// pending-approve 直接投递非法（flush 不应取到）。
	if _, err := s.MarkSent(ctx, sp.SetpointID, 2000); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("未审批不可投递: %v", err)
	}
	if len(s.PendingForFlush()) != 0 {
		t.Fatalf("pending-approval 不应进入待投列表")
	}
	// approve → pending-send → 进入待投 → MarkSent。
	got, err := s.Approve(ctx, sp.SetpointID, "boss", 2000)
	if err != nil || got.State != StatePendingSend {
		t.Fatalf("approve 失败: %v, %+v", err, got)
	}
	if len(s.PendingForFlush()) != 1 {
		t.Fatalf("审批后应进入待投列表")
	}
	if _, err := s.Approve(ctx, sp.SetpointID, "boss", 3000); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("重复审批应 409: %v", err)
	}
	if _, err := s.MarkSent(ctx, sp.SetpointID, 3000); err != nil {
		t.Fatalf("MarkSent 失败: %v", err)
	}
	// reject 仅 pending-approval 可用。
	if _, err := s.Reject(ctx, sp.SetpointID, "boss", 4000); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("已发送后 reject 应 409: %v", err)
	}
}

// TestSetpointRejectTerminal 覆盖拒绝终态。
func TestSetpointRejectTerminal(t *testing.T) {
	s := NewStore(nil)
	ctx := context.Background()
	in := testCreateInput("node-1")
	in.RequireApproval = true
	sp, _ := s.Create(ctx, in)
	got, err := s.Reject(ctx, sp.SetpointID, "boss", 2000)
	if err != nil || got.State != StateRejected || got.OutcomeAt != 2000 {
		t.Fatalf("reject 失败: %v, %+v", err, got)
	}
	if _, err := s.Approve(ctx, sp.SetpointID, "boss", 3000); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("终态后审批应 409: %v", err)
	}
}

// TestSetpointResultLoop 覆盖执行反馈闭环（applied/failed/未知 ID）。
func TestSetpointResultLoop(t *testing.T) {
	s := NewStore(nil)
	ctx := context.Background()
	sp, _ := s.Create(ctx, testCreateInput("node-1"))
	if _, err := s.MarkSent(ctx, sp.SetpointID, 2000); err != nil {
		t.Fatalf("MarkSent 失败: %v", err)
	}
	// 成功回告 → applied 终态。
	got, err := s.ApplyResult(ctx, sp.SetpointID, true, "", 3000)
	if err != nil || got.State != StateApplied || got.OutcomeAt != 3000 {
		t.Fatalf("applied 失败: %v, %+v", err, got)
	}
	// 终态后重复回告幂等拒绝（409）——云端状态机消化重复回告。
	if _, err := s.ApplyResult(ctx, sp.SetpointID, true, "", 4000); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("终态后重复回告应 409: %v", err)
	}
	// 失败回告 → failed 终态。
	sp2, _ := s.Create(ctx, testCreateInput("node-2"))
	_, _ = s.MarkSent(ctx, sp2.SetpointID, 2100)
	got2, err := s.ApplyResult(ctx, sp2.SetpointID, false, "exec boom", 3100)
	if err != nil || got2.State != StateFailed || got2.Error != "exec boom" {
		t.Fatalf("failed 失败: %v, %+v", err, got2)
	}
	// pending-send 期间收到回告（回告快于 MarkSent 的乱序窗口）也闭环。
	sp3, _ := s.Create(ctx, testCreateInput("node-3"))
	got3, err := s.ApplyResult(ctx, sp3.SetpointID, true, "", 2200)
	if err != nil || got3.State != StateApplied {
		t.Fatalf("乱序回告应闭环: %v, %+v", err, got3)
	}
	// 未知 ID。
	if _, err := s.ApplyResult(ctx, "nope", true, "", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未知建单应 404: %v", err)
	}
}

// TestSetpointListAndFailPath 覆盖列表过滤与 Ack 失败路径。
func TestSetpointListAndFailPath(t *testing.T) {
	s := NewStore(nil)
	ctx := context.Background()
	sp1, _ := s.Create(ctx, testCreateInput("node-1"))
	sp2, _ := s.Create(ctx, func() CreateInput {
		in := testCreateInput("node-2")
		in.NowMs = 1100
		return in
	}())
	// Ack 失败 → failed 终态。
	got, err := s.MarkFailed(ctx, sp2.SetpointID, "edge rejected", 3000)
	if err != nil || got.State != StateFailed || got.Error != "edge rejected" {
		t.Fatalf("MarkFailed 失败: %v, %+v", err, got)
	}
	// 列表：nodeID / state 过滤 + 排序（新在前）。
	all := s.List("", "", 0)
	if len(all) != 2 || all[0].SetpointID != sp2.SetpointID {
		t.Fatalf("列表排序不符: %+v", all)
	}
	if got := s.List("node-1", "", 0); len(got) != 1 || got[0].SetpointID != sp1.SetpointID {
		t.Fatalf("nodeID 过滤不符: %+v", got)
	}
	if got := s.List("", StatePendingSend, 0); len(got) != 1 || got[0].SetpointID != sp1.SetpointID {
		t.Fatalf("state 过滤不符: %+v", got)
	}
	// 未知 ID。
	if _, err := s.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未知 ID 应 404: %v", err)
	}
}
