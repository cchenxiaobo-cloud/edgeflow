package main

// v0.23.0 P2 安全卫生新增用例（T-17 / SEC-03 + SEC-08）：
//   - T-17（SEC-03）：--token-file 文件方式传递 token（ps/history 不落明文）、
//     与 --token 同给时 --token-file 优先、edgecore.env 权限 0600、README 不落 token 明文；
//   - SEC-08：--cloudcore-port 数字与 1-65535 范围校验（join + batch 两处）。
// 本文件仅新增，不改动既有 *_test.go。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTempFile 写临时文件并返回路径（token 文件 / 节点清单共用）。
func writeTempFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写临时文件 %s 失败: %v", path, err)
	}
	return path
}

// TestJoinTokenFileValidation 验证 --token-file：文件内容（去首尾空白）作为
// token 渲染进 edgecore.env；文件不存在 / 空文件走既有的缺参报错路径。
func TestJoinTokenFileValidation(t *testing.T) {
	out, _ := tmpOut(t)
	dir := filepath.Dir(out)

	t.Run("文件内容作为token", func(t *testing.T) {
		out := out + "-tf"
		tf := writeTempFile(t, dir, "token.txt", "  s3cret-token-123 \n")
		code, stdout, stderr := runCapture([]string{
			"join", "--cloudcore-ip=192.168.1.10",
			"--token-file=" + tf, "--node-id=edge-tf-01", "--output-dir=" + out,
		}, "")
		if code != 0 {
			t.Fatalf("退出码 = %d，期望 0；stderr=%s stdout=%s", code, stderr, stdout)
		}
		env := readFile(t, filepath.Join(out, "edgecore.env"))
		if !strings.Contains(env, "EDGEFLOW_EDGECORE_TOKEN=s3cret-token-123\n") {
			t.Errorf("edgecore.env 应含文件方式传入的 token（已去首尾空白）: %s", env)
		}
	})

	t.Run("文件不存在报错", func(t *testing.T) {
		out := out + "-missing"
		code, _, stderr := runCapture([]string{
			"join", "--cloudcore-ip=192.168.1.10",
			"--token-file=" + filepath.Join(dir, "no-such-token-file"),
			"--node-id=edge-tf-01", "--output-dir=" + out,
		}, "")
		if code != exitUsage {
			t.Errorf("退出码 = %d，期望 %d（exitUsage）；stderr=%s", code, exitUsage, stderr)
		}
		if !strings.Contains(stderr, "--token-file") {
			t.Errorf("stderr 应提示 --token-file: %q", stderr)
		}
	})

	t.Run("空文件且无token按缺参拒绝", func(t *testing.T) {
		out := out + "-empty"
		tf := writeTempFile(t, dir, "token-empty.txt", "   \n")
		code, _, stderr := runCapture([]string{
			"join", "--cloudcore-ip=192.168.1.10",
			"--token-file=" + tf, "--node-id=edge-tf-01", "--output-dir=" + out,
		}, "")
		if code != exitUsage {
			t.Errorf("退出码 = %d，期望 %d（exitUsage）；stderr=%s", code, exitUsage, stderr)
		}
		if !strings.Contains(stderr, "--token") {
			t.Errorf("stderr 应提示缺少 --token: %q", stderr)
		}
	})
}

// TestJoinTokenFilePriority 验证 --token-file 与 --token 同给时 --token-file
// 优先（显式选择了更安全的传参方式）。
func TestJoinTokenFilePriority(t *testing.T) {
	out, _ := tmpOut(t)
	dir := filepath.Dir(out)
	tf := writeTempFile(t, dir, "token-pri.txt", "file-token\n")
	code, stdout, stderr := runCapture([]string{
		"join", "--cloudcore-ip=192.168.1.10",
		"--token=cli-token", "--token-file=" + tf,
		"--node-id=edge-pri-01", "--output-dir=" + out,
	}, "")
	if code != 0 {
		t.Fatalf("退出码 = %d，期望 0；stderr=%s stdout=%s", code, stderr, stdout)
	}
	env := readFile(t, filepath.Join(out, "edgecore.env"))
	if !strings.Contains(env, "EDGEFLOW_EDGECORE_TOKEN=file-token\n") {
		t.Errorf("--token-file 应优先于 --token: %s", env)
	}
	if strings.Contains(env, "EDGEFLOW_EDGECORE_TOKEN=cli-token") {
		t.Errorf("edgecore.env 不应出现 --token 传值: %s", env)
	}
}

// TestJoinTokenHygiene 证据用例（SEC-03）：edgecore.env 权限 0600（收紧确认，
// 前值即 0600 未放宽）；README.md（0644）不落 token 明文。
func TestJoinTokenHygiene(t *testing.T) {
	out, _ := tmpOut(t)
	code, stdout, stderr := runCapture([]string{
		"join", "--cloudcore-ip=192.168.1.10", "--token=hygiene-token",
		"--node-id=edge-hy-01", "--output-dir=" + out,
	}, "")
	if code != 0 {
		t.Fatalf("退出码 = %d，期望 0；stderr=%s stdout=%s", code, stderr, stdout)
	}

	fi, err := os.Stat(filepath.Join(out, "edgecore.env"))
	if err != nil {
		t.Fatalf("stat edgecore.env 失败: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("edgecore.env 权限 = %o，期望 600（token 明文文件必须 0600）", perm)
	}

	readme := readFile(t, filepath.Join(out, "README.md"))
	if strings.Contains(readme, "hygiene-token") {
		t.Errorf("README.md（0644）不应留存 token 明文（SEC-03）: %s", readme)
	}
}

// TestValidateCloudCorePort 表驱动验证端口校验规则（SEC-08）：
// 空 = 放行（调用方默认值兜底）；1-65535 数字合法；其余拒绝。
func TestValidateCloudCorePort(t *testing.T) {
	cases := []struct {
		name    string
		port    string
		wantErr bool
	}{
		{"空值放行", "", false},
		{"默认端口", "10000", false},
		{"下界1", "1", false},
		{"上界65535", "65535", false},
		{"测试用31000", "31000", false},
		{"零端口", "0", true},
		{"超上界", "65536", true},
		{"负数", "-1", true},
		{"非数字", "80a", true},
		{"带空白", " 80", true},
		{"十六进制", "0x10", true},
		{"浮点", "80.5", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCloudCorePort(tc.port)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateCloudCorePort(%q) err = %v，期望错误=%v", tc.port, err, tc.wantErr)
			}
		})
	}
}

// TestJoinInvalidCloudCorePortRejected 验证 join 对非法端口的端到端拒绝。
func TestJoinInvalidCloudCorePortRejected(t *testing.T) {
	cases := []struct {
		name string
		port string
	}{
		{"超范围", "99999"},
		{"零", "0"},
		{"非数字", "abc"},
		{"负数", "-22"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, _ := tmpOut(t)
			code, _, stderr := runCapture([]string{
				"join", "--cloudcore-ip=192.168.1.10", "--cloudcore-port=" + tc.port,
				"--token=t", "--node-id=edge-port-01", "--output-dir=" + out,
			}, "")
			if code != exitUsage {
				t.Errorf("port=%q 退出码 = %d，期望 %d；stderr=%s", tc.port, code, exitUsage, stderr)
			}
			if !strings.Contains(stderr, "1-65535") {
				t.Errorf("stderr 应含 1-65535 范围提示: %q", stderr)
			}
		})
	}
}

// TestBatchJoinTokenFileAndPort 验证 batch join：--token-file 透传到每个节点
// 产物、非法 --cloudcore-port / 缺 token 的拒绝（与单节点 join 同规则）。
func TestBatchJoinTokenFileAndPort(t *testing.T) {
	out, _ := tmpOut(t)
	dir := filepath.Dir(out)
	nodes := writeTempFile(t, dir, "nodes.txt", "edge-a\nedge-b\n")

	t.Run("token-file批量生效", func(t *testing.T) {
		out := out + "-tf"
		tf := writeTempFile(t, dir, "batch-token.txt", "batch-token-abc\n")
		code, stdout, stderr := runCapture([]string{
			"batch", "--op=join", "--file=" + nodes,
			"--cloudcore-ip=192.168.1.10", "--token-file=" + tf,
			"--output-dir=" + out,
		}, "")
		if code != 0 {
			t.Fatalf("退出码 = %d，期望 0；stderr=%s stdout=%s", code, stderr, stdout)
		}
		for _, node := range []string{"edge-a", "edge-b"} {
			env := readFile(t, filepath.Join(out, node, "edgecore.env"))
			if !strings.Contains(env, "EDGEFLOW_EDGECORE_TOKEN=batch-token-abc\n") {
				t.Errorf("%s 的 edgecore.env 应含 token-file 内容: %s", node, env)
			}
		}
	})

	t.Run("非法端口拒绝", func(t *testing.T) {
		out := out + "-port"
		code, _, stderr := runCapture([]string{
			"batch", "--op=join", "--file=" + nodes,
			"--cloudcore-ip=192.168.1.10", "--cloudcore-port=0",
			"--token=t", "--output-dir=" + out,
		}, "")
		if code != exitUsage {
			t.Errorf("退出码 = %d，期望 %d；stderr=%s", code, exitUsage, stderr)
		}
		if !strings.Contains(stderr, "1-65535") {
			t.Errorf("stderr 应含 1-65535 范围提示: %q", stderr)
		}
	})

	t.Run("token-file不存在拒绝", func(t *testing.T) {
		out := out + "-notf"
		code, _, stderr := runCapture([]string{
			"batch", "--op=join", "--file=" + nodes,
			"--cloudcore-ip=192.168.1.10",
			"--token-file=" + filepath.Join(dir, "no-such-batch-token"),
			"--output-dir=" + out,
		}, "")
		if code != exitUsage {
			t.Errorf("退出码 = %d，期望 %d；stderr=%s", code, exitUsage, stderr)
		}
		if !strings.Contains(stderr, "--token-file") {
			t.Errorf("stderr 应提示 --token-file: %q", stderr)
		}
	})
}
