// 幂等去重键持久化单元测试（CHN-03 / v0.22.0）。
//
// 覆盖面（对应 T-07 验收）：
//   - 写入后 IsProcessed=true（基本去重语义）；
//   - 重启语义：同一 SQLite 文件重新 Open + NewDedupStore → 仍 true
//     （重启去重窗口不丢，云端重试同 ID 被去重的持久层基础）；
//   - TTL 过期：expires_at 已过 → IsProcessed=false（过期键视为未处理），
//     CleanupExpired 回收过期行；
//   - 容量淘汰：灌满超过 DedupMaxKeys 的键 → 最旧（expires_at 最小）被淘汰；
//   - Count / DropAll（测试与诊断辅助）。
//
// 隔离说明：每个用例 t.TempDir() 独立 SQLite 文件，互不干扰；
// 不修改 DedupStore 实现（容量/TTL 为常量，过期与淘汰用「直改 expires_at /
// 灌满数据」方式构造场景）。
package metamanager

import (
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// newDedupTestStore 在临时目录打开 Store 并构造 DedupStore（注册清理）。
func newDedupTestStore(t *testing.T) (*Store, *DedupStore) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "edgeflow.db"))
	if err != nil {
		t.Fatalf("Open 测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	d, err := NewDedupStore(s)
	if err != nil {
		t.Fatalf("NewDedupStore 失败: %v", err)
	}
	return s, d
}

// TestDedupMarkThenIsProcessed 验证基本去重语义：MarkProcessed 后同 ID
// IsProcessed=true，未写入 ID 为 false。
func TestDedupMarkThenIsProcessed(t *testing.T) {
	_, d := newDedupTestStore(t)

	if d.IsProcessed("msg-1") {
		t.Fatal("未写入的 msg-1 不应被视为已处理")
	}
	if !d.MarkProcessed("msg-1") {
		t.Fatal("MarkProcessed(msg-1) 应成功持久化")
	}
	if !d.IsProcessed("msg-1") {
		t.Fatal("MarkProcessed 后 msg-1 应被去重命中（IsProcessed=true）")
	}

	// 重复写入幂等（INSERT ON CONFLICT 展期），不报错且仍命中
	if !d.MarkProcessed("msg-1") || !d.IsProcessed("msg-1") {
		t.Fatal("重复 MarkProcessed 应幂等且保持命中")
	}

	// 空 ID 防御：不写入也不命中
	if d.MarkProcessed("") {
		t.Error("空 ID MarkProcessed 应返回 false")
	}
	if d.IsProcessed("") {
		t.Error("空 ID IsProcessed 应返回 false")
	}
}

// TestDedupRestartPersistence 验证重启语义（CHN-03 核心）：同一 SQLite 文件
// 先写入去重键 → 关闭（模拟进程退出）→ 重新 Open + NewDedupStore（模拟重启）
// → IsProcessed 仍为 true，云端重试同 ID 被去重。
func TestDedupRestartPersistence(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "edgeflow.db")

	// 第一次「进程」：写入去重键后关闭
	s1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("第一次 Open 失败: %v", err)
	}
	d1, err := NewDedupStore(s1)
	if err != nil {
		s1.Close()
		t.Fatalf("第一次 NewDedupStore 失败: %v", err)
	}
	if !d1.MarkProcessed("msg-restart-1") {
		s1.Close()
		t.Fatal("MarkProcessed 应成功")
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("关闭第一次 Store 失败: %v", err)
	}

	// 第二次「进程」（重启）：同一文件重新打开，去重窗口应保留
	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("重启后 Open 失败: %v", err)
	}
	defer s2.Close()
	d2, err := NewDedupStore(s2)
	if err != nil {
		t.Fatalf("重启后 NewDedupStore 失败: %v", err)
	}
	if !d2.IsProcessed("msg-restart-1") {
		t.Fatal("重启后同 ID 应仍被去重（IsProcessed=true）——去重窗口必须跨重启保留")
	}
	// 新 ID 不受影响
	if d2.IsProcessed("msg-restart-other") {
		t.Fatal("重启后未写入的新 ID 不应被去重")
	}
}

// TestDedupTTLExpiry 验证 TTL 过期语义：expires_at 已过 → IsProcessed=false
// （视为未处理，云端重试可重新执行）；CleanupExpired 删除过期行回收空间。
func TestDedupTTLExpiry(t *testing.T) {
	s, d := newDedupTestStore(t)

	if !d.MarkProcessed("msg-ttl") {
		t.Fatal("MarkProcessed 应成功")
	}
	if !d.IsProcessed("msg-ttl") {
		t.Fatal("TTL 内应命中去重")
	}

	// 直改 expires_at 为过去时间（模拟 TTL 已过；TTL 为常量，不注时钟）：
	// expires_at=1ms ≈ 1970，必然已过期。
	if _, err := s.db.Exec(`UPDATE dedup_keys SET expires_at = ? WHERE msg_id = ?`,
		1, "msg-ttl"); err != nil {
		t.Fatalf("直改 expires_at 失败: %v", err)
	}
	if d.IsProcessed("msg-ttl") {
		t.Fatal("expires_at 已过的键应视为未处理（IsProcessed=false）")
	}

	// 过期键仍占行（未清理），CleanupExpired 后被回收
	n, err := d.Count()
	if err != nil || n != 1 {
		t.Fatalf("清理前应仍有 1 行（含过期），got n=%d err=%v", n, err)
	}
	removed, err := d.CleanupExpired()
	if err != nil {
		t.Fatalf("CleanupExpired 失败: %v", err)
	}
	if removed != 1 {
		t.Fatalf("CleanupExpired 应删除 1 条过期键，got %d", removed)
	}
	if n, _ = d.Count(); n != 0 {
		t.Fatalf("清理后应 0 行，got %d", n)
	}
}

// TestDedupEvictOverLimit 验证容量淘汰：键数超过 DedupMaxKeys 时，
// 按 expires_at 升序淘汰最旧一批（LRU-by-expiry：最旧先过期先淘汰）。
// 构造方式：直接 SQL 灌入 DedupMaxKeys+5 条键（绕开 MarkProcessed 的写入
// 计数——否则第 200 次写入会触发异步维护删 500 条，与断言产生竞争），
// expires_at 按写入顺序递增；触发淘汰后最旧一批（含前 5 条）必须被删除、
// 最新一批必须保留。
func TestDedupEvictOverLimit(t *testing.T) {
	s, d := newDedupTestStore(t)

	total := DedupMaxKeys + 5
	// expires_at 以当前时间为基准递增（若用 1970 起的小整数，会被
	// NewDedupStore 启动时的过期清理误删——它们是真实过期键）。
	base := time.Now().Add(DedupTTL).UnixMilli()
	for i := 0; i < total; i++ {
		if _, err := s.db.Exec(
			`INSERT INTO dedup_keys(msg_id, expires_at) VALUES(?, ?)`,
			"msg-evict-"+itoa(i), base+int64(i)); err != nil {
			t.Fatalf("灌入第 %d 条失败: %v", i, err)
		}
	}

	removed, err := d.EvictOverLimit()
	if err != nil {
		t.Fatalf("EvictOverLimit 失败: %v", err)
	}
	if removed != DedupEvictBatch {
		t.Fatalf("超限 5 条时应淘汰一批 %d 条，got %d", DedupEvictBatch, removed)
	}

	// 最旧 5 条必须已被淘汰（不再占用去重语义）
	for i := 0; i < 5; i++ {
		if d.IsProcessed("msg-evict-" + itoa(i)) {
			t.Errorf("最旧键 msg-evict-%d 应已被容量淘汰", i)
		}
	}
	// 最新写入的键必须保留
	for i := total - 5; i < total; i++ {
		if !d.IsProcessed("msg-evict-" + itoa(i)) {
			t.Errorf("最新键 msg-evict-%d 不应被淘汰", i)
		}
	}
	// 未超限调用应为 no-op（删除后余量 = total-500 < DedupMaxKeys）
	if n, err := d.EvictOverLimit(); err != nil || n != 0 {
		t.Fatalf("未超限时应返回 (0, nil)，got (%d, %v)", n, err)
	}
}

// TestDedupCountAndDropAll 验证 Count 与 DropAll（测试/诊断辅助语义）。
func TestDedupCountAndDropAll(t *testing.T) {
	_, d := newDedupTestStore(t)

	if n, err := d.Count(); err != nil || n != 0 {
		t.Fatalf("初始应为 0 条，got (%d, %v)", n, err)
	}
	for i := 0; i < 3; i++ {
		d.MarkProcessed("msg-drop-" + itoa(i))
	}
	if n, err := d.Count(); err != nil || n != 3 {
		t.Fatalf("写入 3 条后应 3 条，got (%d, %v)", n, err)
	}
	if err := d.DropAll(); err != nil {
		t.Fatalf("DropAll 失败: %v", err)
	}
	if n, err := d.Count(); err != nil || n != 0 {
		t.Fatalf("DropAll 后应 0 条，got (%d, %v)", n, err)
	}
	if d.IsProcessed("msg-drop-0") {
		t.Fatal("DropAll 后不应再命中任何键")
	}
}

// TestDedupNilStore 验证哨兵错误：nil Store 构造返回 ErrDedupStoreNil。
func TestDedupNilStore(t *testing.T) {
	_, err := NewDedupStore(nil)
	if !errors.Is(err, ErrDedupStoreNil) {
		t.Fatalf("NewDedupStore(nil) 应返回 ErrDedupStoreNil，got %v", err)
	}
}

// itoa 是测试内的极简十进制转换（统一包装，便于用例内可读调用）。
func itoa(i int) string {
	return strconv.Itoa(i)
}
