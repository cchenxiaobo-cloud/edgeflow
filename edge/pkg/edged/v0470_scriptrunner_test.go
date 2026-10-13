package edged

// v0.47.0（spec 0020 US-4）scriptrunner 单测：opt-in 门控、sha256 硬校验、
// 生命周期执行（install/start）、卸载清理、PodStatus 形态映射。
// 外部行为（真实 sh 执行）按项目惯例用真实进程断言（无 mock）。

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildScriptPkg 构造测试脚本包（tar.gz：install/start/stop/uninstall 四动作）。
func buildScriptPkg(t *testing.T, marker string) []byte {
	t.Helper()
	files := map[string]string{
		"install":   "#!/bin/sh\necho install >> marker.txt\necho " + marker + " > app_state.txt\n",
		"start":     "#!/bin/sh\necho start >> marker.txt\n",
		"stop":      "#!/bin/sh\necho stop >> marker.txt\n",
		"uninstall": "#!/bin/sh\necho uninstall >> marker.txt\n",
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// scriptConfigJSON 构造 ConfigSync 负载 JSON（与云端下发一致）。
func scriptConfigJSON(t *testing.T, op, app, version string, blob []byte, sha string) []byte {
	t.Helper()
	_ = op
	return []byte(`{"operation":"` + op + `","config":{"name":"edgeflow-app-` + app + `","namespace":"edgeflow","kind":"ConfigMap","data":{"app":"` + app + `","version":"` + version + `","scriptRef":"demo","scriptTar":"` + base64.StdEncoding.EncodeToString(blob) + `","sha256":"` + sha + `"}}}`)
}

func TestV0470ScriptRunnerDisabledByDefault(t *testing.T) {
	// 未设置 env → 未启用：HandleConfigSync 零执行（不落包、不执行、无状态）
	s := NewScriptRunner()
	if s.Enabled() {
		t.Fatal("默认应关闭")
	}
	s.HandleConfigSync(scriptConfigJSON(t, "add", "demo", "v1", buildScriptPkg(t, "x"), ""))
	if got := s.StatusPayloads("n1"); len(got) != 0 {
		t.Fatalf("未启用不应产生伪 Pod 状态: %+v", got)
	}
}

func TestV0470ScriptRunnerLifecycle(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvScriptRunner, "on")
	t.Setenv(EnvAppPkgDir, dir)
	s := NewScriptRunner()
	if !s.Enabled() {
		t.Fatal("env=on 应启用")
	}
	blob := buildScriptPkg(t, "hello")
	sum := sha256.Sum256(blob)
	s.HandleConfigSync(scriptConfigJSON(t, "add", "demo", "v1.0.0", blob, hex.EncodeToString(sum[:])))

	// 状态：running + 伪 Pod 形态（edgeflow-script-demo / edgeflow ns / Running）
	ps := s.StatusPayloads("n1")
	if len(ps) != 1 || ps[0].PodName != "edgeflow-script-demo" || ps[0].Namespace != "edgeflow" || ps[0].Phase != "Running" {
		t.Fatalf("伪 Pod 状态 = %+v", ps)
	}
	if ps[0].NodeID != "n1" {
		t.Fatalf("NodeID = %s", ps[0].NodeID)
	}
	// 工作目录产物：install/start 真实执行（marker 追加两行 + app_state 内容）
	work := filepath.Join(dir, "work", "demo-v1.0.0")
	marker, err := os.ReadFile(filepath.Join(work, "marker.txt"))
	if err != nil {
		t.Fatalf("install/start 未执行（marker 缺失）: %v", err)
	}
	if lines := strings.Count(string(marker), "\n"); lines < 2 {
		t.Fatalf("marker 行数 = %d, want ≥2（install+start）", lines)
	}
	// 包缓存落盘
	if _, err := os.Stat(filepath.Join(dir, "demo", "v1.0.0.tgz")); err != nil {
		t.Fatalf("包缓存未落盘: %v", err)
	}

	// 卸载：stop/uninstall 执行 + 工作目录清理 + 状态清空
	s.HandleConfigSync([]byte(`{"operation":"delete","config":{"name":"edgeflow-app-demo","namespace":"edgeflow"}}`))
	if ps := s.StatusPayloads("n1"); len(ps) != 0 {
		t.Fatalf("卸载后状态应清空: %+v", ps)
	}
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Fatalf("工作目录未清理")
	}
	// uninstall 动作有执行痕迹（工作目录被清前 marker 已含 uninstall）
}

func TestV0470ScriptRunnerShaMismatch(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvScriptRunner, "on")
	t.Setenv(EnvAppPkgDir, dir)
	s := NewScriptRunner()
	s.HandleConfigSync(scriptConfigJSON(t, "add", "bad", "v1", buildScriptPkg(t, "x"), "deadbeef"))
	ps := s.StatusPayloads("n1")
	if len(ps) != 1 || ps[0].Phase != "Error" || !strings.Contains(ps[0].Message, "sha256 不一致") {
		t.Fatalf("sha256 不一致应 failed（Phase=Error + detail）: %+v", ps)
	}
	// 校验失败不得解包执行
	if _, err := os.Stat(filepath.Join(dir, "work", "bad-v1")); !os.IsNotExist(err) {
		t.Fatalf("校验失败不应解包执行")
	}
}

func TestV0470ScriptRunnerNonScriptConfigNoop(t *testing.T) {
	t.Setenv(EnvScriptRunner, "on")
	s := NewScriptRunner()
	// 普通配置（无 scriptRef/scriptTar）零行为
	s.HandleConfigSync([]byte(`{"operation":"add","config":{"name":"cm-1","namespace":"default","kind":"ConfigMap","data":{"k":"v"}}}`))
	if ps := s.StatusPayloads("n1"); len(ps) != 0 {
		t.Fatalf("非脚本配置不应产生状态: %+v", ps)
	}
}
