// CHN-20 观测锚点测试：连续 listFailed 告警阈值（EDGEFLOW_LISTFAILED_ALERT_ROUNDS）。
// 覆盖：阈值=2 两轮失败触发一次告警并重置、list 成功轮清零、阈值=0 行为
// 完全不变（no-op）、env 解析回退。既有测试见 drift_test.go / status_report_test.go
// （不改一行）。告警日志经 log.SetOutput 捕获（pkg/log 测试注入先例）。
package edged

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"edgeflow/pkg/log"
)

// countAlerts 统计缓冲中 distinct 告警（"Edged ALERT"）条数。
func countAlerts(buf *bytes.Buffer) int {
	return strings.Count(buf.String(), "Edged ALERT")
}

// TestListFailedAlertTriggersAtThreshold 阈值=2：连续 2 轮 listFailed 触发
// 一次 distinct WARN 告警并重置计数；后续失败重新累加；list 成功轮清零。
func TestListFailedAlertTriggersAtThreshold(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, 100*time.Millisecond)
	e.listFailedAlertRounds = 2

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(nil) // 还原 stderr（pkg/log：nil → stderr）

	// 第 1 轮失败：累加（streak=1），未达阈值，无告警。
	rt.SetListErr(errors.New("runtime down"))
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("reconcileOnce（失败轮 1）: %v", err)
	}
	if e.listFailedStreak != 1 {
		t.Fatalf("第 1 轮失败后 listFailedStreak = %d，期望 1", e.listFailedStreak)
	}
	if n := countAlerts(&buf); n != 0 {
		t.Fatalf("未达阈值不应有告警，实得 %d 条", n)
	}

	// 第 2 轮失败：达到阈值 → 一次告警，计数重置。
	rt.SetListErr(errors.New("runtime down"))
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("reconcileOnce（失败轮 2）: %v", err)
	}
	if e.listFailedStreak != 0 {
		t.Fatalf("触发告警后 listFailedStreak = %d，期望 0（已重置）", e.listFailedStreak)
	}
	if n := countAlerts(&buf); n != 1 {
		t.Fatalf("阈值=2 连续两轮失败应恰好 1 条告警，实得 %d 条（日志:\n%s）", n, buf.String())
	}

	// 第 3 轮仍失败：重新累加（streak=1），不告警（新一轮未达阈值）。
	rt.SetListErr(errors.New("runtime down"))
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("reconcileOnce（失败轮 3）: %v", err)
	}
	if e.listFailedStreak != 1 {
		t.Fatalf("告警后第 1 轮失败 streak = %d，期望 1", e.listFailedStreak)
	}
	if n := countAlerts(&buf); n != 1 {
		t.Fatalf("重置后未达阈值不应新增告警，实得 %d 条", n)
	}

	// list 成功轮：清零。
	rt.SetListErr(nil)
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("reconcileOnce（成功轮）: %v", err)
	}
	if e.listFailedStreak != 0 {
		t.Fatalf("list 成功后 listFailedStreak = %d，期望 0（清零）", e.listFailedStreak)
	}
	if n := countAlerts(&buf); n != 1 {
		t.Fatalf("成功清零后告警总数应仍为 1，实得 %d 条", n)
	}
}

// TestListFailedAlertDefaultOff 阈值=0（默认，env 未设置）：连续多轮
// listFailed 完全 no-op——无告警、streak 恒 0，既有行为不变。
func TestListFailedAlertDefaultOff(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, 100*time.Millisecond)
	if e.listFailedAlertRounds != 0 {
		t.Fatalf("默认 listFailedAlertRounds = %d，期望 0（行为不变）", e.listFailedAlertRounds)
	}

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(nil)

	for i := 0; i < 5; i++ {
		rt.SetListErr(errors.New("runtime down"))
		if err := e.reconcileOnce(); err != nil {
			t.Fatalf("reconcileOnce（失败轮 %d）: %v", i+1, err)
		}
	}
	if e.listFailedStreak != 0 {
		t.Fatalf("阈值=0 时 streak = %d，期望恒 0（no-op）", e.listFailedStreak)
	}
	if n := countAlerts(&buf); n != 0 {
		t.Fatalf("阈值=0 不应产生告警，实得 %d 条", n)
	}
}

// TestListFailedAlertEnvParsing 验证 env 解析回退模式（沿 resources.go：
// 未设置/非法/负值/0 → 0；正数 → 生效）。
func TestListFailedAlertEnvParsing(t *testing.T) {
	t.Setenv(EnvListFailedAlertRounds, "3")
	if got := defaultListFailedAlertRounds(); got != 3 {
		t.Fatalf("env=3 → %d，期望 3", got)
	}
	t.Setenv(EnvListFailedAlertRounds, "abc")
	if got := defaultListFailedAlertRounds(); got != 0 {
		t.Fatalf("env=abc → %d，期望 0（非法回退）", got)
	}
	t.Setenv(EnvListFailedAlertRounds, "-1")
	if got := defaultListFailedAlertRounds(); got != 0 {
		t.Fatalf("env=-1 → %d，期望 0（负值回退）", got)
	}
	t.Setenv(EnvListFailedAlertRounds, "0")
	if got := defaultListFailedAlertRounds(); got != 0 {
		t.Fatalf("env=0 → %d，期望 0（行为不变）", got)
	}
	t.Setenv(EnvListFailedAlertRounds, "")
	if got := defaultListFailedAlertRounds(); got != 0 {
		t.Fatalf("env 未设置 → %d，期望 0", got)
	}
}
