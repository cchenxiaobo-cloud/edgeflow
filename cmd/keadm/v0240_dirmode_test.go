package main

// v0.24.0 SEC-03 附新增用例：join 产物目录权限 opt-in（EDGEFLOW_JOIN_DIR_MODE）。
//   - 未设置（或空值）：默认 0755，与历史行为逐字节一致；
//   - 设置 "0700"：产物目录以 0700 创建，文件权限不变（edgecore.env 仍 0600）；
//   - 非法值（非八进制 "abc"、含非八进制数字 "999"、超 0777 "1000"）：
//     join 与 batch 入口 fail-fast（exitUsage），不创建任何目录。
// 本文件仅新增，不改动既有 *_test.go。

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// pinUmask 固定进程 umask 为 022：mkdir 的权限位受 umask 掩码影响，测试内
// 固定后才能对目录权限做精确断言，结束时恢复原值。
func pinUmask(t *testing.T) {
	t.Helper()
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })
}

// assertDirPerm 断言路径存在、是目录且权限精确匹配。
func assertDirPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s 失败: %v", path, err)
	}
	if !fi.IsDir() {
		t.Fatalf("%s 不是目录", path)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("目录权限 = %o，期望 %o", got, want)
	}
}

// joinArgsForOutput 构造一组合法 join 参数（产物目录由调用方指定）。
func joinArgsForOutput(out string) []string {
	return []string{
		"--cloudcore-ip=192.168.1.10", "--token=secret-token",
		"--node-id=edge-dm-01", "--output-dir=" + out,
	}
}

// TestResolveJoinDirMode 覆盖 helper 的默认值 / opt-in / 非法值解析。
func TestResolveJoinDirMode(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		want    os.FileMode
		wantErr bool
	}{
		{"未设置默认0755", "", 0o755, false},
		{"0700生效", "0700", 0o700, false},
		{"无前导零等价", "700", 0o700, false},
		{"999含非八进制数字报错", "999", 0, true},
		{"abc非八进制报错", "abc", 0, true},
		{"1000超0777报错", "1000", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(joinDirModeEnv, tc.value)
			mode, err := resolveJoinDirMode()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("%s=%q 应报错，实际返回权限 %o", joinDirModeEnv, tc.value, mode)
				}
				if !strings.Contains(err.Error(), joinDirModeEnv) {
					t.Errorf("错误信息应包含环境变量名 %s: %v", joinDirModeEnv, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s=%q 意外报错: %v", joinDirModeEnv, tc.value, err)
			}
			if mode != tc.want {
				t.Errorf("resolveJoinDirMode() = %o，期望 %o", mode, tc.want)
			}
		})
	}
}

// TestJoinDirModeDefault0755 未设置时产物目录权限与历史行为一致（0755）。
func TestJoinDirModeDefault0755(t *testing.T) {
	pinUmask(t)
	t.Setenv(joinDirModeEnv, "") // 置空等价于未设置
	out := filepath.Join(t.TempDir(), "keadm-out")
	var stdout, stderr bytes.Buffer
	if code := runJoin(joinArgsForOutput(out), &stdout, &stderr); code != exitOK {
		t.Fatalf("join 退出码 = %d，期望 %d；stderr=%s", code, exitOK, stderr.String())
	}
	assertDirPerm(t, out, 0o755)
}

// TestJoinDirModeOptIn0700 设置 0700 后产物目录收紧为仅属主可访问，
// 文件权限不受影响（edgecore.env 仍 0600）。
func TestJoinDirModeOptIn0700(t *testing.T) {
	pinUmask(t)
	t.Setenv(joinDirModeEnv, "0700")
	out := filepath.Join(t.TempDir(), "keadm-out")
	var stdout, stderr bytes.Buffer
	if code := runJoin(joinArgsForOutput(out), &stdout, &stderr); code != exitOK {
		t.Fatalf("join 退出码 = %d，期望 %d；stderr=%s", code, exitOK, stderr.String())
	}
	assertDirPerm(t, out, 0o700)
	fi, err := os.Stat(filepath.Join(out, "edgecore.env"))
	if err != nil {
		t.Fatalf("stat edgecore.env 失败: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("edgecore.env 权限 = %o，期望 600（目录 opt-in 不影响文件权限）", got)
	}
}

// TestJoinDirModeInvalidFailFast 非法值在 join 入口 fail-fast（exitUsage），
// stderr 提示环境变量名，且不创建任何产物目录。
func TestJoinDirModeInvalidFailFast(t *testing.T) {
	for _, v := range []string{"999", "abc"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(joinDirModeEnv, v)
			out := filepath.Join(t.TempDir(), "keadm-out")
			var stdout, stderr bytes.Buffer
			code := runJoin(joinArgsForOutput(out), &stdout, &stderr)
			if code != exitUsage {
				t.Errorf("join 退出码 = %d，期望 %d（exitUsage）；stderr=%s", code, exitUsage, stderr.String())
			}
			if !strings.Contains(stderr.String(), joinDirModeEnv) {
				t.Errorf("stderr 应提示 %s: %q", joinDirModeEnv, stderr.String())
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Errorf("非法配置下不应创建产物目录 %s（stat err=%v）", out, err)
			}
		})
	}
}

// TestBatchJoinDirModeInvalidFailFast batch join 入口同样 fail-fast：
// 在处理任何节点前报错，不创建批量产物根目录。
func TestBatchJoinDirModeInvalidFailFast(t *testing.T) {
	t.Setenv(joinDirModeEnv, "abc")
	dir := t.TempDir()
	nodesFile := filepath.Join(dir, "nodes.txt")
	if err := os.WriteFile(nodesFile, []byte("node-dm-1\nnode-dm-2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outRoot := filepath.Join(dir, "out")
	code, out := runBatchForTest(t,
		"--op=join", "--file="+nodesFile,
		"--cloudcore-ip=192.168.1.10", "--token=secret-token",
		"--output-dir="+outRoot)
	if code != exitUsage {
		t.Errorf("batch join 退出码 = %d，期望 %d（exitUsage）；输出:\n%s", code, exitUsage, out)
	}
	if !strings.Contains(out, joinDirModeEnv) {
		t.Errorf("输出应提示 %s: %q", joinDirModeEnv, out)
	}
	if _, err := os.Stat(outRoot); !os.IsNotExist(err) {
		t.Errorf("非法配置下不应创建批量产物根目录 %s（stat err=%v）", outRoot, err)
	}
}
