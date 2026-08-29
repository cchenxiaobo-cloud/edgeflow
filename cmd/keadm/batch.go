package main

// keadm batch —— 批量操作（WBS 8.6 批量 join/upgrade/rollback）。
//
// 设计：复用 runJoin/runUpgrade/runRollback 的完整参数校验与执行逻辑
// （不复制实现），batch 只负责「读清单 → 逐节点构造子命令参数 → 汇总」。
// 失败语义按操作区分：
//   - join / rollback：任一节点失败不中断其余节点（逐个尝试），
//     最终以退出码反映是否有失败；
//   - upgrade：灰度发布语义（WBS 10.2）——按 --batch-size 分组逐批升级、
//     批间暂停 --pause-between、任一节点失败立即中止（fail-fast），
//     报告成功/失败/未执行清单并提示用 rollback 恢复（不自动回滚）。
//
// 清单文件格式（每行一个节点，空行与 # 注释行跳过）：
//   - join：    <node-id> 或 <node-id>,<cloudcore-ip>（IP 缺省用 --cloudcore-ip）
//   - upgrade： <output-dir>（该节点的产物目录，keadm-out/<node-id>）
//   - rollback：<output-dir>
//
// 示例：
//   keadm batch --op=join --file=nodes.txt --cloudcore-ip=192.168.1.10 --token-file=/path/token
//   keadm batch --op=join --file=nodes.txt --cloudcore-ip=192.168.1.10 --token=abc123
//   keadm batch --op=upgrade --file=dirs.txt --version=v0.2.0
//   keadm batch --op=upgrade --file=dirs.txt --version=v0.2.0 --batch-size=10 --pause-between=30s
//   keadm batch --op=rollback --file=dirs.txt --latest

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// batchOptions 是 keadm batch 的参数集合。
type batchOptions struct {
	Op            string // join | upgrade | rollback
	File          string // 节点清单文件
	CloudCoreIP   string // join：云端 IP（清单行可覆盖）
	CloudCorePort string // join：CloudHub 端口
	Token         string // join：接入令牌（--token 直传；生产建议 --token-file）
	TokenFile     string // join：接入令牌文件路径（SEC-03，v0.23.0）；与 --token 同给时本参数优先
	TLS           bool   // join：启用 mTLS
	OutputDir     string // join：批量产物根目录（每节点 <根>/<node-id>/）；upgrade/rollback 用清单行
	Version       string // upgrade：目标版本
	Latest        bool   // rollback：回滚到最新有效备份
	BackupID      string // rollback：指定备份 id
	// BatchSize / PauseBetween 是 upgrade 灰度发布分批参数：
	// 每 batchSize 个节点一批，批间暂停 PauseBetween（默认 0 不暂停），
	// 任一节点失败即中止（fail-fast）。join/rollback 不接受这两个参数。
	BatchSize    int
	PauseBetween time.Duration
}

// runBatch 是 batch 子命令入口。
func runBatch(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("batch", stderr)
	opts := batchOptions{CloudCorePort: "10000", OutputDir: "./keadm-out"}
	fs.StringVar(&opts.Op, "op", "", "批量操作类型: join|upgrade|rollback（必填）")
	fs.StringVar(&opts.File, "file", "", "节点清单文件路径（必填；每行一个节点）")
	fs.StringVar(&opts.CloudCoreIP, "cloudcore-ip", "", "云端 CloudHub 节点 IP（join 必填；清单行可覆盖）")
	fs.StringVar(&opts.CloudCorePort, "cloudcore-port", opts.CloudCorePort, "CloudHub 端口（join）")
	fs.StringVar(&opts.Token, "token", "", "接入令牌（join 必填；生产建议 --token-file）")
	fs.StringVar(&opts.TokenFile, "token-file", "", "接入令牌文件路径（join，SEC-03，v0.23.0：从文件读 token；与 --token 同给时本参数优先）")
	fs.BoolVar(&opts.TLS, "tls", false, "启用云边 mTLS（join）")
	fs.StringVar(&opts.OutputDir, "output-dir", opts.OutputDir, "批量产物根目录（join 时每个节点生成 <根>/<node-id>/）")
	fs.StringVar(&opts.Version, "version", "", "目标版本 vX.Y.Z（upgrade 必填）")
	fs.BoolVar(&opts.Latest, "latest", false, "回滚到最新有效备份（rollback）")
	fs.StringVar(&opts.BackupID, "backup", "", "指定备份 id（rollback，与 --latest 互斥）")
	fs.IntVar(&opts.BatchSize, "batch-size", 1, "分批大小（upgrade：每批升级节点数，灰度发布，默认 1）")
	fs.DurationVar(&opts.PauseBetween, "pause-between", 0, "批间暂停时长（upgrade，如 30s；默认 0 不暂停）")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "错误: batch 不接受位置参数 %q\n", fs.Arg(0))
		return exitUsage
	}

	// 分批参数只属于 upgrade（灰度发布）：显式传给了 join/rollback 则拒绝；
	// upgrade 场景下校验取值（batch-size>=1，pause 非负）。
	explicitBatch := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "batch-size" || f.Name == "pause-between" {
			explicitBatch = true
		}
	})
	if opts.Op != "upgrade" && explicitBatch {
		_, _ = fmt.Fprintln(stderr, "错误: --batch-size/--pause-between 仅 upgrade 批量操作支持（灰度发布）")
		return exitUsage
	}
	if opts.Op == "upgrade" {
		if opts.BatchSize < 1 {
			_, _ = fmt.Fprintf(stderr, "错误: --batch-size 必须 >= 1（默认 1），收到 %d\n", opts.BatchSize)
			return exitUsage
		}
		if opts.PauseBetween < 0 {
			_, _ = fmt.Fprintf(stderr, "错误: --pause-between 不能为负（如 30s），收到 %s\n", opts.PauseBetween)
			return exitUsage
		}
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
	// join：端口校验（SEC-08，与 join 单节点同规则）+ token 来源处理
	// （--token-file 优先读文件，缺省回落 --token，语义与 join 完全一致）。
	if opts.Op == "join" {
		if err := validateCloudCorePort(opts.CloudCorePort); err != nil {
			_, _ = fmt.Fprintf(stderr, "错误: %v\n", err)
			return exitUsage
		}
		// 产物目录权限（SEC-03 附，v0.24.0）：批量 join 入口同样 fail-fast，
		// 避免逐节点处理到一半才因非法 EDGEFLOW_JOIN_DIR_MODE 失败。
		if _, err := resolveJoinDirMode(); err != nil {
			_, _ = fmt.Fprintf(stderr, "错误: %v\n", err)
			return exitUsage
		}
		if opts.TokenFile != "" {
			b, err := os.ReadFile(opts.TokenFile)
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "错误: 读取 --token-file=%s 失败: %v\n", opts.TokenFile, err)
				return exitUsage
			}
			if t := strings.TrimSpace(string(b)); t != "" {
				opts.Token = t
			}
		}
		if strings.TrimSpace(opts.Token) == "" {
			_, _ = fmt.Fprintln(stderr, "错误: join 批量操作缺少 --token（接入令牌；生产建议 --token-file=<路径>）")
			return exitUsage
		}
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
	if opts.Op == "upgrade" && (opts.BatchSize > 1 || opts.PauseBetween > 0) {
		_, _ = fmt.Fprintf(stdout, "灰度分批：batch-size=%d，批间暂停=%s（fail-fast：任一节点失败即中止）\n",
			opts.BatchSize, opts.PauseBetween)
	}

	if opts.Op == "upgrade" {
		// 灰度发布：分批逐批升级，批间暂停，任一节点失败即中止（不自动回滚）。
		ok, failed, succeeded, failedNodes, aborted := runUpgradeBatches(
			opts, lines, runBatchUpgrade, time.Sleep, stdout, stderr)
		if failed > 0 {
			_, _ = fmt.Fprintf(stdout, "批量 upgrade 中止：成功 %d，失败 %d，未执行 %d（fail-fast，灰度发布语义）\n",
				ok, failed, len(aborted))
			_, _ = fmt.Fprintln(stdout, "  成功节点:")
			for _, l := range succeeded {
				_, _ = fmt.Fprintf(stdout, "    - %s\n", l)
			}
			_, _ = fmt.Fprintln(stdout, "  失败节点:")
			for _, l := range failedNodes {
				_, _ = fmt.Fprintf(stdout, "    - %s\n", l)
			}
			if len(aborted) > 0 {
				_, _ = fmt.Fprintln(stdout, "  未执行（已中止）:")
				for _, l := range aborted {
					_, _ = fmt.Fprintf(stdout, "    - %s\n", l)
				}
			}
			_, _ = fmt.Fprintln(stdout, "  提示: 不自动回滚；对失败节点可执行 keadm rollback --latest --output-dir=<目录> 恢复")
			_, _ = fmt.Fprintln(stdout, "        （各节点备份 id 见上方输出与 ops-ledger 台账）")
			return exitRuntime
		}
		_, _ = fmt.Fprintf(stdout, "批量 upgrade 完成：成功 %d，失败 %d\n", ok, failed)
		return exitOK
	}

	ok, failed := 0, 0
	for i, line := range lines {
		_, _ = fmt.Fprintf(stdout, "[%d/%d] %s …\n", i+1, len(lines), describe(opts.Op, line))
		var code int
		switch opts.Op {
		case "join":
			code = runBatchJoin(opts, line, stdout, stderr)
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

// upgradeExecutor 执行单节点 upgrade（可注入 fake 执行器测试分批逻辑，
// 避免真实升级；生产实现为 runBatchUpgrade）。
type upgradeExecutor func(opts batchOptions, line string, stdout, stderr io.Writer) int

// batchSleeper 批间暂停（可注入 fake 避免测试真实等待；生产实现为 time.Sleep）。
type batchSleeper func(d time.Duration)

// runUpgradeBatches 以灰度发布语义分批执行 upgrade 清单（WBS 10.2）：
//
//   - 每 batchSize 个节点为一批（batchSize<1 时按 1 处理），批内顺序执行；
//   - 批间调用 sleep(pause) 暂停（pause<=0 不暂停）；
//   - 任一节点失败立即中止（fail-fast），剩余节点计入 aborted；
//   - 不自动回滚：失败节点的产物状态由调用方提示用 keadm rollback 恢复
//     （各节点 upgrade 内部已各自备份 + 落台账，与既有 rollback 语义衔接）。
//
// 返回成功/失败计数与 成功/失败/未执行 三个节点清单。
func runUpgradeBatches(opts batchOptions, lines []string, exec upgradeExecutor, sleep batchSleeper,
	stdout, stderr io.Writer) (ok, failed int, succeeded, failedNodes, aborted []string) {
	batchSize := opts.BatchSize
	if batchSize < 1 {
		batchSize = 1
	}
	total := len(lines)
	batches := (total + batchSize - 1) / batchSize

	for start := 0; start < total; start += batchSize {
		end := start + batchSize
		if end > total {
			end = total
		}
		batchNo := start/batchSize + 1
		if opts.BatchSize > 1 {
			_, _ = fmt.Fprintf(stdout, "批次 %d/%d（节点 %d-%d/%d）\n", batchNo, batches, start+1, end, total)
		}
		for i := start; i < end; i++ {
			line := lines[i]
			_, _ = fmt.Fprintf(stdout, "[%d/%d] %s …\n", i+1, total, describe("upgrade", line))
			if code := exec(opts, line, stdout, stderr); code == exitOK {
				ok++
				succeeded = append(succeeded, line)
				_, _ = fmt.Fprintf(stdout, "  ✅ %s 成功\n", line)
			} else {
				failed++
				failedNodes = append(failedNodes, line)
				_, _ = fmt.Fprintf(stderr, "  ❌ %s 失败（退出码 %d）\n", line, code)
				_, _ = fmt.Fprintln(stderr, "  已中止后续批次（fail-fast，灰度发布语义；不自动回滚，可用 keadm rollback 恢复失败节点）")
				aborted = append(aborted, lines[end:]...)
				return ok, failed, succeeded, failedNodes, aborted
			}
		}
		if end < total {
			if opts.PauseBetween > 0 {
				_, _ = fmt.Fprintf(stdout, "批次 %d 完成，暂停 %s 后继续…\n", batchNo, opts.PauseBetween)
			} else {
				_, _ = fmt.Fprintf(stdout, "批次 %d 完成，继续…\n", batchNo)
			}
			sleep(opts.PauseBetween)
		}
	}
	return ok, failed, succeeded, failedNodes, aborted
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

// runBatchUpgrade 对清单行（output-dir）执行一次 upgrade；
// 透传灰度分批参数（--batch-size/--pause-between，非默认值时），
// 便于脚本对单节点与批量模式统一传参（单节点升级无分批效果）。
func runBatchUpgrade(opts batchOptions, line string, stdout, stderr io.Writer) int {
	args := []string{"--output-dir=" + line, "--version=" + opts.Version}
	if opts.BatchSize != 1 {
		args = append(args, "--batch-size="+strconv.Itoa(opts.BatchSize))
	}
	if opts.PauseBetween != 0 {
		args = append(args, "--pause-between="+opts.PauseBetween.String())
	}
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
