// CHN-19 观测锚点测试：续约队列水位 / 丢弃计数指标渲染（metrics.go）。
// 照 LeaseRenewalFailures / LeaseHBRebuilds（L12/L12+）既有模式：
// 注入输出 / 0 值可见 / 未注入不输出。既有测试见 metrics_test.go（不改一行）。
package metrics

import (
	"strings"
	"testing"
)

// TestLeaseRenewQueueMetrics 验证 depth（gauge）与 dropped（counter）两个
// 新指标的渲染行为。
func TestLeaseRenewQueueMetrics(t *testing.T) {
	t.Run("injected", func(t *testing.T) {
		m := New(Providers{
			LeaseRenewQueueDepth:   func() int { return 7 },
			LeaseRenewQueueDropped: func() uint64 { return 42 },
		})
		text := string(m.render())
		// depth：gauge 元信息 + 取值行
		if !strings.Contains(text, "# HELP edgeflow_cloudcore_lease_renew_queue_depth") ||
			!strings.Contains(text, "# TYPE edgeflow_cloudcore_lease_renew_queue_depth gauge") {
			t.Errorf("缺少队列水位指标元信息:\n%s", text)
		}
		if !strings.Contains(text, "edgeflow_cloudcore_lease_renew_queue_depth 7") {
			t.Errorf("缺少队列水位取值行:\n%s", text)
		}
		// dropped：counter 元信息 + 取值行
		if !strings.Contains(text, "# HELP edgeflow_cloudcore_lease_renew_queue_dropped_total") ||
			!strings.Contains(text, "# TYPE edgeflow_cloudcore_lease_renew_queue_dropped_total counter") {
			t.Errorf("缺少队列丢弃指标元信息:\n%s", text)
		}
		if !strings.Contains(text, "edgeflow_cloudcore_lease_renew_queue_dropped_total 42") {
			t.Errorf("缺少队列丢弃取值行:\n%s", text)
		}
	})
	t.Run("zero-value-visible", func(t *testing.T) {
		m := New(Providers{
			LeaseRenewQueueDepth:   func() int { return 0 },
			LeaseRenewQueueDropped: func() uint64 { return 0 },
		})
		text := string(m.render())
		if !strings.Contains(text, "edgeflow_cloudcore_lease_renew_queue_depth 0") {
			t.Error("水位 0 值也应输出（监控面板基线）")
		}
		if !strings.Contains(text, "edgeflow_cloudcore_lease_renew_queue_dropped_total 0") {
			t.Error("丢弃 0 值也应输出（面板可基于增长率告警）")
		}
	})
	t.Run("nil-provider-omitted", func(t *testing.T) {
		m := New(Providers{})
		text := string(m.render())
		if strings.Contains(text, "lease_renew_queue_depth") {
			t.Error("Provider 为 nil 时不应输出水位指标（非外部模式基线）")
		}
		if strings.Contains(text, "lease_renew_queue_dropped_total") {
			t.Error("Provider 为 nil 时不应输出丢弃指标（非外部模式基线）")
		}
	})
}
