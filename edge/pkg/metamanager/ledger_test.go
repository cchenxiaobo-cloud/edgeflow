// 操作台账测试（WBS 5.2 验收：台账可查 + 保留 30 天）。
//
// 覆盖：
//   - SaveOp 落盘持久化（重启后仍可查——用同一 DB 文件重新 Open 验证）；
//   - ListOps 按 设备名/方向/时间范围 组合条件查询；
//   - CleanupOps 删除超过保留期的记录（30 天），保留期内记录不受影响。
package metamanager

import (
	"path/filepath"
	"testing"
	"time"
)

// newTestLedger 在临时目录创建台账并注册清理。
func newTestLedger(t *testing.T) (*Ledger, *Store) {
	t.Helper()
	s := newTestStore(t)
	l, err := NewLedger(s)
	if err != nil {
		t.Fatalf("NewLedger 失败: %v", err)
	}
	return l, s
}

// saveSampleOps 写入一批覆盖不同设备/方向/时间的样本记录。
func saveSampleOps(t *testing.T, l *Ledger, baseTs int64) {
	t.Helper()
	ops := []OpRecord{
		{Ts: baseTs, DeviceID: "mb-sensor-01", Direction: DirUp, RegAddr: "0x0000-0x0001", Value: "250/600", Result: "ok", Message: "读温度+湿度"},
		{Ts: baseTs + 1000, DeviceID: "mb-sensor-01", Direction: DirDown, RegAddr: "0x0010", Value: "255", Result: "ok", Message: "写目标温度 25.5°C，回读一致"},
		{Ts: baseTs + 2000, DeviceID: "mb-sensor-01", Direction: DirDown, RegAddr: "coil:0x0020", Value: "1", Result: "ok", Message: "写线圈0 ON，回读一致"},
		{Ts: baseTs + 3000, DeviceID: "mb-sensor-01", Direction: DirUp, RegAddr: "0x0000-0x0001", Value: "260/590", Result: "error", Message: "连接超时"},
		{Ts: baseTs + 4000, DeviceID: "other-device", Direction: DirUp, RegAddr: "0x0000", Value: "999", Result: "ok", Message: "其他设备"},
	}
	for _, op := range ops {
		if err := l.SaveOp(op); err != nil {
			t.Fatalf("SaveOp(%+v) 失败: %v", op, err)
		}
	}
}

// TestSaveOpAndListAll SaveOp 落盘后可全量查询（含 ID 自增与方向/地址字段）。
func TestSaveOpAndListAll(t *testing.T) {
	l, _ := newTestLedger(t)
	base := time.Now().UnixMilli() - 10000
	saveSampleOps(t, l, base)

	ops, err := l.ListOps(OpFilter{})
	if err != nil {
		t.Fatalf("ListOps 失败: %v", err)
	}
	if len(ops) != 5 {
		t.Fatalf("记录数 = %d，期望 5", len(ops))
	}
	// 倒序返回：最新（other-device）在前
	if ops[0].DeviceID != "other-device" || ops[0].Ts != base+4000 {
		t.Errorf("首条 = %+v，期望 other-device/base+4000（倒序）", ops[0])
	}
	if ops[0].ID <= 0 {
		t.Errorf("ID 未自增分配: %d", ops[0].ID)
	}
	// 字段完整性（方向/地址/值/结果/消息）——取 coil 写记录（id 3，倒序第 3 条）
	down := ops[2]
	if down.Direction != DirDown || down.RegAddr != "coil:0x0020" ||
		down.Value != "1" || down.Result != "ok" {
		t.Errorf("下发记录字段不完整: %+v", down)
	}
}

// TestListOpsFilterByDevice 按设备名过滤。
func TestListOpsFilterByDevice(t *testing.T) {
	l, _ := newTestLedger(t)
	base := time.Now().UnixMilli() - 10000
	saveSampleOps(t, l, base)

	ops, err := l.ListOps(OpFilter{DeviceID: "mb-sensor-01"})
	if err != nil {
		t.Fatalf("ListOps 失败: %v", err)
	}
	if len(ops) != 4 {
		t.Fatalf("mb-sensor-01 记录数 = %d，期望 4", len(ops))
	}
	for _, op := range ops {
		if op.DeviceID != "mb-sensor-01" {
			t.Errorf("混入其他设备记录: %+v", op)
		}
	}
	// 不存在的设备 → 空切片（非 nil）
	none, err := l.ListOps(OpFilter{DeviceID: "nobody"})
	if err != nil {
		t.Fatalf("ListOps 失败: %v", err)
	}
	if len(none) != 0 || none == nil {
		t.Errorf("无匹配应返回空切片: %#v", none)
	}
}

// TestListOpsFilterByDirection 按方向过滤（up=上报 / down=下发）。
func TestListOpsFilterByDirection(t *testing.T) {
	l, _ := newTestLedger(t)
	base := time.Now().UnixMilli() - 10000
	saveSampleOps(t, l, base)

	ups, err := l.ListOps(OpFilter{Direction: DirUp})
	if err != nil {
		t.Fatalf("ListOps(up) 失败: %v", err)
	}
	if len(ups) != 3 {
		t.Fatalf("up 记录数 = %d，期望 3", len(ups))
	}
	downs, err := l.ListOps(OpFilter{Direction: DirDown})
	if err != nil {
		t.Fatalf("ListOps(down) 失败: %v", err)
	}
	if len(downs) != 2 {
		t.Fatalf("down 记录数 = %d，期望 2", len(downs))
	}
	for _, op := range downs {
		if op.Direction != DirDown {
			t.Errorf("混入非 down 记录: %+v", op)
		}
	}
}

// TestListOpsFilterByTimeRange 按时间范围过滤（含组合条件：设备+方向+范围）。
func TestListOpsFilterByTimeRange(t *testing.T) {
	l, _ := newTestLedger(t)
	base := time.Now().UnixMilli() - 10000
	saveSampleOps(t, l, base)

	// 时间范围 [base+1000, base+3000]
	ops, err := l.ListOps(OpFilter{StartTs: base + 1000, EndTs: base + 3000})
	if err != nil {
		t.Fatalf("ListOps 失败: %v", err)
	}
	if len(ops) != 3 {
		t.Fatalf("时间范围记录数 = %d，期望 3", len(ops))
	}
	for _, op := range ops {
		if op.Ts < base+1000 || op.Ts > base+3000 {
			t.Errorf("记录时间 %d 超出范围", op.Ts)
		}
	}

	// 组合条件：设备=mb-sensor-01 + 方向=down + 时间范围
	combined, err := l.ListOps(OpFilter{
		DeviceID:  "mb-sensor-01",
		Direction: DirDown,
		StartTs:   base,
		EndTs:     base + 2000,
	})
	if err != nil {
		t.Fatalf("ListOps(组合) 失败: %v", err)
	}
	if len(combined) != 2 {
		t.Fatalf("组合条件记录数 = %d，期望 2（0x0010 写 + coil 写）", len(combined))
	}
	for _, op := range combined {
		if op.DeviceID != "mb-sensor-01" || op.Direction != DirDown {
			t.Errorf("组合过滤失效: %+v", op)
		}
	}
}

// TestListOpsLimit 条数上限。
func TestListOpsLimit(t *testing.T) {
	l, _ := newTestLedger(t)
	base := time.Now().UnixMilli() - 10000
	saveSampleOps(t, l, base)

	ops, err := l.ListOps(OpFilter{Limit: 2})
	if err != nil {
		t.Fatalf("ListOps 失败: %v", err)
	}
	if len(ops) != 2 {
		t.Fatalf("Limit=2 返回 %d 条，期望 2", len(ops))
	}
	// 倒序：取最新 2 条
	if ops[0].Ts != base+4000 || ops[1].Ts != base+3000 {
		t.Errorf("Limit 未取最新记录: %+v", ops)
	}
}

// TestLedgerPersistsAcrossReopen 台账 SQLite 持久化：关闭 Store 后重新
// Open 同一 DB 文件，记录仍在（重启不丢——验收要求）。
func TestLedgerPersistsAcrossReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	s1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	l1, err := NewLedger(s1)
	if err != nil {
		t.Fatalf("NewLedger 失败: %v", err)
	}
	if err := l1.SaveOp(OpRecord{
		Ts: time.Now().UnixMilli(), DeviceID: "mb-sensor-01",
		Direction: DirDown, RegAddr: "0x0010", Value: "300", Result: "ok",
	}); err != nil {
		t.Fatalf("SaveOp 失败: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}

	// 模拟重启：重新打开同一 DB 文件
	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("重新 Open 失败: %v", err)
	}
	defer func() { _ = s2.Close() }()
	l2, err := NewLedger(s2)
	if err != nil {
		t.Fatalf("重新 NewLedger 失败: %v", err)
	}
	ops, err := l2.ListOps(OpFilter{})
	if err != nil {
		t.Fatalf("ListOps 失败: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("重启后记录数 = %d，期望 1（持久化）", len(ops))
	}
	if ops[0].DeviceID != "mb-sensor-01" || ops[0].Value != "300" {
		t.Errorf("重启后记录内容不符: %+v", ops[0])
	}
}

// TestCleanupOpsRemovesExpired 清理：超过 30 天的记录被删，保留期内保留。
func TestCleanupOpsRemovesExpired(t *testing.T) {
	l, _ := newTestLedger(t)
	now := time.Now()
	// 手工插入不同年代的记录（SaveOp 的 Ts 可显式指定）
	ops := []OpRecord{
		{Ts: now.AddDate(0, 0, -31).UnixMilli(), DeviceID: "old-1", Direction: DirUp, RegAddr: "0x0000", Value: "1", Result: "ok"},
		{Ts: now.AddDate(0, 0, -40).UnixMilli(), DeviceID: "old-2", Direction: DirDown, RegAddr: "0x0010", Value: "2", Result: "ok"},
		{Ts: now.AddDate(0, 0, -29).UnixMilli(), DeviceID: "keep-1", Direction: DirUp, RegAddr: "0x0000", Value: "3", Result: "ok"},
		{Ts: now.Add(-1 * time.Hour).UnixMilli(), DeviceID: "keep-2", Direction: DirDown, RegAddr: "coil:0x0020", Value: "1", Result: "ok"},
	}
	for _, op := range ops {
		if err := l.SaveOp(op); err != nil {
			t.Fatalf("SaveOp 失败: %v", err)
		}
	}

	n, err := l.CleanupOps(LedgerRetentionDays * 24 * time.Hour)
	if err != nil {
		t.Fatalf("CleanupOps 失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("清理条数 = %d，期望 2（31 天前与 40 天前的记录）", n)
	}

	remain, err := l.ListOps(OpFilter{})
	if err != nil {
		t.Fatalf("ListOps 失败: %v", err)
	}
	if len(remain) != 2 {
		t.Fatalf("清理后记录数 = %d，期望 2", len(remain))
	}
	for _, op := range remain {
		if op.DeviceID == "old-1" || op.DeviceID == "old-2" {
			t.Errorf("过期记录未被清理: %+v", op)
		}
	}
}

// TestSaveOpValidation 非法记录被拒绝（缺设备名/非法方向/缺地址）。
func TestSaveOpValidation(t *testing.T) {
	l, _ := newTestLedger(t)
	cases := []OpRecord{
		{DeviceID: "", Direction: DirUp, RegAddr: "0x0000"},
		{DeviceID: "mb-sensor-01", Direction: "sideways", RegAddr: "0x0000"},
		{DeviceID: "mb-sensor-01", Direction: DirUp, RegAddr: ""},
	}
	for i, op := range cases {
		if err := l.SaveOp(op); err == nil {
			t.Errorf("用例 %d 应返回错误: %+v", i, op)
		}
	}
}
