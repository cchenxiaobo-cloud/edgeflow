package edgehub

// v0.23.0 P2 新增用例（CHN-17，审计建议项）：nextBackoff 退避封顶序列的
// 独立边界契约测试（此前仅 client_test 经重连流程间接覆盖）。
// 实现零改动（client.go nextBackoff）：当前值翻倍、封顶 max。
// 契约：默认参数（Base=1s, Max=60s）下序列 1s→2s→4s→8s→16s→32s→60s→60s…；
// current ≥ max 时钳制为 max；current=0 时结果恒 0（0 翻倍仍为 0，永不增长
// ——调用方 runLoop 以 BackoffBase 起步并保证 Base>0（Options 归一化），0 只
// 出现在显式传参/调用点误用场景，此处将行为文档化为测试契约）。
// 本文件仅新增，不改动既有 *_test.go。

import (
	"testing"
	"time"
)

// TestNextBackoffCapSequence 表驱动验证封顶序列：1s→2s→…→60s→60s（封顶后恒 60s）。
func TestNextBackoffCapSequence(t *testing.T) {
	max := DefaultBackoffMax // 60s
	current := DefaultBackoffBase

	want := []time.Duration{
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		32 * time.Second,
		60 * time.Second, // 64s > 60s → 钳制 60s
		60 * time.Second, // 已封顶，恒 60s
		60 * time.Second,
		60 * time.Second,
	}
	for i, w := range want {
		got := nextBackoff(current, max)
		if got != w {
			t.Fatalf("第 %d 次推进: nextBackoff(%s, 60s) = %s，期望 %s", i+1, current, got, w)
		}
		current = got
	}
	// 起点自证：序列必须从 Base=1s 开始。
	if DefaultBackoffBase != 1*time.Second {
		t.Fatalf("DefaultBackoffBase = %s，契约起点应为 1s", DefaultBackoffBase)
	}
}

// TestNextBackoffClamp 当 current ≥ max 时结果必须钳制为 max（不无限增长）。
func TestNextBackoffClamp(t *testing.T) {
	cases := []struct {
		name    string
		current time.Duration
		max     time.Duration
		want    time.Duration
	}{
		{"current等于max", 60 * time.Second, 60 * time.Second, 60 * time.Second},
		{"current超过max", 90 * time.Second, 60 * time.Second, 60 * time.Second},
		{"远超max", 10 * time.Minute, 5 * time.Second, 5 * time.Second},
		{"max为1s边界", 2 * time.Second, 1 * time.Second, 1 * time.Second},
		{"max为0退化", 5 * time.Second, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextBackoff(tc.current, tc.max); got != tc.want {
				t.Errorf("nextBackoff(%s, %s) = %s，期望 %s（钳制）", tc.current, tc.max, got, tc.want)
			}
		})
	}
}

// TestNextBackoffZeroBase 文档化 current=0 行为：0×2=0 恒为 0（永不增长）。
// 之所以安全：runLoop 以 opts.BackoffBase 起步，New/Options 归一化保证
// Base<=0 时回落 DefaultBackoffBase=1s（见 client.go Options 应用处），
// 正常运行期 current 不会为 0。本用例钉死该前提被破坏时的可观测行为。
func TestNextBackoffZeroBase(t *testing.T) {
	for i := 0; i < 5; i++ {
		if got := nextBackoff(0, DefaultBackoffMax); got != 0 {
			t.Fatalf("current=0 时第 %d 次推进 = %s，期望恒为 0（0 翻倍不增长）", i+1, got)
		}
	}
	// 默认参数下正常序列不经过 0：Base=1s 起步恒增长至 60s 封顶。
	if got := nextBackoff(DefaultBackoffBase, DefaultBackoffMax); got != 2*time.Second {
		t.Errorf("nextBackoff(1s, 60s) = %s，期望 2s", got)
	}
}
