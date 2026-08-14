// keadm ops-ledger：查询操作台账（WBS 10.2，见 docs/UPGRADE.md）。
//
// 台账文件为 output-dir/ops-ledger.jsonl（逐行 JSON，追加写不覆盖历史），
// 本命令按行解析后输出最近 N 条（默认 20）为 JSON 数组（机器可解析）。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// opsLedgerOptions 是 keadm ops-ledger 的参数集合。
type opsLedgerOptions struct {
	// OutputDir 是产物输出目录（台账位于其下）。
	OutputDir string
	// Limit 是输出的最近记录条数（默认 20）。
	Limit int
}

// runOpsLedger 实现 keadm ops-ledger：输出最近 N 条台账记录（JSON 数组）。
// 台账不存在时输出空数组并以 0 退出（查询空台账不是错误）。
func runOpsLedger(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("ops-ledger", stderr)
	opts := opsLedgerOptions{OutputDir: "./keadm-out", Limit: 20}
	fs.StringVar(&opts.OutputDir, "output-dir", opts.OutputDir, "产物输出目录")
	fs.IntVar(&opts.Limit, "limit", opts.Limit, "输出最近 N 条记录（默认 20）")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "错误: ops-ledger 不接受位置参数 %q\n", fs.Arg(0))
		return exitUsage
	}
	if opts.Limit <= 0 {
		opts.Limit = 20
	}

	ledgerPath := filepath.Join(opts.OutputDir, ledgerFile)
	b, err := os.ReadFile(ledgerPath)
	if err != nil {
		if os.IsNotExist(err) {
			_, _ = fmt.Fprintln(stdout, "[]")
			_, _ = fmt.Fprintf(stderr, "提示: 台账 %s 不存在（尚无 upgrade/rollback 操作记录）\n", ledgerPath)
			return exitOK
		}
		_, _ = fmt.Fprintf(stderr, "错误: 读取台账失败: %v\n", err)
		return exitRuntime
	}

	// 逐行解析：跳过空行与损坏行（附警告），保留最近 N 条。
	var entries []json.RawMessage
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !json.Valid([]byte(line)) {
			_, _ = fmt.Fprintf(stderr, "提示: 台账第 %d 行不是合法 JSON，已跳过\n", i+1)
			continue
		}
		entries = append(entries, json.RawMessage(line))
	}
	if len(entries) > opts.Limit {
		entries = entries[len(entries)-opts.Limit:]
	}

	out, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "错误: 序列化台账失败: %v\n", err)
		return exitRuntime
	}
	_, _ = fmt.Fprintln(stdout, string(out))
	return exitOK
}
