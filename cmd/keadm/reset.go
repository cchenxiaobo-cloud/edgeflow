package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// resetOptions 是 keadm reset 的参数集合。
type resetOptions struct {
	// OutputDir 是待清理的产物目录。
	OutputDir string
	// Force 跳过删除确认（供脚本/CI 使用）。
	Force bool
}

// generatedFileNames 是 keadm 各子命令生成的全部产物文件名
// （reset 只清理这份清单内的文件，绝不碰目录里的其他内容）。
var generatedFileNames = append(append([]string{}, initOutputs...), joinOutputs...)

// runReset 实现 keadm reset：清理 output-dir 下的 keadm 生成产物。
// 幂等：目录不存在或没有 keadm 产物时提示后以 0 退出。
func runReset(args []string, stdout, stderr io.Writer, stdin io.Reader) int {
	fs := newFlagSet("reset", stderr)
	opts := resetOptions{OutputDir: "./keadm-out"}
	fs.StringVar(&opts.OutputDir, "output-dir", opts.OutputDir, "产物输出目录")
	fs.BoolVar(&opts.Force, "force", false, "跳过删除确认")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "错误: reset 不接受位置参数 %q\n", fs.Arg(0))
		return exitUsage
	}

	// 幂等：目录不存在直接成功返回。
	entries, err := os.ReadDir(opts.OutputDir)
	if err != nil {
		if os.IsNotExist(err) {
			_, _ = fmt.Fprintf(stdout, "reset: %s 不存在，无可清理产物（幂等）\n", opts.OutputDir)
			return exitOK
		}
		_, _ = fmt.Fprintf(stderr, "错误: 读取目录 %s 失败: %v\n", opts.OutputDir, err)
		return exitRuntime
	}

	// 只收集 keadm 生成的文件，其他文件一律不动。
	var toDelete []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		for _, name := range generatedFileNames {
			if e.Name() == name {
				toDelete = append(toDelete, filepath.Join(opts.OutputDir, e.Name()))
				break
			}
		}
	}
	if len(toDelete) == 0 {
		_, _ = fmt.Fprintf(stdout, "reset: %s 下没有 keadm 生成产物，无需清理（幂等）\n", opts.OutputDir)
		return exitOK
	}

	// 删除前确认（--force 跳过）。
	if !opts.Force && !confirmDelete(stdout, stderr, stdin, toDelete) {
		_, _ = fmt.Fprintln(stdout, "reset: 已取消，未删除任何文件")
		return exitOK
	}

	// 逐个删除（全部是普通文件，直接 os.Remove）。
	for _, p := range toDelete {
		if err := os.Remove(p); err != nil {
			_, _ = fmt.Fprintf(stderr, "错误: 删除 %s 失败: %v\n", p, err)
			return exitRuntime
		}
	}

	// 目录若已空则一并移除（非空时保留，不报错——里面可能有用户自己的文件）。
	_ = os.Remove(opts.OutputDir) // 非空时返回错误，忽略即可

	_, _ = fmt.Fprintf(stdout, "reset: 已删除 %d 个 keadm 生成产物: %s\n", len(toDelete), opts.OutputDir)
	return exitOK
}

// confirmDelete 向用户列出将删除的文件并请求确认（y/yes 才删除）。
func confirmDelete(stdout, stderr io.Writer, stdin io.Reader, files []string) bool {
	if stdin == nil {
		_, _ = fmt.Fprintln(stderr, "错误: 无法读取确认输入（无 stdin），请使用 --force 跳过确认")
		return false
	}
	_, _ = fmt.Fprintln(stdout, "将删除以下 keadm 生成产物:")
	for _, f := range files {
		_, _ = fmt.Fprintf(stdout, "  - %s\n", f)
	}
	_, _ = fmt.Fprint(stdout, "确认删除? [y/N] ")

	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && line == "" {
		_, _ = fmt.Fprintln(stdout, "")
		_, _ = fmt.Fprintln(stdout, "未收到确认输入，已取消")
		return false
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes"
}
