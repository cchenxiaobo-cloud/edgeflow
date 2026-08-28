package opcua

import (
	"testing"
	"time"

	opcuapkg "edgeflow/pkg/opcua"
)

// TestSubscriptionLoopExitsOnClosedChannel：pubCh 关闭后 subscriptionLoop
// 必须退出且不误触发重建（subOn=false，Stop 场景）（PRT-18）。
func TestSubscriptionLoopExitsOnClosedChannel(t *testing.T) {
	m := &OPCUAMapper{deviceName: "exit-test", subValues: make(map[string]float64)}
	ch := make(chan opcuapkg.PublishResult, 1)
	done := make(chan struct{})
	go func() {
		m.subscriptionLoop(ch)
		close(done)
	}()
	close(ch) // 模拟 client 泵退出/Close 关闭出口
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pubCh 关闭后 subscriptionLoop 未退出（PRT-18）")
	}
}
