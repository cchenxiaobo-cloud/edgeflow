package main

// keadm batch —— 批量操作（WBS 8.6 批量 join/upgrade/rollback）。
//
// 设计：复用 runJoin/runUpgrade/runRollback 的完整参数校验与执行逻辑
// （不复制实现），batch 只负责「读清单 → 逐节点构造子命令参数 → 汇总」。
// 任一节点失败不中断其余节点（逐个尝试），最终以退出码反映是否有失败。
//
// 清单文件格式（每行一个节点，空行与 # 注释行跳过）：
//   - join：    <node-id> 或 <node-id>,<cloudcore-ip>（IP 缺省用 --cloudcore-ip）
//   - upgrade： <output-dir>（该节点的产物目录，keadm-out/<node-id>）
//   - rollback：<output-dir>
//
// 示例：
//   keadm batch --op=join --file=nodes.txt --cloudcore-ip=192.168.1.10 --token=abc123
//   keadm batch --op=upgrade --file=dirs.txt --version=v0.2.0
//   keadm batch --op=rollback --file=dirs.txt --latest

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// batchOptions 是 keadm batch 的参数集合。
type batchOptions struct {
	Op           string // join | upgrade | rollback
	File         string // 节点清单文件
	CloudCoreIP  string // join：云端 IP（清单行可覆盖）
	CloudCorePort string // join：CloudHub 端口
	Token        string // join：接入令牌
	TLS          bool   // join：启用 mTLS
	OutputDir    string // join：批量产物根目录（每节点 <根>/<node-id>/）；upgrade/rollback 用清单行
	Version      string // upgrade：目标版本
	Latest       bool   // rollback：回滚到最新有效备份
	BackupID     string // rollback：指定备份 id
}

// runBatch 是 batch 子命令入口。
func runBatch(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("batch", stderr)
	opts := batchOptions{CloudCorePort: "10000", OutputDir: "./keadm-out"}
	fs.StringVar(&opts.Op, "op", "", "批量操作类型: join|upgrade|rollback（必填）")
	fs.StringVar(&opts.File, "file", "", "节点清单文件路径（必填；每行一个节点）")
	fs.StringVar(&opts.CloudCoreIP, "cloudcore-ip", "", "云端 CloudHub 节点 IP（join 必填；清单行可覆盖）")
	fs.StringVar(&opts.CloudCorePort, "cloudcore-port", opts.CloudCorePort, "CloudHub 端口（join）")
	fs.StringVar(&opts.Token, "token", "", "接入令牌（join 必填）")
	fs.BoolVar(&opts.TLS, "tls", false, "启用云边 mTLS（join）")
	fs.StringVar(&opts.OutputDir, "output-dir", opts.OutputDir, "批量产物根目录（join 时每个节点生成 <根>/<node-id>/）")
	fs.StringVar(&opts.Version, "version", "", "目标版本 vX.Y.Z（upgrade 必填）")
	fs.BoolVar(&opts.Latest, "latest", false, "回滚到最新有效备份（rollback）")
	fs.StringVar(&opts.BackupID, "backup", "", "指定备份 id（rollback，与 --latest 互斥）")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "错误: batch 不接受位置参数 %q\n", fs.Arg(0))
		return exitUsage
	}

	switch opts.Op {
	case "join", "upgrade", "rollback":
	default:
		_, _ = fmt.Fprintf(stderr, "错误: --op 必须为 join|upgrade|rollback，收到 %q\n", opts.Op)
		return exitUsage
	}
	if strings.TrimSpace(opts.File) == "" {
		_, _ = fmt.Fprintln(stderr, "错误: 缺少必填参数 --file（节点清单文件）")
		return exitUsage
	}
	if opts.Op == "join" && strings.TrimSpace(opts.Token) == "" {
		_, _ = fmt.Fprintln(stderr, "错误: join 批量操作缺少 --token（接入令牌）")
		return exitUsage
	}
	if opts.Op == "upgrade" && strings.TrimSpace(opts.Version) == "" {
		_, _ = fmt.Fprintln(stderr, "错误: upgrade 批量操作缺少 --version（目标版本）")
		return exitUsage
	}

	lines, err := readNodeList(opts.File)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "错误: 读取清单文件 %s 失败: %v\n", opts.File, err)
		return exitUsage
	}
	if len(lines) == 0 {
		_, _ = fmt.Fprintf(stderr, "错误: 清单文件 %s 为空（或全部为注释/空行）\n", opts.File)
		return exitUsage
	}

	_, _ = fmt.Fprintf(stdout, "批量 %s：共 %d 个节点（清单 %s）\n", opts.Op, len(lines), opts.File)
	ok, failed := 0, 0
	for i, line := range lines {
		_, _ = fmt.Fprintf(stdout, "[%d/%d] %s …\n", i+1, len(lines), describe(opts.Op, line))
		var code int
		switch opts.Op {
		case "join":
			code = runBatchJoin(opts, line, stdout, stderr)
		case "upgrade":
			code = runBatchUpgrade(opts, line, stdout, stderr)
		case "rollback":
			code = runBatchRollback(opts, line, stdout, stderr)
		}
		if code == exitOK {
			ok++
			_, _ = fmt.Fprintf(stdout, "  ✅ %s 成功\n", line)
		} else {
			failed++
			_, _ = fmt.Fprintf(stderr, "  ❌ %s 失败（退出码 %d），继续处理其余节点\n", line, code)
		}
	}
	_, _ = fmt.Fprintf(stdout, "批量 %s 完成：成功 %d，失败 %d\n", opts.Op, ok, failed)
	if failed > 0 {
		return exitRuntime
	}
	return exitOK
}

// readNodeList 读取清单文件：跳过空行与 # 注释行，返回有效行（已去首尾空白）。
func readNodeList(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lines = append(lines, line)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return lines, nil
}

// describe 输出单节点操作的描述（日志用）。
func describe(op, line string) string {
	switch op {
	case "join":
		id := line
		if i := strings.IndexByte(line, ','); i >= 0 {
			id = line[:i]
		}
		return "join 节点 " + id
	default:
		return op + " " + line
	}
}

// runBatchJoin 对清单行（node-id 或 node-id,ip）执行一次 join。
func runBatchJoin(opts batchOptions, line string, stdout, stderr io.Writer) int {
	nodeID, ip := line, ""
	if i := strings.IndexByte(line, ','); i >= 0 {
		nodeID, ip = strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:])
	}
	if nodeID == "" {
		_, _ = fmt.Fprintf(stderr, "错误: 清单行 %q 的 node-id 为空\n", line)
		return exitUsage
	}
	if ip == "" {
		ip = opts.CloudCoreIP
	}
	args := []string{
		"--cloudcore-ip=" + ip,
		"--cloudcore-port=" + opts.CloudCorePort,
		"--token=" + opts.Token,
		"--node-id=" + nodeID,
		"--output-dir=" + filepath.Join(opts.OutputDir, nodeID),
	}
	if opts.TLS {
		args = append(args, "--tls")
	}
	return runJoin(args, stdout, stderr)
}

// runBatchUpgrade 对清单行（output-dir）执行一次 upgrade。
func runBatchUpgrade(opts batchOptions, line string, stdout, stderr io.Writer) int {
	args := []string{"--output-dir=" + line, "--version=" + opts.Version}
	return runUpgrade(args, stdout, stderr)
}

// runBatchRollback 对清单行（output-dir）执行一次 rollback。
func runBatchRollback(opts batchOptions, line string, stdout, stderr io.Writer) int {
	args := []string{"--output-dir=" + line}
	if opts.Latest {
		args = append(args, "--latest")
	} else {
		args = append(args, "--backup="+opts.BackupID)
	}
	return runRollback(args, stdout, stderr)
}
