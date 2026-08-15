package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// joinOptions 是 keadm join 的参数集合。
type joinOptions struct {
	// CloudCoreIP 是云端 CloudHub 所在节点的 IP（必填，合法 IPv4/IPv6）。
	CloudCoreIP string
	// CloudCorePort 是 CloudHub 端口（NodePort 部署时为节点端口，默认 10000）。
	CloudCorePort string
	// Token 是接入令牌（必填；edgecore 注册时携带，云端 EDGEFLOW_CLOUDCORE_NODE_TOKEN
	// 启用时校验，见 WBS 7.3 设备认证）。
	Token string
	// NodeID 是边缘节点 ID（默认 edge-<主机名>，与 edgehub.DefaultNodeID 约定一致）。
	NodeID string
	// TLS 是否启用云边 mTLS（edgecore 侧注入 EDGEFLOW_EDGECORE_TLS=on，地址升级 wss://）。
	TLS bool
	// OutputDir 是产物输出目录。
	OutputDir string
}

// joinOutputs 是 keadm join 生成的产物清单（reset 依据此清单清理）。
var joinOutputs = []string{"edgecore.env", "edgecore.service", "install.sh", "README.md"}

// 边缘节点安装的固定路径约定（与 install.sh / edgecore.service 保持一致）。
const (
	// edgeConfDir 是边缘配置目录（环境变量文件所在目录）。
	edgeConfDir = "/etc/edgeflow"
	// edgeEnvPath 是环境变量文件在节点上的安装路径。
	edgeEnvPath = "/etc/edgeflow/edgecore.env"
	// edgeDBPath 是 edgecore 元数据 SQLite 的绝对路径（避免相对 cwd 歧义）。
	edgeDBPath = "/var/lib/edgeflow/edgeflow.db"
	// edgeCertDir 是 mTLS 证书目录（edgecore 首次运行自动生成，幂等）。
	edgeCertDir = "/etc/edgeflow/certs"
	// edgeWorkDir 是 edgecore 的 WorkingDirectory（数据库所在目录）。
	edgeWorkDir = "/var/lib/edgeflow"
	// edgeBinPath 是 edgecore 二进制安装路径。
	edgeBinPath = "/usr/local/bin/edgecore"
)

// runJoin 实现 keadm join：生成边缘接入产物（env + systemd 单元 + 安装脚本 + 说明）。
func runJoin(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("join", stderr)
	opts := joinOptions{
		CloudCorePort: "10000",
		NodeID:        defaultNodeID(),
		OutputDir:     "./keadm-out",
	}
	fs.StringVar(&opts.CloudCoreIP, "cloudcore-ip", "", "云端 CloudHub 节点 IP（必填，支持 IPv4/IPv6）")
	fs.StringVar(&opts.CloudCorePort, "cloudcore-port", opts.CloudCorePort, "CloudHub 端口（NodePort 部署时填节点端口）")
	fs.StringVar(&opts.Token, "token", "", "接入令牌（必填；edgecore 注册携带，云端启用时校验，WBS 7.3）")
	fs.StringVar(&opts.NodeID, "node-id", opts.NodeID, "边缘节点 ID（默认 edge-<主机名>）")
	fs.BoolVar(&opts.TLS, "tls", false, "启用云边 mTLS（edgecore 注入 TLS env，地址使用 wss://）")
	fs.StringVar(&opts.OutputDir, "output-dir", opts.OutputDir, "产物输出目录")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "错误: join 不接受位置参数 %q\n", fs.Arg(0))
		return exitUsage
	}

	// 参数校验：IP 合法、token 非空、node-id 非空无空白。
	if err := opts.validate(); err != nil {
		_, _ = fmt.Fprintf(stderr, "错误: %v\n", err)
		return exitUsage
	}

	// 创建输出目录（已存在则复用，保证重复执行幂等）。
	if err := os.MkdirAll(opts.OutputDir, 0o755); err != nil {
		_, _ = fmt.Fprintf(stderr, "错误: 创建输出目录 %s 失败: %v\n", opts.OutputDir, err)
		return exitRuntime
	}

	// 生成 edgecore.env：环境变量文件，键名与 edgecore/edgehub 读取的完全一致。
	envBytes := renderEdgeEnv(opts)
	if err := os.WriteFile(filepath.Join(opts.OutputDir, "edgecore.env"), envBytes, 0o600); err != nil {
		_, _ = fmt.Fprintf(stderr, "错误: 写入 edgecore.env 失败: %v\n", err)
		return exitRuntime
	}

	// 生成 edgecore.service：systemd 单元（EnvironmentFile 指向节点上的固定路径）。
	svcBytes := renderEdgeService(opts)
	if err := os.WriteFile(filepath.Join(opts.OutputDir, "edgecore.service"), svcBytes, 0o644); err != nil {
		_, _ = fmt.Fprintf(stderr, "错误: 写入 edgecore.service 失败: %v\n", err)
		return exitRuntime
	}

	// 生成 install.sh：在边缘节点上执行的安装脚本（需 root）。
	installBytes := renderInstallScript(opts)
	if err := os.WriteFile(filepath.Join(opts.OutputDir, "install.sh"), installBytes, 0o755); err != nil {
		_, _ = fmt.Fprintf(stderr, "错误: 写入 install.sh 失败: %v\n", err)
		return exitRuntime
	}

	// 生成 README.md：接入说明（含手动安装步骤片段，供无法直接跑脚本的环境参考）。
	readmeBytes := renderJoinReadme(opts)
	if err := os.WriteFile(filepath.Join(opts.OutputDir, "README.md"), readmeBytes, 0o644); err != nil {
		_, _ = fmt.Fprintf(stderr, "错误: 写入 README.md 失败: %v\n", err)
		return exitRuntime
	}

	// 登记产物校验清单（M4C P2-②）：与 init 共用同一清单文件，追加登记 join 产物。
	m, _, err := loadManifest(opts.OutputDir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "错误: 读取校验清单失败: %v\n", err)
		return exitRuntime
	}
	if err := recordGeneratedFiles(opts.OutputDir, joinOutputs, m); err != nil {
		_, _ = fmt.Fprintf(stderr, "错误: 登记产物校验和失败: %v\n", err)
		return exitRuntime
	}
	if err := saveManifest(opts.OutputDir, m); err != nil {
		_, _ = fmt.Fprintf(stderr, "错误: 写入校验清单 %s 失败: %v\n", manifestName, err)
		return exitRuntime
	}

	_, _ = fmt.Fprintf(stdout, "keadm join 完成: 边缘接入产物已生成到 %s\n", opts.OutputDir)
	_, _ = fmt.Fprintf(stdout, "  - edgecore.env（环境变量文件）\n")
	_, _ = fmt.Fprintf(stdout, "  - edgecore.service（systemd 单元）\n")
	_, _ = fmt.Fprintf(stdout, "  - install.sh（安装脚本，需 root 在边缘节点执行）\n")
	_, _ = fmt.Fprintf(stdout, "  - README.md（接入说明）\n")
	_, _ = fmt.Fprintf(stdout, "接入地址: %s\n", opts.cloudAddr())
	return exitOK
}

// validate 校验 join 参数，返回带可操作建议的错误信息。
func (o joinOptions) validate() error {
	if o.CloudCoreIP == "" {
		return fmt.Errorf("缺少必填参数 --cloudcore-ip（示例: keadm join --cloudcore-ip=192.168.1.10 --token=<token>）")
	}
	if net.ParseIP(o.CloudCoreIP) == nil {
		return fmt.Errorf("--cloudcore-ip=%q 不是合法 IP 地址（支持 IPv4 如 192.168.1.10，或 IPv6 如 ::1）", o.CloudCoreIP)
	}
	if strings.TrimSpace(o.Token) == "" {
		return fmt.Errorf("缺少必填参数 --token（示例: keadm join --cloudcore-ip=192.168.1.10 --token=abc123）")
	}
	if strings.TrimSpace(o.NodeID) == "" {
		return fmt.Errorf("--node-id 不能为空（默认 edge-<主机名>，也可显式指定如 --node-id=edge-worker-01）")
	}
	if strings.ContainsAny(o.NodeID, " \t\n") {
		return fmt.Errorf("--node-id=%q 含空白字符，节点 ID 不允许空格", o.NodeID)
	}
	// M4C P2-①：NodeID 字符白名单（字母/数字/-/_）。nodeID 会写入 systemd
	// EnvironmentFile 与容器名等位置，限制字符集可防注入（如 $ 展开）与
	// 非法容器名（docker 名称仅允许 [a-zA-Z0-9][a-zA-Z0-9_.-]）。
	for _, r := range o.NodeID {
		valid := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' || r == '-' || r == '_'
		if !valid {
			return fmt.Errorf("--node-id=%q 含非法字符 %q（仅允许字母/数字/连字符/下划线，如 edge-worker-01）",
				o.NodeID, r)
		}
	}
	return nil
}

// defaultNodeID 与 edgehub.DefaultNodeID 的兜底逻辑对齐：edge-<主机名>，失败时 edge-local。
//
// 注意（P2-① 回归修复）：主机名可能含白名单外字符（macOS 主机名如
// "MacdeMacBook-Pro.local" 含点号），直接拼接会被 join 校验拒绝。
// 这里先把主机名清洗为白名单字符集（非法字符替换为 '-'），
// 保证默认值始终可用；显式传入的 --node-id 仍走严格校验。
func defaultNodeID() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		cleaned := sanitizeNodeIDChars(h)
		if cleaned == "" {
			return "edge-local"
		}
		return "edge-" + cleaned
	}
	return "edge-local"
}

// sanitizeNodeIDChars 把非白名单字符（非字母/数字/连字符/下划线）替换为连字符，
// 并压缩连续连字符（保持默认 node-id 可读）。
func sanitizeNodeIDChars(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		valid := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' || r == '-' || r == '_'
		if valid {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// cloudAddr 生成 edgecore 的云端地址：ws://<ip>:<port>/v1/edge，
// TLS 启用时用 wss://（/v1/edge 是与 CloudHub 契约固定的通道路径）。
func (o joinOptions) cloudAddr() string {
	scheme := "ws"
	if o.TLS {
		scheme = "wss"
	}
	host := o.CloudCoreIP
	if strings.Contains(host, ":") { // IPv6 需要加方括号
		host = "[" + host + "]"
	}
	return fmt.Sprintf("%s://%s:%s/v1/edge", scheme, host, o.CloudCorePort)
}

// edgeEnvTemplate 生成 edgecore.env：键名与 edgehub/metamanager/eventbus
// 读取的环境变量一一对应；EDGEFLOW_EDGECORE_TOKEN 为接入令牌（WBS 7.3 设备认证，edgecore 注册时携带）。
// 首行下的版本标记行（envVersionLine）供 upgrade 更新 / rollback 恢复（WBS 10.2）。
var edgeEnvTemplate = template.Must(template.New("edgecore.env").Parse(`# 由 keadm join 生成（EdgeFlow WBS 8.6）。安装到 {{ .EnvPath }}。
# 键名与 edgecore 读取的环境变量一一对应，请勿修改。
{{ .VersionLine }}

# 边缘节点 ID（唯一标识本节点，云端注册用）
EDGEFLOW_EDGECORE_NODE_ID={{ .NodeID }}

# 云端 CloudHub 地址（/v1/edge 为云边通道路径，与 CloudHub 契约固定）
EDGEFLOW_EDGECORE_CLOUD_ADDR={{ .CloudAddr }}

# 元数据 SQLite 路径（绝对路径，避免 systemd 相对 cwd 歧义）
EDGEFLOW_EDGECORE_DB_PATH={{ .DBPath }}

# 边缘 MQTT broker（设备数据面接入点，与 EventBus 默认值一致）
EDGEFLOW_EDGECORE_MQTT_ADDR=tcp://127.0.0.1:1883

# 接入令牌（WBS 7.3 设备认证：edgecore 注册时携带，云端 EDGEFLOW_CLOUDCORE_NODE_TOKEN 启用时校验）
EDGEFLOW_EDGECORE_TOKEN={{ .Token }}
{{- if .TLS }}

# mTLS：edgecore 首次运行自动生成/加载证书（幂等），地址自动升级 wss://
EDGEFLOW_EDGECORE_TLS=on
EDGEFLOW_EDGECORE_CERT_DIR={{ .CertDir }}
{{- end }}
`))

// renderEdgeEnv 渲染 edgecore.env 文本。
func renderEdgeEnv(o joinOptions) []byte {
	var buf strings.Builder
	if err := edgeEnvTemplate.Execute(&buf, struct {
		NodeID      string
		CloudAddr   string
		DBPath      string
		Token       string
		TLS         bool
		CertDir     string
		EnvPath     string
		VersionLine string
	}{
		NodeID:      o.NodeID,
		CloudAddr:   o.cloudAddr(),
		DBPath:      edgeDBPath,
		Token:       o.Token,
		TLS:         o.TLS,
		CertDir:     edgeCertDir,
		EnvPath:     edgeEnvPath,
		VersionLine: envVersionLine(edgeEnvVersion),
	}); err != nil {
		// 模板是编译期固定的，运行期不会失败；失败时返回错误文本兜底。
		return []byte("# 渲染 edgecore.env 失败: " + err.Error() + "\n")
	}
	return []byte(buf.String())
}

// renderEdgeService 生成 edgecore.service（systemd 单元）。
func renderEdgeService(o joinOptions) []byte {
	return []byte(fmt.Sprintf(`# 由 keadm join 生成（EdgeFlow WBS 8.6）。安装到 /etc/systemd/system/edgecore.service。
[Unit]
Description=EdgeFlow EdgeCore（边缘计算核心：EdgeHub + MetaManager + Edged）
Documentation=https://github.com/edgeflow/edgeflow
# 等待网络就绪：edgecore 需要连接云端 CloudHub
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# 环境变量文件（keadm join 生成，路径固定）
EnvironmentFile=%s
# 二进制由 install.sh 安装（交叉编译 linux/arm64 或 linux/amd64）
ExecStart=%s
# 工作目录固定为数据目录（SQLite 落盘位置，见 env 中 DB_PATH）
WorkingDirectory=%s
# 断线自动重启：云边通道有指数退避重连，崩溃则由 systemd 拉起
Restart=on-failure
RestartSec=5
# 安全加固
NoNewPrivileges=true
PrivateTmp=true
# 日志交给 journald（journalctl -u edgecore -f 查看）
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
`, edgeEnvPath, edgeBinPath, edgeWorkDir))
}

// renderInstallScript 生成 install.sh：在边缘节点以 root 执行。
func renderInstallScript(o joinOptions) []byte {
	return []byte(fmt.Sprintf(`#!/usr/bin/env bash
# 由 keadm join 生成（EdgeFlow WBS 8.6）。在边缘节点以 root 执行：
#   sudo ./install.sh
# 前置条件：本目录已放置 edgecore 二进制（与节点架构匹配，
# 交叉编译命令见 docs/KEADM.md）。
set -euo pipefail

echo "==> [1/4] 安装 edgecore 二进制"
install -m 0755 edgecore %s

echo "==> [2/4] 创建目录并安装环境变量文件"
install -d -m 0755 %s %s
install -m 0600 edgecore.env %s
%s

echo "==> [3/4] 安装 systemd 单元"
install -m 0644 edgecore.service /etc/systemd/system/edgecore.service
systemctl daemon-reload

echo "==> [4/4] 启动 edgecore"
systemctl enable --now edgecore
systemctl status edgecore --no-pager || true

echo
echo "安装完成。查看日志: journalctl -u edgecore -f"
`, edgeBinPath, edgeConfDir, edgeWorkDir, edgeEnvPath, installCertDirLine(o)))
}

// installCertDirLine 生成 install.sh 中证书目录的创建语句（仅 TLS 时）。
func installCertDirLine(o joinOptions) string {
	if o.TLS {
		return fmt.Sprintf("install -d -m 0700 %s", edgeCertDir)
	}
	return "# 未启用 mTLS，跳过证书目录创建"
}

// renderJoinReadme 生成 README.md：接入步骤说明与手动安装片段（供无脚本环境参考）。
func renderJoinReadme(o joinOptions) []byte {
	tlsLine := ""
	if o.TLS {
		tlsLine = "（mTLS 已启用：edgecore 首次运行自动生成客户端证书）"
	}
	return []byte(fmt.Sprintf(`# 边缘节点接入说明（由 keadm join 生成）

本目录产物用于把一台边缘节点接入 EdgeFlow 云端（CloudHub: %s）%s。

## 产物清单

| 文件 | 用途 |
| --- | --- |
| edgecore.env | 环境变量文件（NODE_ID / CLOUD_ADDR / DB_PATH / MQTT_ADDR / TLS / CERT_DIR / TOKEN） |
| edgecore.service | systemd 单元（EnvironmentFile + ExecStart + 重启策略） |
| install.sh | 一键安装脚本（需 root，依赖同目录的 edgecore 二进制） |
| README.md | 本说明 |

## 安装步骤

1. 准备 edgecore 二进制（与节点架构匹配，linux/amd64 或 linux/arm64）：
   - 方式一：从发布包获取；
   - 方式二：源码交叉编译：
       GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o edgecore ./cmd/edgecore
2. 把本目录全部文件 + edgecore 二进制拷贝到边缘节点（scp 等）。
3. 以 root 执行安装脚本：
       sudo ./install.sh
4. 验证：
       systemctl status edgecore
       journalctl -u edgecore -f

## 手动安装片段（无法直接跑脚本时的等价步骤）

    install -m 0755 edgecore /usr/local/bin/edgecore
    install -d -m 0755 /etc/edgeflow /var/lib/edgeflow
    install -m 0600 edgecore.env /etc/edgeflow/edgecore.env
    install -m 0644 edgecore.service /etc/systemd/system/edgecore.service
    systemctl daemon-reload
    systemctl enable --now edgecore

## 关键配置

- 节点 ID: %s
- 云端地址: %s
- 元数据库: %s
- 接入令牌: %s（WBS 7.3 设备认证：edgecore 注册携带，云端启用时校验）

## 排障

- edgecore 起不来：journalctl -u edgecore -e 查看；确认网络可达 %s。
- 想更换节点 ID：改 /etc/edgeflow/edgecore.env 后 systemctl restart edgecore。
- 想卸载：systemctl disable --now edgecore && rm /etc/systemd/system/edgecore.service /etc/edgeflow/edgecore.env
`, o.cloudAddr(), tlsLine, o.NodeID, o.cloudAddr(), edgeDBPath, o.Token, o.CloudCoreIP))
}
