// CHN-14 观测锚点测试：Pod 变更事件丢弃计数（notify.go notify 的
// select default 分支静默丢弃 → Store.droppedEvents 计数 + DroppedEvents()
// 访问器暴露）。仅测新增观测面，既有背压行为见 notify_test.go（不改一行）。
package metamanager

import (
	"testing"
	"time"
)

// TestDroppedEventsCountsWhenSubscriberFull 订阅者通道缓冲塞满后，
// 触发广播（SavePod）时溢出事件被丢弃且计数自增；成功投递不计数。
func TestDroppedEventsCountsWhenSubscriberFull(t *testing.T) {
	s := newTestStore(t)
	if got := s.DroppedEvents(); got != 0 {
		t.Fatalf("初始 DroppedEvents = %d，期望 0", got)
	}

	// 缓冲 = 2 的订阅者：恰好能吃下前 2 个事件，之后开始丢弃。
	id, ch, err := s.Subscribe(SubscribeOptions{BufferSize: 2})
	if err != nil {
		t.Fatalf("Subscribe 失败: %v", err)
	}
	defer s.Unsubscribe(id)

	// 前 2 个事件成功投递（填满缓冲），不产生丢弃计数。
	for i := 0; i < 2; i++ {
		if err := s.SavePod(podJSON("full-pod", "nginx:1.25", i)); err != nil {
			t.Fatalf("SavePod #%d 失败: %v", i, err)
		}
	}
	if got := s.DroppedEvents(); got != 0 {
		t.Fatalf("缓冲未满时 DroppedEvents = %d，期望 0", got)
	}

	// 第 3 个事件溢出：被丢弃，计数 = 1。
	if err := s.SavePod(podJSON("overflow-pod", "nginx:1.25", 2)); err != nil {
		t.Fatalf("SavePod（溢出）失败: %v", err)
	}
	if got := s.DroppedEvents(); got != 1 {
		t.Fatalf("缓冲满后 DroppedEvents = %d，期望 1", got)
	}

	// 再丢 2 个：计数累计到 3（累计值，不清零）。
	for i := 0; i < 2; i++ {
		if err := s.SavePod(podJSON("overflow-pod", "nginx:1.25", 3+i)); err != nil {
			t.Fatalf("SavePod（溢出 #%d）失败: %v", i, err)
		}
	}
	if got := s.DroppedEvents(); got != 3 {
		t.Fatalf("两次溢出后 DroppedEvents = %d，期望 3", got)
	}

	// 被丢弃的事件确实没进订阅者通道：通道里只有最初的 2 条。
	if len(ch) != 2 {
		t.Fatalf("订阅者通道长度 = %d，期望 2（仅成功投递的前两条）", len(ch))
	}

	// 注销订阅者后广播不再触达任何人，也不再产生丢弃。
	s.Unsubscribe(id)
	if err := s.SavePod(podJSON("after-unsub", "nginx:1.25", 9)); err != nil {
		t.Fatalf("SavePod（注销后）失败: %v", err)
	}
	if got := s.DroppedEvents(); got != 3 {
		t.Fatalf("注销后 DroppedEvents = %d，期望仍为 3", got)
	}
}

// TestDroppedEventsMultipleSubscribers 多订阅者场景：只有缓冲满的订阅者
// 才计入丢弃，其余订阅者正常收到事件（丢弃按订阅者粒度计数）。
func TestDroppedEventsMultipleSubscribers(t *testing.T) {
	s := newTestStore(t)

	// 订阅者 A：缓冲 1；订阅者 B：缓冲 100（不会满）。
	idA, chA, err := s.Subscribe(SubscribeOptions{BufferSize: 1})
	if err != nil {
		t.Fatalf("Subscribe A 失败: %v", err)
	}
	defer s.Unsubscribe(idA)
	idB, chB, err := s.Subscribe(SubscribeOptions{BufferSize: 100})
	if err != nil {
		t.Fatalf("Subscribe B 失败: %v", err)
	}
	defer s.Unsubscribe(idB)

	// 3 个事件：A 收到第 1 个后溢出 2 个，B 全部收到。
	for i := 0; i < 3; i++ {
		if err := s.SavePod(podJSON("multi-pod", "nginx:1.25", i)); err != nil {
			t.Fatalf("SavePod #%d 失败: %v", i, err)
		}
	}
	if got := s.DroppedEvents(); got != 2 {
		t.Fatalf("DroppedEvents = %d，期望 2（仅订阅者 A 溢出的两条）", got)
	}
	if len(chB) != 3 {
		t.Fatalf("订阅者 B 通道长度 = %d，期望 3（不受 A 丢弃影响）", len(chB))
	}

	// 消费 A 的通道后再次广播：A 能再接收，丢弃不再增加。
	<-chA
	if err := s.SavePod(podJSON("multi-pod", "nginx:1.25", 3)); err != nil {
		t.Fatalf("SavePod（A 腾空后）失败: %v", err)
	}
	if got := s.DroppedEvents(); got != 2 {
		t.Fatalf("A 腾空后 DroppedEvents = %d，期望仍为 2", got)
	}
	select {
	case ev := <-chA:
		if ev.Name != "multi-pod" {
			t.Fatalf("A 收到意外事件: %+v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("A 腾空后等待事件超时")
	}
}
