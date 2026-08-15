package main

// keadm upgrade 灰度发布分批测试（WBS 10.2）：
// 单节点 upgrade 的 --batch-size/--pause-between 参数校验；
// 分批逻辑（runUpgradeBatches）用注入 fake 执行器/暂停器验证
// 分组、批间暂停、fail-fast 中止与清单报告，避免真实升级与等待。

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeUpgradeExecutor 返回记录调用顺序的执行器：results 中标记的节点返回
// 指定退出码，其余返回成功。
func fakeUpgradeExecutor(results map[string]int) (upgradeExecutor, *[]string) {
	var calls []string
	exec := func(opts batchOptions, line string, stdout, stderr io.Writer) int {
		calls = append(calls, line)
		if code, ok := results[line]; ok {
			return code
		}
		return exitOK
	}
	return exec, &calls
}

// fakeSleeper 记录每次暂停时长（不真实等待）。
func fakeSleeper(durations *[]time.Duration) batchSleeper {
	return func(d time.Duration) { *durations = append(*durations, d) }
}

// TestUpgradeBatchFlagsValidation 验证单节点 upgrade 的分批参数校验：
// batch-size < 1 / 非法时长 / 负时长 → exit 2；合法值 → 正常升级（无分批效果）。
func TestUpgradeBatchFlagsValidation(t *testing.T) {
	out, _ := tmpOut(t)
	joinOnce(t, out)

	bad := [][]string{
		{"upgrade", "--version=v0.2.0", "--batch-size=0", "--output-dir=" + out},
		{"upgrade", "--version=v0.2.0", "--batch-size=-3", "--output-dir=" + out},
		{"upgrade", "--version=v0.2.0", "--pause-between=abc", "--output-dir=" + out},
		{"upgrade", "--version=v0.2.0", "--pause-between=-30s", "--output-dir=" + out},
	}
	for i, args := range bad {
		code, _, stderr := runCapture(args, "")
		if code != 2 {
			t.Errorf("用例 %d (%v): 退出码 = %d，期望 2；stderr=%s", i, args, code, stderr)
		}
	}

	// 合法值：单节点升级接受但无分批效果（正常完成）。
	code, stdout, stderr := runCapture([]string{
		"upgrade", "--version=v0.2.0", "--batch-size=5", "--pause-between=30s", "--output-dir=" + out,
	}, "")
	if code != 0 {
		t.Fatalf("合法分批参数应正常升级，退出码 = %d；stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "v0.1.0 -> v0.2.0") {
		t.Errorf("升级未生效: %q", stdout)
	}
}

// TestRunUpgradeBatches_GroupingAndPause 验证分批逻辑成功路径：
// 5 节点 batch-size=2 → 3 批（2+2+1），批间暂停 2 次，全部成功。
func TestRunUpgradeBatches_GroupingAndPause(t *testing.T) {
	lines := []string{"d1", "d2", "d3", "d4", "d5"}
	exec, calls := fakeUpgradeExecutor(nil)
	var pauses []time.Duration
	opts := batchOptions{BatchSize: 2, PauseBetween: 30 * time.Second}
	var out, errBuf bytes.Buffer

	ok, failed, succeeded, failedNodes, aborted := runUpgradeBatches(
		opts, lines, exec, fakeSleeper(&pauses), &out, &errBuf)

	if ok != 5 || failed != 0 {
		t.Errorf("ok=%d failed=%d，期望 5/0", ok, failed)
	}
	if len(succeeded) != 5 || len(failedNodes) != 0 || len(aborted) != 0 {
		t.Errorf("清单错误: succeeded=%v failed=%v aborted=%v", succeeded, failedNodes, aborted)
	}
	// 执行顺序 = 清单顺序；暂停发生在批次 1、2 之后（各 30s）。
	wantCalls := []string{"d1", "d2", "d3", "d4", "d5"}
	for i := range wantCalls {
		if (*calls)[i] != wantCalls[i] {
			t.Errorf("第 %d 次执行 = %q，期望 %q（顺序错误）", i, (*calls)[i], wantCalls[i])
		}
	}
	if len(pauses) != 2 || pauses[0] != 30*time.Second || pauses[1] != 30*time.Second {
		t.Errorf("暂停应为 [30s 30s]，实际 %v", pauses)
	}
	// 输出含批次信息。
	if !strings.Contains(out.String(), "批次 1/3") || !strings.Contains(out.String(), "暂停 30s") {
		t.Errorf("输出应含批次与暂停信息: %q", out.String())
	}
}

// TestRunUpgradeBatches_FailFastFirstBatch 验证首批失败即中止：
// 4 节点 batch-size=2，第 2 节点失败 → 只执行 2 个节点，无暂停，
// 失败 1 / 未执行 2。
func TestRunUpgradeBatches_FailFastFirstBatch(t *testing.T) {
	lines := []string{"d1", "d2", "d3", "d4"}
	exec, calls := fakeUpgradeExecutor(map[string]int{"d2": exitRuntime})
	var pauses []time.Duration
	opts := batchOptions{BatchSize: 2, PauseBetween: 30 * time.Second}
	var out, errBuf bytes.Buffer

	ok, failed, succeeded, failedNodes, aborted := runUpgradeBatches(
		opts, lines, exec, fakeSleeper(&pauses), &out, &errBuf)

	if ok != 1 || failed != 1 {
		t.Errorf("ok=%d failed=%d，期望 1/1", ok, failed)
	}
	if len(*calls) != 2 {
		t.Errorf("应只执行 2 个节点（fail-fast），实际执行 %v", *calls)
	}
	if len(pauses) != 0 {
		t.Errorf("首批失败不应有暂停，实际 %v", pauses)
	}
	if len(succeeded) != 1 || succeeded[0] != "d1" {
		t.Errorf("成功清单 = %v，期望 [d1]", succeeded)
	}
	if len(failedNodes) != 1 || failedNodes[0] != "d2" {
		t.Errorf("失败清单 = %v，期望 [d2]", failedNodes)
	}
	if len(aborted) != 2 || aborted[0] != "d3" || aborted[1] != "d4" {
		t.Errorf("未执行清单 = %v，期望 [d3 d4]", aborted)
	}
	if !strings.Contains(errBuf.String(), "中止") || !strings.Contains(errBuf.String(), "rollback") {
		t.Errorf("stderr 应提示中止与 rollback 衔接: %q", errBuf.String())
	}
}

// TestRunUpgradeBatches_FailFastSecondBatch 验证跨批失败中止：
// 5 节点 batch-size=2，第 4 节点失败 → 批次 1 完成（暂停 1 次），
// 批次 2 内中止，未执行 1。
func TestRunUpgradeBatches_FailFastSecondBatch(t *testing.T) {
	lines := []string{"d1", "d2", "d3", "d4", "d5"}
	exec, calls := fakeUpgradeExecutor(map[string]int{"d4": exitUsage})
	var pauses []time.Duration
	opts := batchOptions{BatchSize: 2, PauseBetween: 15 * time.Second}
	var out, errBuf bytes.Buffer

	ok, failed, _, _, aborted := runUpgradeBatches(
		opts, lines, exec, fakeSleeper(&pauses), &out, &errBuf)

	if ok != 3 || failed != 1 {
		t.Errorf("ok=%d failed=%d，期望 3/1", ok, failed)
	}
	if len(*calls) != 4 {
		t.Errorf("应只执行 4 个节点（fail-fast），实际执行 %v", *calls)
	}
	if len(pauses) != 1 || pauses[0] != 15*time.Second {
		t.Errorf("批次 1 完成后应暂停 1 次 15s，实际 %v", pauses)
	}
	if len(aborted) != 1 || aborted[0] != "d5" {
		t.Errorf("未执行清单 = %v，期望 [d5]", aborted)
	}
}

// TestBatchUpgradeGrayReleaseE2E 验证 batch upgrade 灰度端到端：
// --batch-size=2 --pause-between=1ms → 3 节点分批升级全部成功。
func TestBatchUpgradeGrayReleaseE2E(t *testing.T) {
	root := t.TempDir()
	dirs := make([]string, 3)
	for i := range dirs {
		dirs[i] = filepath.Join(root, "out", "node-g"+string(rune('a'+i)))
		joinOnce(t, dirs[i])
	}
	listFile := filepath.Join(root, "dirs.txt")
	if err := os.WriteFile(listFile, []byte(strings.Join(dirs, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out := runBatchForTest(t,
		"--op=upgrade", "--file="+listFile, "--version=v0.2.0",
		"--batch-size=2", "--pause-between=1ms")
	if code != exitOK {
		t.Fatalf("灰度批量 upgrade 退出码 = %d，输出:\n%s", code, out)
	}
	if !strings.Contains(out, "灰度分批") || !strings.Contains(out, "批次 1/2") {
		t.Errorf("输出应含灰度分批信息:\n%s", out)
	}
	if !strings.Contains(out, "成功 3，失败 0") {
		t.Errorf("汇总缺失:\n%s", out)
	}
	for _, d := range dirs {
		env := readFile(t, filepath.Join(d, "edgecore.env"))
		if !strings.Contains(env, "v0.2.0") {
			t.Errorf("%s 未升级到 v0.2.0", d)
		}
	}
}

// TestBatchUpgradeFailFastE2E 验证灰度 fail-fast 端到端：
// 中间节点失败 → 中止，后续节点未被升级，报告清单并提示 rollback。
func TestBatchUpgradeFailFastE2E(t *testing.T) {
	root := t.TempDir()
	nodeA := filepath.Join(root, "out", "node-a")
	nodeC := filepath.Join(root, "out", "node-c")
	nodeB := filepath.Join(root, "out", "node-b") // 空目录：upgrade 缺产物必失败
	joinOnce(t, nodeA)
	joinOnce(t, nodeC)
	if err := os.MkdirAll(nodeB, 0o755); err != nil {
		t.Fatal(err)
	}

	listFile := filepath.Join(root, "dirs.txt")
	if err := os.WriteFile(listFile, []byte(nodeA+"\n"+nodeB+"\n"+nodeC+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out := runBatchForTest(t,
		"--op=upgrade", "--file="+listFile, "--version=v0.2.0", "--batch-size=1")
	if code != exitRuntime {
		t.Fatalf("fail-fast 批量 upgrade 退出码 = %d，期望 exitRuntime；输出:\n%s", code, out)
	}
	// 中止报告：成功/失败/未执行清单 + rollback 提示。
	for _, want := range []string{"中止", "成功节点", "失败节点", "未执行", "rollback --latest"} {
		if !strings.Contains(out, want) {
			t.Errorf("输出应含 %q:\n%s", want, out)
		}
	}
	// node-a 已升级；node-c 未被触碰（fail-fast 中止在 node-b）。
	if env := readFile(t, filepath.Join(nodeA, "edgecore.env")); !strings.Contains(env, "v0.2.0") {
		t.Errorf("node-a 应已升级: %s", env)
	}
	if env := readFile(t, filepath.Join(nodeC, "edgecore.env")); !strings.Contains(env, "v0.1.0") || strings.Contains(env, "v0.2.0") {
		t.Errorf("node-c 不应被升级（已中止）: %s", env)
	}
	if _, err := os.Stat(filepath.Join(nodeC, backupsDir)); !os.IsNotExist(err) {
		t.Errorf("node-c 不应产生备份（未执行）")
	}
	// 失败节点台账记录 failed（与既有语义衔接）。
	entries := readLedger(t, nodeB)
	if len(entries) != 1 || entries[0].Result != "failed" {
		t.Errorf("node-b 应有 1 条 failed 台账，实际 %+v", entries)
	}
}

// TestBatchRejectsUpgradeFlagsForOtherOps 验证 --batch-size/--pause-between
// 仅 upgrade 支持：传给 join/rollback → exit 2。
func TestBatchRejectsUpgradeFlagsForOtherOps(t *testing.T) {
	for _, args := range [][]string{
		{"--op=join", "--file=x.txt", "--token=t", "--batch-size=2"},
		{"--op=rollback", "--file=x.txt", "--latest", "--pause-between=30s"},
	} {
		code, out := runBatchForTest(t, args...)
		if code != exitUsage || !strings.Contains(out, "仅 upgrade") {
			t.Errorf("%v: code=%d out=%q，期望 exit 2 且提示仅 upgrade", args, code, out)
		}
	}
}
