// keadm 是 EdgeFlow 的安装管理 CLI（对标 KubeEdge 的 keadm，WBS 8.6）。
//
// 当前为「基础版」：只做离线产物生成，不在本机直接操作集群/节点——
//   - keadm init  生成云端部署产物（可直接 kubectl apply -f 的 cloudcore.yaml +
//     部署说明 NOTES.txt），并给出 Helm Chart 替代路径；
//   - keadm join  生成边缘接入产物（edgecore.env 环境变量文件 + edgecore.service
//     systemd 单元 + install.sh 安装脚本 + 接入说明 README.md）；
//   - keadm reset 清理 output-dir 下由 keadm 生成的产物（确认后删除，幂等）；
//   - keadm version 输出版本信息（复用 pkg/version）。
//
// 实现约束：仅依赖标准库（flag 解析子命令，无 Cobra/urfave）。
// 退出码约定：0 成功；1 产物生成/IO 等运行时错误；2 参数/用法错误。
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"edgeflow/pkg/version"
)

// usageText 是 keadm 的顶层用法说明。
const usageText = `keadm 是 EdgeFlow 的安装管理 CLI（WBS 8.6 基础版）。

用法:
  keadm <command> [flags]

命令:
  init     生成云端部署产物（cloudcore.yaml + NOTES.txt）
  join     生成边缘接入产物（edgecore.env + edgecore.service + install.sh）
  reset    清理 output-dir 下的 keadm 生成产物（确认后删除，幂等）
  version  打印版本信息

全局帮助:
  keadm -h | --help     打印本帮助
  keadm <command> -h    查看子命令参数

示例:
  keadm init --tls --output-dir=./keadm-out
  keadm join --cloudcore-ip=192.168.1.10 --token=abc123 --tls --output-dir=./keadm-out
  keadm reset --output-dir=./keadm-out --force
`

// exit 退出码约定（见包注释）。
const (
	exitOK      = 0 // 成功
	exitRuntime = 1 // 运行时错误（IO、生成失败等）
	exitUsage   = 2 // 参数/用法错误
)

func main() {
	if code := run(os.Args[1:], os.Stdout, os.Stderr, os.Stdin); code != 0 {
		os.Exit(code)
	}
}

// run 是可测试入口：解析子命令并分发，返回进程退出码。
// stdin 仅 reset 确认删除时使用。
func run(args []string, stdout, stderr io.Writer, stdin io.Reader) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		fmt.Fprintln(stderr, "错误: 缺少子命令（init/join/reset/version）")
		return exitUsage
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "init":
		return runInit(rest, stdout, stderr)
	case "join":
		return runJoin(rest, stdout, stderr)
	case "reset":
		return runReset(rest, stdout, stderr, stdin)
	case "version":
		return runVersion(rest, stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usageText)
		return exitOK
	default:
		fmt.Fprintf(stderr, "错误: 未知子命令 %q\n\n", cmd)
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}
}

// newFlagSet 创建子命令统一的 flag 解析器：
//   - ContinueOnError：解析失败不退出进程，由调用方决定返回码；
//   - 输出统一走 stderr，保证 stdout 只出现正常结果。
func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// runVersion 实现 keadm version：复用 pkg/version 输出编译期注入的版本信息。
// 支持 --json 输出结构化版本信息（供脚本解析）。
func runVersion(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("version", stderr)
	asJSON := fs.Bool("json", false, "以 JSON 格式输出版本信息")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "错误: version 不接受位置参数 %q\n", fs.Arg(0))
		return exitUsage
	}

	info := version.Get()
	if *asJSON {
		b, err := jsonMarshalIndent(info)
		if err != nil {
			fmt.Fprintf(stderr, "错误: 序列化版本信息失败: %v\n", err)
			return exitRuntime
		}
		fmt.Fprintln(stdout, string(b))
		return exitOK
	}
	fmt.Fprintf(stdout, "keadm %s\n", info.String())
	return exitOK
}
