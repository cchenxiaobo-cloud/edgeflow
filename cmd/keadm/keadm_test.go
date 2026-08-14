package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tmpOut 创建临时输出目录并返回路径 + 清理函数。
func tmpOut(t *testing.T) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "keadm-out")
	return out, func() {}
}

// readFile 读取文件内容，失败即测试失败。
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return string(b)
}

// runCapture 运行 keadm 并返回退出码 + stdout/stderr。
func runCapture(args []string, stdin string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr, strings.NewReader(stdin))
	return code, stdout.String(), stderr.String()
}

// TestInitGeneratesDeployableYAML 验证 init 生成产物：YAML 含镜像/探针/卷/TLS env。
func TestInitGeneratesDeployableYAML(t *testing.T) {
	out, _ := tmpOut(t)
	code, stdout, stderr := runCapture([]string{
		"init", "--tls", "--tls-san=IP:192.168.1.10",
		"--cloudcore-image=edgeflow/cloudcore:v0.1.0",
		"--output-dir=" + out,
	}, "")
	if code != 0 {
		t.Fatalf("init 退出码 = %d，期望 0；stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "keadm init 完成") {
		t.Errorf("stdout 缺少完成提示: %q", stdout)
	}

	// 产物文件必须存在
	yamlText := readFile(t, filepath.Join(out, "cloudcore.yaml"))
	notesText := readFile(t, filepath.Join(out, "NOTES.txt"))

	// 镜像
	if !strings.Contains(yamlText, "image: edgeflow/cloudcore:v0.1.0") {
		t.Errorf("YAML 缺少镜像: %s", yamlText)
	}
	// 探针（/healthz）
	if !strings.Contains(yamlText, "path: /healthz") {
		t.Errorf("YAML 缺少 /healthz 探针")
	}
	// 卷（/data）
	if !strings.Contains(yamlText, "mountPath: /data") {
		t.Errorf("YAML 缺少 /data 卷挂载")
	}
	// TLS env 透传
	if !strings.Contains(yamlText, "EDGEFLOW_CLOUDCORE_TLS") || !strings.Contains(yamlText, `value: "on"`) {
		t.Errorf("YAML 缺少 TLS env 透传")
	}
	if !strings.Contains(yamlText, "EDGEFLOW_CLOUDCORE_CERT_DIR") || !strings.Contains(yamlText, "/data/certs") {
		t.Errorf("YAML 缺少 CERT_DIR env")
	}
	// SAN 注入
	if !strings.Contains(yamlText, "EDGEFLOW_CLOUDCORE_TLS_SAN") || !strings.Contains(yamlText, "IP:192.168.1.10") {
		t.Errorf("YAML 缺少 TLS_SAN env")
	}
	// 端口与 Service（NodePort 默认）
	if !strings.Contains(yamlText, "containerPort: 8080") || !strings.Contains(yamlText, "containerPort: 10000") {
		t.Errorf("YAML 缺少 http/hub 端口")
	}
	if !strings.Contains(yamlText, "type: NodePort") {
		t.Errorf("YAML Service 应为 NodePort")
	}
	// 部署说明含 Helm 替代路径
	if !strings.Contains(notesText, "helm install") || !strings.Contains(notesText, "kubectl apply -f") {
		t.Errorf("NOTES.txt 缺少部署说明/Helm 替代路径")
	}
}

// TestInitWithoutTLS 验证未开 TLS 时 YAML 不含 TLS env。
func TestInitWithoutTLS(t *testing.T) {
	out, _ := tmpOut(t)
	code, _, stderr := runCapture([]string{"init", "--output-dir=" + out}, "")
	if code != 0 {
		t.Fatalf("init 退出码 = %d，期望 0；stderr=%s", code, stderr)
	}
	yamlText := readFile(t, filepath.Join(out, "cloudcore.yaml"))
	if strings.Contains(yamlText, "EDGEFLOW_CLOUDCORE_TLS") {
		t.Errorf("未启用 --tls 时不应出现 TLS env")
	}
}

// TestInitIdempotent 验证 init 重复执行幂等（不报错、覆盖生成）。
func TestInitIdempotent(t *testing.T) {
	out, _ := tmpOut(t)
	for i := 0; i < 2; i++ {
		code, _, stderr := runCapture([]string{"init", "--output-dir=" + out}, "")
		if code != 0 {
			t.Fatalf("第 %d 次 init 退出码 = %d，期望 0；stderr=%s", i+1, code, stderr)
		}
	}
}

// TestJoinEnvFile 验证 join 生成 env 文件：键值正确 + TLS 透传 + 地址形态。
func TestJoinEnvFile(t *testing.T) {
	out, _ := tmpOut(t)
	code, stdout, stderr := runCapture([]string{
		"join", "--cloudcore-ip=192.168.1.10", "--cloudcore-port=31000",
		"--token=abc123", "--node-id=edge-worker-01", "--tls",
		"--output-dir=" + out,
	}, "")
	if code != 0 {
		t.Fatalf("join 退出码 = %d，期望 0；stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "wss://192.168.1.10:31000/v1/edge") {
		t.Errorf("stdout 接入地址错误: %q", stdout)
	}

	envText := readFile(t, filepath.Join(out, "edgecore.env"))
	checks := []string{
		"EDGEFLOW_EDGECORE_NODE_ID=edge-worker-01",
		"EDGEFLOW_EDGECORE_CLOUD_ADDR=wss://192.168.1.10:31000/v1/edge",
		"EDGEFLOW_EDGECORE_DB_PATH=/var/lib/edgeflow/edgeflow.db",
		"EDGEFLOW_EDGECORE_MQTT_ADDR=tcp://127.0.0.1:1883",
		"EDGEFLOW_EDGECORE_TOKEN=abc123",
		"EDGEFLOW_EDGECORE_TLS=on",
		"EDGEFLOW_EDGECORE_CERT_DIR=/etc/edgeflow/certs",
	}
	for _, c := range checks {
		if !strings.Contains(envText, c) {
			t.Errorf("edgecore.env 缺少 %q\n完整内容:\n%s", c, envText)
		}
	}

	// systemd 单元：EnvironmentFile + ExecStart
	svcText := readFile(t, filepath.Join(out, "edgecore.service"))
	for _, c := range []string{
		"EnvironmentFile=/etc/edgeflow/edgecore.env",
		"ExecStart=/usr/local/bin/edgecore",
		"Restart=on-failure",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(svcText, c) {
			t.Errorf("edgecore.service 缺少 %q\n完整内容:\n%s", c, svcText)
		}
	}

	// 安装脚本：可执行权限 + 关键步骤
	installPath := filepath.Join(out, "install.sh")
	info, err := os.Stat(installPath)
	if err != nil {
		t.Fatalf("install.sh 不存在: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("install.sh 应具有可执行权限，实际 %v", info.Mode().Perm())
	}
	installText := readFile(t, installPath)
	for _, c := range []string{"systemctl enable --now edgecore", "install -m 0600 edgecore.env"} {
		if !strings.Contains(installText, c) {
			t.Errorf("install.sh 缺少 %q", c)
		}
	}
}

// TestJoinWithoutTLS 验证未开 TLS 时 env 使用 ws:// 且无 TLS 键。
func TestJoinWithoutTLS(t *testing.T) {
	out, _ := tmpOut(t)
	code, _, stderr := runCapture([]string{
		"join", "--cloudcore-ip=10.0.0.5", "--token=xyz", "--node-id=edge-1",
		"--output-dir=" + out,
	}, "")
	if code != 0 {
		t.Fatalf("join 退出码 = %d，期望 0；stderr=%s", code, stderr)
	}
	envText := readFile(t, filepath.Join(out, "edgecore.env"))
	if !strings.Contains(envText, "EDGEFLOW_EDGECORE_CLOUD_ADDR=ws://10.0.0.5:10000/v1/edge") {
		t.Errorf("非 TLS 地址错误: %s", envText)
	}
	if strings.Contains(envText, "EDGEFLOW_EDGECORE_TLS=") {
		t.Errorf("未启用 --tls 时不应出现 TLS 键")
	}
}

// TestJoinIPv6 验证 IPv6 地址加方括号。
func TestJoinIPv6(t *testing.T) {
	out, _ := tmpOut(t)
	code, stdout, stderr := runCapture([]string{
		"join", "--cloudcore-ip=::1", "--token=t", "--node-id=edge-v6",
		"--output-dir=" + out,
	}, "")
	if code != 0 {
		t.Fatalf("join(IPv6) 退出码 = %d，期望 0；stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "ws://[::1]:10000/v1/edge") {
		t.Errorf("IPv6 地址应加方括号: %q", stdout)
	}
	envText := readFile(t, filepath.Join(out, "edgecore.env"))
	if !strings.Contains(envText, "EDGEFLOW_EDGECORE_CLOUD_ADDR=ws://[::1]:10000/v1/edge") {
		t.Errorf("IPv6 env 地址错误: %s", envText)
	}
}

// TestReset 验证 reset：确认后删除、幂等、--force 跳过确认。
func TestReset(t *testing.T) {
	out, _ := tmpOut(t)
	// 先生成产物
	if code, _, stderr := runCapture([]string{"init", "--output-dir=" + out}, ""); code != 0 {
		t.Fatalf("init 失败: %s", stderr)
	}
	if _, err := os.Stat(filepath.Join(out, "cloudcore.yaml")); err != nil {
		t.Fatalf("预置产物失败: %v", err)
	}

	// 拒绝确认：文件保留
	code, stdout, _ := runCapture([]string{"reset", "--output-dir=" + out}, "n\n")
	if code != 0 {
		t.Fatalf("reset(拒绝) 退出码 = %d，期望 0", code)
	}
	if !strings.Contains(stdout, "已取消") {
		t.Errorf("拒绝确认时应提示已取消: %q", stdout)
	}
	if _, err := os.Stat(filepath.Join(out, "cloudcore.yaml")); err != nil {
		t.Errorf("拒绝确认后产物不应被删除: %v", err)
	}

	// 确认删除：文件消失，目录被移除
	code, stdout, _ = runCapture([]string{"reset", "--output-dir=" + out}, "y\n")
	if code != 0 {
		t.Fatalf("reset(确认) 退出码 = %d，期望 0", code)
	}
	if !strings.Contains(stdout, "已删除") {
		t.Errorf("确认后应提示已删除: %q", stdout)
	}
	if _, err := os.Stat(filepath.Join(out, "cloudcore.yaml")); !os.IsNotExist(err) {
		t.Errorf("确认后 cloudcore.yaml 应被删除")
	}

	// 幂等：再次 reset 以 0 退出并提示无需清理
	code, stdout, _ = runCapture([]string{"reset", "--output-dir=" + out}, "")
	if code != 0 {
		t.Fatalf("reset(幂等) 退出码 = %d，期望 0", code)
	}
	if !strings.Contains(stdout, "无可清理产物") && !strings.Contains(stdout, "无需清理") {
		t.Errorf("幂等 reset 应提示无需清理: %q", stdout)
	}

	// --force 跳过确认
	if code, _, _ := runCapture([]string{"init", "--output-dir=" + out}, ""); code != 0 {
		t.Fatalf("init(第二次) 失败")
	}
	code, _, _ = runCapture([]string{"reset", "--force", "--output-dir=" + out}, "")
	if code != 0 {
		t.Fatalf("reset(--force) 退出码 = %d，期望 0", code)
	}
	if _, err := os.Stat(filepath.Join(out, "cloudcore.yaml")); !os.IsNotExist(err) {
		t.Errorf("--force 后产物应被删除")
	}
}

// TestResetPreservesForeignFiles 验证 reset 不删除用户自己的文件。
func TestResetPreservesForeignFiles(t *testing.T) {
	out, _ := tmpOut(t)
	if code, _, _ := runCapture([]string{"init", "--output-dir=" + out}, ""); code != 0 {
		t.Fatalf("init 失败")
	}
	foreign := filepath.Join(out, "my-notes.md")
	if err := os.WriteFile(foreign, []byte("用户文件"), 0o644); err != nil {
		t.Fatalf("写用户文件失败: %v", err)
	}

	if code, _, _ := runCapture([]string{"reset", "--force", "--output-dir=" + out}, ""); code != 0 {
		t.Fatalf("reset 失败")
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("reset 不应删除用户文件: %v", err)
	}
}

// TestVersion 验证 version 输出与退出码。
func TestVersion(t *testing.T) {
	code, stdout, stderr := runCapture([]string{"version"}, "")
	if code != 0 {
		t.Fatalf("version 退出码 = %d，期望 0；stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "keadm version=") {
		t.Errorf("version 输出缺少版本信息: %q", stdout)
	}

	// --json 形态
	code, stdout, _ = runCapture([]string{"version", "--json"}, "")
	if code != 0 {
		t.Fatalf("version --json 退出码 = %d，期望 0", code)
	}
	if !strings.Contains(stdout, `"version"`) || !strings.Contains(stdout, `"goVersion"`) {
		t.Errorf("version --json 输出缺少字段: %q", stdout)
	}
}

// TestUsageAndUnknownCommand 验证无参数/未知子命令退出码非 0。
func TestUsageAndUnknownCommand(t *testing.T) {
	// 无参数：用法错误退出码 2
	code, _, stderr := runCapture(nil, "")
	if code != 2 {
		t.Errorf("无参数退出码 = %d，期望 2；stderr=%s", code, stderr)
	}
	// 未知子命令：退出码 2
	code, _, stderr = runCapture([]string{"frobnicate"}, "")
	if code != 2 {
		t.Errorf("未知子命令退出码 = %d，期望 2；stderr=%s", code, stderr)
	}
	// -h：帮助走 stdout，退出码 0
	code, stdout, _ := runCapture([]string{"--help"}, "")
	if code != 0 || !strings.Contains(stdout, "keadm <command>") {
		t.Errorf("-h 应打印帮助并以 0 退出: code=%d stdout=%q", code, stdout)
	}
}

// TestJoinInvalidArgs 验证异常路径：缺 ip / 非法 ip / 空 token / 空 node-id 均非 0。
func TestJoinInvalidArgs(t *testing.T) {
	out, _ := tmpOut(t)
	cases := []struct {
		name string
		args []string
	}{
		{"缺 ip", []string{"join", "--token=t", "--output-dir=" + out}},
		{"非法 ip", []string{"join", "--cloudcore-ip=999.1.1.1", "--token=t", "--output-dir=" + out}},
		{"非法 ip 文本", []string{"join", "--cloudcore-ip=not-an-ip", "--token=t", "--output-dir=" + out}},
		{"空 token", []string{"join", "--cloudcore-ip=1.2.3.4", "--token=", "--output-dir=" + out}},
		{"空 node-id", []string{"join", "--cloudcore-ip=1.2.3.4", "--token=t", "--node-id=", "--output-dir=" + out}},
		{"node-id 含空格", []string{"join", "--cloudcore-ip=1.2.3.4", "--token=t", "--node-id=edge a", "--output-dir=" + out}},
	}
	for _, tc := range cases {
		code, _, stderr := runCapture(tc.args, "")
		if code == 0 {
			t.Errorf("%s: 退出码 = 0，期望非 0；stderr=%s", tc.name, stderr)
		}
		if stderr == "" {
			t.Errorf("%s: stderr 应有错误提示", tc.name)
		}
		// 错误提示应给出可操作建议（含"示例"或"错误"字样）
		if !strings.Contains(stderr, "错误") {
			t.Errorf("%s: stderr 应含错误标识: %q", tc.name, stderr)
		}
	}
}

// TestInitInvalidArgs 验证 init 异常路径：空镜像/非法 service-type 非 0。
func TestInitInvalidArgs(t *testing.T) {
	out, _ := tmpOut(t)
	cases := []struct {
		name string
		args []string
	}{
		{"空镜像", []string{"init", "--cloudcore-image=", "--output-dir=" + out}},
		{"非法 service-type", []string{"init", "--service-type=LoadBalancer", "--output-dir=" + out}},
		{"位置参数", []string{"init", "extra", "--output-dir=" + out}},
	}
	for _, tc := range cases {
		code, _, stderr := runCapture(tc.args, "")
		if code == 0 {
			t.Errorf("%s: 退出码 = 0，期望非 0；stderr=%s", tc.name, stderr)
		}
	}
}
