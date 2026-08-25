package modelrelease

// 领跑锁单测（设计 §5.4 + 裁决 D5 语义；WBS-9）：
//   - EtcdLockKV.TryAcquire：新键 → 获取成功且写值（值内编码 expiresAt）；
//     持有者活跃（expiresAt 新鲜）→ false 不推进；过期/值损坏/缺失 →
//     接管（grant-per-claim 重绑租约）；后端错误 → error；
//   - RefreshPeriod（D5）：refresh = max(5s, TTL/3)，TTL ≥ 3×refresh
//     恒成立（TTL=15s → 5s 自洽；默认 60s → 20s 原节奏）；
//   - NoopLockKV 恒成功（embed/内存单实例天然领跑者）。

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func newLockFixture(t *testing.T) (*EtcdLockKV, *fakeKV, *fakeClock) {
	t.Helper()
	clk := newFakeClock(time.UnixMilli(1787000000000))
	kv := newFakeKV()
	l := NewEtcdLockKV(kv, clk.Now)
	if l == nil {
		t.Fatal("NewEtcdLockKV(nil backend) 应返回 nil")
	}
	return l, kv, clk
}

func TestTryAcquire_新键获取(t *testing.T) {
	l, kv, clk := newLockFixture(t)
	ok, err := l.TryAcquire(context.Background(), "/lock/rel-1", []byte("rel-1"), 60*time.Second)
	if err != nil || !ok {
		t.Fatalf("新键应获取成功（ok=%v err=%v）", ok, err)
	}
	if kv.leaseCount != 1 {
		t.Fatalf("应执行 1 次 GrantHeartbeatLease，got %d", kv.leaseCount)
	}
	v, err := kv.Get(context.Background(), "/lock/rel-1")
	if err != nil || v == nil {
		t.Fatalf("锁键应已写入: %v", err)
	}
	var lv lockValue
	if err := json.Unmarshal(v, &lv); err != nil {
		t.Fatalf("锁值反序列化失败: %v", err)
	}
	if lv.ReleaseID != "rel-1" {
		t.Fatalf("锁值 releaseID = %q", lv.ReleaseID)
	}
	wantExpiry := clk.t.UnixMilli() + 60000
	if lv.ExpiresAt != wantExpiry {
		t.Fatalf("expiresAt = %d, want %d（授予时刻 + TTL）", lv.ExpiresAt, wantExpiry)
	}
}

func TestTryAcquire_持有者活跃(t *testing.T) {
	l, kv, clk := newLockFixture(t)
	if ok, _ := l.TryAcquire(context.Background(), "/lock/rel-1", []byte("rel-1"), 60*time.Second); !ok {
		t.Fatal("首次获取应成功")
	}
	// 持有者刷新中（expiresAt 新鲜）→ 他副本不推进
	ok, err := l.TryAcquire(context.Background(), "/lock/rel-1", []byte("rel-2"), 60*time.Second)
	if err != nil || ok {
		t.Fatalf("持有者活跃时他副本应拿不到锁（ok=%v err=%v）", ok, err)
	}
	// 原持有者刷新的值仍在（未被覆盖）——释放校验：他副本请求不重绑租约
	if kv.leaseCount != 1 {
		t.Fatalf("失败获取不应触发 GrantHeartbeatLease，got %d", kv.leaseCount)
	}
	// 持有者自身刷新（expiresAt 重写）→ 仍成功
	clk.Advance(20 * time.Second)
	if ok, _ := l.TryAcquire(context.Background(), "/lock/rel-1", []byte("rel-1"), 60*time.Second); !ok {
		t.Fatal("持有者刷新应成功")
	}
	if kv.leaseCount != 2 {
		t.Fatalf("刷新应再 Grant 一次，got %d", kv.leaseCount)
	}
}

func TestTryAcquire_过期接管(t *testing.T) {
	l, _, clk := newLockFixture(t)
	if ok, _ := l.TryAcquire(context.Background(), "/lock/rel-1", []byte("rel-1"), 60*time.Second); !ok {
		t.Fatal("首次获取应成功")
	}
	// 领跑者崩溃：租约到期（expiresAt 过期）→ 他副本接管（grant-per-claim）
	clk.Advance(61 * time.Second)
	ok, err := l.TryAcquire(context.Background(), "/lock/rel-1", []byte("rel-2"), 60*time.Second)
	if err != nil || !ok {
		t.Fatalf("过期后接管应成功（ok=%v err=%v）", ok, err)
	}
	v, _ := kvGet(l, "/lock/rel-1")
	var lv lockValue
	if err := json.Unmarshal(v, &lv); err != nil {
		t.Fatal(err)
	}
	if lv.ReleaseID != "rel-2" {
		t.Fatalf("接管后锁值应为新持有者，got %q", lv.ReleaseID)
	}
}

func TestTryAcquire_值损坏视为可接管(t *testing.T) {
	l, kv, _ := newLockFixture(t)
	if err := kv.Put(context.Background(), "/lock/rel-1", []byte("garbage-not-json")); err != nil {
		t.Fatal(err)
	}
	ok, err := l.TryAcquire(context.Background(), "/lock/rel-1", []byte("rel-2"), 60*time.Second)
	if err != nil || !ok {
		t.Fatalf("值损坏应视为可接管（ok=%v err=%v）", ok, err)
	}
}

func TestTryAcquire_后端错误(t *testing.T) {
	l, kv, _ := newLockFixture(t)
	kv.failGet = errors.New("etcd unreachable")
	_, err := l.TryAcquire(context.Background(), "/lock/rel-1", []byte("rel-1"), 60*time.Second)
	if err == nil {
		t.Fatal("后端 Get 失败应返回 error（控制器本轮跳过、下轮重试）")
	}
}

// kvGet 便捷读（fakeKV.Get 的测试包装）。
func kvGet(l *EtcdLockKV, key string) ([]byte, error) {
	return l.backend.Get(context.Background(), key)
}

func TestNoopLockKV(t *testing.T) {
	n := &NoopLockKV{}
	for i := 0; i < 3; i++ {
		ok, err := n.TryAcquire(context.Background(), "/lock/whatever", []byte("x"), time.Minute)
		if err != nil || !ok {
			t.Fatalf("NoopLockKV 应恒成功（ok=%v err=%v）", ok, err)
		}
	}
}

func TestRefreshPeriod_D5(t *testing.T) {
	cases := []struct {
		ttl, want time.Duration
	}{
		{15 * time.Second, 5 * time.Second}, // 下限：TTL=3×refresh 自洽
		{20 * time.Second, time.Duration(20*time.Second) / 3},
		{60 * time.Second, 20 * time.Second}, // 默认：保持原节奏
		{120 * time.Second, 40 * time.Second},
	}
	for _, tc := range cases {
		got := RefreshPeriod(tc.ttl)
		if got != tc.want {
			t.Fatalf("RefreshPeriod(%v) = %v, want %v", tc.ttl, got, tc.want)
		}
	}
	// D5 护栏：任何合法 TTL（>=15s）恒满足 TTL >= 3×refresh
	for _, ttl := range []time.Duration{15, 16, 30, 45, 60, 90, 300} {
		ttl *= time.Second
		if r := RefreshPeriod(ttl); r*3 > ttl {
			t.Fatalf("TTL=%v refresh=%v 违反 TTL >= 3×refresh", ttl, r)
		}
	}
}
