// Pod 变更订阅测试（notify.go）：订阅→事件投递、注销、背压丢弃、关闭清理，
// 以及并发订阅/广播下的安全性（-race）。
package metamanager

import (
	"sync"
	"testing"
	"time"
)

// recvEvent 从事件通道读取一条事件（带超时，防止测试挂死）。
func recvEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("等待事件超时")
		return Event{}
	}
}

// assertNoEvent 断言在窗口期内没有事件到达（用于注销/未订阅场景）。
// 通道被关闭（注销/Close 后的关闭信号，读取 ok=false）同样视为"无新事件"。
func assertNoEvent(t *testing.T, ch <-chan Event, window time.Duration) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if ok {
			t.Fatalf("期望无事件，却收到 %+v", ev)
		}
		// ok=false：通道已关闭，视为无新事件
	case <-time.After(window):
	}
}

// TestSubscribeReceivesUpsert 订阅后 SavePod 产生 pod.upsert 事件，
// 且字段正确（Type/Namespace/Name/Value=Pod JSON 原样）。
func TestSubscribeReceivesUpsert(t *testing.T) {
	s := newTestStore(t)
	id, ch, err := s.Subscribe(SubscribeOptions{})
	if err != nil {
		t.Fatalf("Subscribe 失败: %v", err)
	}
	defer s.Unsubscribe(id)
	if id <= 0 {
		t.Errorf("订阅 ID = %d，期望 > 0", id)
	}

	nginx := podJSON("nginx", "nginx:1.25", 1)
	if err := s.SavePod(nginx); err != nil {
		t.Fatalf("SavePod 失败: %v", err)
	}
	ev := recvEvent(t, ch)
	if ev.Type != EventPodUpsert {
		t.Errorf("事件类型 = %q，期望 %q", ev.Type, EventPodUpsert)
	}
	if ev.Namespace != "default" || ev.Name != "nginx" {
		t.Errorf("事件对象 = %s/%s，期望 default/nginx", ev.Namespace, ev.Name)
	}
	if ev.Value != nginx {
		t.Errorf("事件 Value 不是 Pod JSON 原样: %s", ev.Value)
	}

	// 缺省 namespace 的 Pod：事件应带规范化后的 "default"
	if err := s.SavePod(`{"name":"redis","image":"redis:7.0"}`); err != nil {
		t.Fatalf("SavePod(缺 namespace) 失败: %v", err)
	}
	ev = recvEvent(t, ch)
	if ev.Namespace != "default" || ev.Name != "redis" {
		t.Errorf("缺省 ns 事件对象 = %s/%s，期望 default/redis", ev.Namespace, ev.Name)
	}

	// 非法 JSON / 缺 name：SavePod 报错，不应产生任何事件
	if err := s.SavePod("not-json{"); err == nil {
		t.Error("SavePod(非法 JSON) 期望报错")
	}
	assertNoEvent(t, ch, 100*time.Millisecond)
}

// TestSubscribeReceivesDelete 订阅后 DeletePod 产生 pod.delete 事件（Value 为空串）。
func TestSubscribeReceivesDelete(t *testing.T) {
	s := newTestStore(t)
	_, ch, err := s.Subscribe(SubscribeOptions{})
	if err != nil {
		t.Fatalf("Subscribe 失败: %v", err)
	}

	// 先落盘再删除（删除事件只携带 namespace/name，不携带旧值）
	if err := s.SavePod(podJSON("nginx", "nginx:1.25", 1)); err != nil {
		t.Fatalf("SavePod 失败: %v", err)
	}
	if ev := recvEvent(t, ch); ev.Type != EventPodUpsert {
		t.Fatalf("期望先收到 upsert，实际 %+v", ev)
	}

	if err := s.DeletePod("default", "nginx"); err != nil {
		t.Fatalf("DeletePod 失败: %v", err)
	}
	ev := recvEvent(t, ch)
	if ev.Type != EventPodDelete {
		t.Errorf("事件类型 = %q，期望 %q", ev.Type, EventPodDelete)
	}
	if ev.Namespace != "default" || ev.Name != "nginx" {
		t.Errorf("事件对象 = %s/%s，期望 default/nginx", ev.Namespace, ev.Name)
	}
	if ev.Value != "" {
		t.Errorf("delete 事件 Value 应为空串，实际 %q", ev.Value)
	}

	// 删除不存在的 Pod（幂等成功）：仍广播 delete 事件（消费方按 key 幂等处理）
	if err := s.DeletePod("default", "nginx"); err != nil {
		t.Fatalf("DeletePod(不存在) 失败: %v", err)
	}
	if ev := recvEvent(t, ch); ev.Type != EventPodDelete {
		t.Errorf("幂等删除的事件类型 = %q，期望 %q", ev.Type, EventPodDelete)
	}
}

// TestUnsubscribeStopsEvents 注销后不再收到事件（新事件被丢弃）。
func TestUnsubscribeStopsEvents(t *testing.T) {
	s := newTestStore(t)
	id, ch, err := s.Subscribe(SubscribeOptions{})
	if err != nil {
		t.Fatalf("Subscribe 失败: %v", err)
	}

	// 先确认订阅在生效，再注销
	if err := s.SavePod(podJSON("nginx", "nginx:1.25", 1)); err != nil {
		t.Fatalf("SavePod 失败: %v", err)
	}
	recvEvent(t, ch)

	s.Unsubscribe(id)
	// 重复注销：幂等，不 panic
	s.Unsubscribe(id)
	// 未知 ID：幂等，不 panic
	s.Unsubscribe(99999)

	if err := s.SavePod(podJSON("redis", "redis:7.0", 1)); err != nil {
		t.Fatalf("注销后 SavePod 失败: %v", err)
	}
	if err := s.DeletePod("default", "redis"); err != nil {
		t.Fatalf("注销后 DeletePod 失败: %v", err)
	}
	assertNoEvent(t, ch, 100*time.Millisecond)

	// 注销后通道被关闭：消费方应能感知终止（ok=false）
	if _, ok := <-ch; ok {
		t.Error("注销后通道应已关闭（ok=false）")
	}
}

// TestSubscribeBufferFullDrops 验证背压策略：缓冲满时新事件被丢弃而非阻塞写路径
// （SavePod 快速连写不等待消费）。消费者随后读到的是最早未被丢弃的事件。
func TestSubscribeBufferFullDrops(t *testing.T) {
	s := newTestStore(t)
	_, ch, err := s.Subscribe(SubscribeOptions{BufferSize: 1})
	if err != nil {
		t.Fatalf("Subscribe 失败: %v", err)
	}

	// 缓冲 1：连续三条事件，只有第一条能进缓冲，后两条被丢弃
	for i := 0; i < 3; i++ {
		if err := s.SavePod(podJSONNS("default", "nginx", "nginx:1.25", 1)); err != nil {
			t.Fatalf("SavePod 失败: %v", err)
		}
	}
	ev := recvEvent(t, ch)
	if ev.Name != "nginx" {
		t.Errorf("缓冲中应是第一条事件（nginx），实际 %+v", ev)
	}
	// 缓冲已空：后续事件正常投递
	if err := s.SavePod(podJSON("redis", "redis:7.0", 1)); err != nil {
		t.Fatalf("SavePod(redis) 失败: %v", err)
	}
	ev = recvEvent(t, ch)
	if ev.Name != "redis" {
		t.Errorf("缓冲清空后应收到 redis 事件，实际 %+v", ev)
	}
}

// TestStoreCloseClosesSubscriberChannels Store.Close 关闭全部订阅者通道：
// 消费方 goroutine 可据此退出，不永久阻塞。
func TestStoreCloseClosesSubscriberChannels(t *testing.T) {
	s := newTestStore(t)
	_, ch, err := s.Subscribe(SubscribeOptions{})
	if err != nil {
		t.Fatalf("Subscribe 失败: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	if _, ok := <-ch; ok {
		t.Error("Close 后订阅通道应已关闭（ok=false）")
	}
}

// TestConcurrentSubscribeNotify 并发订阅/广播/注销（配合 -race）：
// 多个订阅者 + 并发 SavePod/DeletePod，验证订阅表加锁与通道投递
// 在并发下安全（无数据竞争、无发送到已关闭通道的 panic）。
// 背压策略允许丢事件，因此不断言事件数量。
func TestConcurrentSubscribeNotify(t *testing.T) {
	s := newTestStore(t)

	const (
		subs    = 4
		writers = 2
		rounds  = 60
	)
	chans := make([]<-chan Event, 0, subs)
	for i := 0; i < subs; i++ {
		_, ch, err := s.Subscribe(SubscribeOptions{BufferSize: 16})
		if err != nil {
			t.Fatalf("Subscribe 失败: %v", err)
		}
		chans = append(chans, ch)
	}

	// 消费者：并发 drain（丢事件是允许的，消费只是触发通道读写）
	var readers sync.WaitGroup
	for _, ch := range chans {
		readers.Add(1)
		go func(ch <-chan Event) {
			defer readers.Done()
			for {
				if _, ok := <-ch; !ok {
					return // 通道关闭：退出
				}
			}
		}(ch)
	}

	// 写者：并发 SavePod/DeletePod
	var writersWG sync.WaitGroup
	for w := 0; w < writers; w++ {
		writersWG.Add(1)
		go func() {
			defer writersWG.Done()
			for i := 0; i < rounds; i++ {
				_ = s.SavePod(podJSONNS("default", "nginx", "nginx:1.25", 1))
				_ = s.DeletePod("default", "nginx")
			}
		}()
	}
	writersWG.Wait()

	// 全部注销：关闭通道，读者退出（先取 ID 快照再逐个注销，避免锁内重复加锁）
	s.subMu.Lock()
	ids := make([]int, 0, len(s.subscribers))
	for id := range s.subscribers {
		ids = append(ids, id)
	}
	s.subMu.Unlock()
	for _, id := range ids {
		s.Unsubscribe(id)
	}
	readers.Wait()
}
