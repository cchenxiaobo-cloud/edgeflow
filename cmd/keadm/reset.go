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
// 注：keadm-manifest.json 是产物校验清单（M4C P2-②），同为 keadm 生成，
// 随 reset 删除（删除前需先成功解析，见 runReset）。
var generatedFileNames = append(append([]string{manifestName}, initOutputs...), joinOutputs...)

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
	var candidates []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		for _, name := range generatedFileNames {
			if e.Name() == name {
				candidates = append(candidates, filepath.Join(opts.OutputDir, e.Name()))
				break
			}
		}
	}
	if len(candidates) == 0 {
		_, _ = fmt.Fprintf(stdout, "reset: %s 下没有 keadm 生成产物，无需清理（幂等）\n", opts.OutputDir)
		return exitOK
	}

	// M4C P2-② 修复：删除前校验文件来源。
	// 读校验清单（keadm-manifest.json，init/join 生成产物时写入文件名+sha256）：
	//   - 有清单：哈希匹配才删；不匹配/未登记 → 跳过并提示人工确认（防误删同名用户文件）；
	//   - 无清单（旧版本产物）：保持原白名单行为（确认后删除），提示无校验保护；
	//   - 清单损坏：保守处理——不删除任何文件，提示人工核对。
	m, hasManifest, loadErr := loadManifest(opts.OutputDir)
	if loadErr != nil {
		_, _ = fmt.Fprintf(stderr, "错误: %v；为防误删，本次不删除任何文件，请人工核对后处理\n", loadErr)
		return exitRuntime
	}

	var toDelete []string // 校验通过、待删除
	var skipped []string  // 校验不通过、跳过（提示人工确认）
	if hasManifest {
		for _, p := range candidates {
			name := filepath.Base(p)
			if name == manifestName {
				// 清单本身是 keadm 的账本文件：能被解析即视为 keadm 产物
				toDelete = append(toDelete, p)
				continue
			}
			ok, err := verifyManifestFile(opts.OutputDir, name, m)
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "错误: 校验 %s 失败: %v；跳过删除，请人工确认\n", p, err)
				skipped = append(skipped, p)
				continue
			}
			if ok {
				toDelete = append(toDelete, p)
			} else {
				skipped = append(skipped, p)
			}
		}
	} else {
		// 旧版本产物（无清单）：保持原有白名单行为，仅提示无校验保护
		toDelete = candidates
		_, _ = fmt.Fprintln(stderr, "警告: 未找到校验清单（旧版本 keadm 产物？），无法校验文件来源；")
		_, _ = fmt.Fprintln(stderr, "      将按文件名白名单删除，请确认目录中没有同名的用户文件")
	}

	if len(toDelete) == 0 {
		if len(skipped) > 0 {
			_, _ = fmt.Fprintf(stdout, "reset: %d 个文件校验不匹配已跳过（可能被修改或是同名用户文件），请人工确认:\n", len(skipped))
			for _, p := range skipped {
				_, _ = fmt.Fprintf(stdout, "  - %s\n", p)
			}
		}
		_, _ = fmt.Fprintf(stdout, "reset: %s 下没有可删除的 keadm 产物（幂等）\n", opts.OutputDir)
		return exitOK
	}

	// 删除前确认（--force 跳过）。注意：--force 只跳过 y/N 确认，
	// 不绕过哈希校验——校验不匹配的文件始终需要人工确认。
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
	if len(skipped) > 0 {
		_, _ = fmt.Fprintf(stdout, "reset: %d 个文件校验不匹配已跳过（可能被修改或是同名用户文件），请人工确认:\n", len(skipped))
		for _, p := range skipped {
			_, _ = fmt.Fprintf(stdout, "  - %s\n", p)
		}
	}
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
