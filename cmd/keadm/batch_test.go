package main

// keadm batch 批量操作测试（WBS 8.6）：清单读取、逐节点 join/upgrade/rollback、
// 错误路径与汇总退出码。

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runBatchForTest 封装 runBatch：返回退出码与合并输出。
func runBatchForTest(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := runBatch(args, &out, &errBuf)
	return code, out.String() + errBuf.String()
}

func TestReadNodeListSkipsCommentsAndBlank(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nodes.txt")
	content := "# 节点清单\n\nnode-a\nnode-b,192.168.1.20\n\n  # 注释行\nnode-c\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, err := readNodeList(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"node-a", "node-b,192.168.1.20", "node-c"}
	if len(lines) != len(want) {
		t.Fatalf("行数 = %d，期望 %d（内容 %v）", len(lines), len(want), lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("第 %d 行 = %q，期望 %q", i, lines[i], want[i])
		}
	}
}

func TestBatchFlagsValidation(t *testing.T) {
	// --op 非法
	code, out := runBatchForTest(t, "--op=deploy", "--file=x.txt")
	if code != exitUsage || !strings.Contains(out, "--op 必须为") {
		t.Errorf("非法 op: code=%d out=%q", code, out)
	}
	// 缺 --file
	code, out = runBatchForTest(t, "--op=join")
	if code != exitUsage || !strings.Contains(out, "--file") {
		t.Errorf("缺 file: code=%d out=%q", code, out)
	}
	// join 缺 token
	code, out = runBatchForTest(t, "--op=join", "--file=x.txt")
	if code != exitUsage || !strings.Contains(out, "--token") {
		t.Errorf("join 缺 token: code=%d out=%q", code, out)
	}
	// upgrade 缺 version
	code, out = runBatchForTest(t, "--op=upgrade", "--file=x.txt")
	if code != exitUsage || !strings.Contains(out, "--version") {
		t.Errorf("upgrade 缺 version: code=%d out=%q", code, out)
	}
}

func TestBatchJoinMultipleNodes(t *testing.T) {
	dir := t.TempDir()
	nodesFile := filepath.Join(dir, "nodes.txt")
	if err := os.WriteFile(nodesFile, []byte("node-b1\nnode-b2,127.0.0.2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := runBatchForTest(t,
		"--op=join", "--file="+nodesFile,
		"--cloudcore-ip=127.0.0.1", "--token=batched-secret",
		"--output-dir="+filepath.Join(dir, "out"))
	if code != exitOK {
		t.Fatalf("batch join 退出码 = %d，输出:\n%s", code, out)
	}
	if !strings.Contains(out, "成功 2，失败 0") {
		t.Errorf("汇总缺失，输出:\n%s", out)
	}
	// 每个节点独立产物目录 + 独立 token
	for _, id := range []string{"node-b1", "node-b2"} {
		env := filepath.Join(dir, "out", id, "edgecore.env")
		data, err := os.ReadFile(env)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", env, err)
		}
		if !strings.Contains(string(data), "batched-secret") {
			t.Errorf("%s 未写入 token", env)
		}
	}
	// 清单行覆盖 IP：node-b2 用 127.0.0.2
	env2 := filepath.Join(dir, "out", "node-b2", "edgecore.env")
	data, _ := os.ReadFile(env2)
	if !strings.Contains(string(data), "127.0.0.2") {
		t.Errorf("node-b2 未使用清单行 IP，内容:\n%s", data)
	}
}

func TestBatchUpgradeAndRollback(t *testing.T) {
	dir := t.TempDir()
	// 先单节点 join 生成产物
	joinArgs := []string{"--cloudcore-ip=127.0.0.1", "--token=t", "--node-id=node-u1",
		"--output-dir=" + filepath.Join(dir, "out", "node-u1")}
	if code := runJoin(joinArgs, &bytes.Buffer{}, &bytes.Buffer{}); code != exitOK {
		t.Fatalf("预置 join 失败 code=%d", code)
	}

	dirsFile := filepath.Join(dir, "dirs.txt")
	if err := os.WriteFile(dirsFile, []byte(filepath.Join(dir, "out", "node-u1")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 批量 upgrade 到 v0.2.0
	code, out := runBatchForTest(t, "--op=upgrade", "--file="+dirsFile, "--version=v0.2.0")
	if code != exitOK {
		t.Fatalf("batch upgrade 退出码 = %d，输出:\n%s", code, out)
	}
	env := filepath.Join(dir, "out", "node-u1", "edgecore.env")
	data, _ := os.ReadFile(env)
	if !strings.Contains(string(data), "v0.2.0") {
		t.Errorf("upgrade 后版本未更新，内容:\n%s", data)
	}

	// 批量 rollback --latest
	code, out = runBatchForTest(t, "--op=rollback", "--file="+dirsFile, "--latest")
	if code != exitOK {
		t.Fatalf("batch rollback 退出码 = %d，输出:\n%s", code, out)
	}
	data, _ = os.ReadFile(env)
	if !strings.Contains(string(data), "v0.1.0") {
		t.Errorf("rollback 后版本未回退，内容:\n%s", data)
	}
}

func TestBatchPartialFailure(t *testing.T) {
	dir := t.TempDir()
	nodesFile := filepath.Join(dir, "nodes.txt")
	// node-bad 的 node-id 非法（含非法字符）会校验失败
	if err := os.WriteFile(nodesFile, []byte("node-ok\nbad..id\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := runBatchForTest(t,
		"--op=join", "--file="+nodesFile,
		"--cloudcore-ip=127.0.0.1", "--token=t",
		"--output-dir="+filepath.Join(dir, "out"))
	if code != exitRuntime {
		t.Fatalf("部分失败应返回 exitRuntime，实际 %d，输出:\n%s", code, out)
	}
	if !strings.Contains(out, "成功 1，失败 1") {
		t.Errorf("汇总应为 成功 1，失败 1，输出:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "out", "node-ok", "edgecore.env")); err != nil {
		t.Errorf("node-ok 产物应生成: %v", err)
	}
}
