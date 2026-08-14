// keadm rollback：从 backups/ 恢复产物文件（WBS 10.2，见 docs/UPGRADE.md）。
//
//   - --backup=<id> 指定备份；--latest 取最新有效备份（按 id 时间戳倒序）；
//   - 恢复前校验备份完整性（manifest 含版本/时间/文件清单 + sha256 逐文件比对）；
//   - 恢复失败兜底：保留备份目录不删，并输出人工介入的 cp 命令示例；
//   - 每次操作（成功/失败）都追加台账记录。
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// rollbackOptions 是 keadm rollback 的参数集合。
type rollbackOptions struct {
	// BackupID 是指定回滚的备份 id（--backup=<id>，backups/ 下的目录名）。
	BackupID string
	// Latest 是否取最新有效备份（--latest，与 --backup 互斥）。
	Latest bool
	// OutputDir 是产物输出目录。
	OutputDir string
}

// listBackups 列出 backups/ 下的有效备份（按 id 倒序，最新在前）。
// 备份目录不存在时返回空列表（尚无备份，不是错误）；无效备份
// （缺 manifest.json 或清单解析失败）跳过并返回警告，不删除现场。
func listBackups(outputDir string) ([]backupManifest, []string, error) {
	root := filepath.Join(outputDir, backupsDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	var backups []backupManifest
	var warnings []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mb, err := os.ReadFile(filepath.Join(root, e.Name(), "manifest.json"))
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("跳过 %s: 缺少 manifest.json（%v）", e.Name(), err))
			continue
		}
		var m backupManifest
		if err := json.Unmarshal(mb, &m); err != nil {
			warnings = append(warnings, fmt.Sprintf("跳过 %s: manifest.json 解析失败（%v）", e.Name(), err))
			continue
		}
		if name := invalidBackupFile(m.Files); name != "" {
			// 清单含白名单外文件（P2-5）：视为无效备份跳过，保留现场不删。
			warnings = append(warnings, fmt.Sprintf("跳过 %s: 备份清单含未知文件 %q（仅允许 %s）",
				e.Name(), name, strings.Join(backupFiles, "/")))
			continue
		}
		backups = append(backups, m)
	}
	// id 为同一格式时间戳，字典序倒排即时间倒序（最新在前）。
	sort.Slice(backups, func(i, j int) bool { return backups[i].ID > backups[j].ID })
	return backups, warnings, nil
}

// backupWhitelist 是备份/恢复允许的文件名白名单（P2-5 纵深防御）：
// 备份清单 Files 中的文件名只能是 keadm 的三个产物（与 backupFiles
// 单一来源，自动保持一致）；manifest.json 是清单文件本身，不在 Files 内。
// 白名单外的条目（如绝对路径 /etc/cron.d/x、路径穿越 ../escape）在
// 加载时拒绝、恢复时跳过，防止被篡改的清单写出白名单之外的文件。
var backupWhitelist = func() map[string]bool {
	m := make(map[string]bool, len(backupFiles))
	for _, n := range backupFiles {
		m[n] = true
	}
	return m
}()

// invalidBackupFile 返回备份文件清单中第一个不在白名单内的文件名；
// 全部合法时返回空串。
func invalidBackupFile(files []string) string {
	for _, name := range files {
		if !backupWhitelist[name] {
			return name
		}
	}
	return ""
}

// findBackup 定位指定备份目录并校验完整性：manifest 可解析、版本/文件清单
// 字段完整、清单文件名全部在白名单内（P2-5）、清单中每个文件存在且
// sha256 与 manifest 一致。返回备份清单与备份目录绝对路径。
func findBackup(outputDir, id string) (backupManifest, string, error) {
	dir := filepath.Join(outputDir, backupsDir, id)
	mb, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return backupManifest{}, "", fmt.Errorf("备份 %s 不存在或缺少 manifest.json（%v）", id, err)
	}
	var m backupManifest
	if err := json.Unmarshal(mb, &m); err != nil {
		return backupManifest{}, "", fmt.Errorf("备份 %s 的 manifest.json 损坏: %v", id, err)
	}
	if m.Version == "" || len(m.Files) == 0 {
		return backupManifest{}, "", fmt.Errorf("备份 %s 清单字段不完整（version/files 为空）", id)
	}
	if name := invalidBackupFile(m.Files); name != "" {
		return backupManifest{}, "", fmt.Errorf("备份 %s 清单含未知文件 %q（仅允许 %s），拒绝加载",
			id, name, strings.Join(backupFiles, "/"))
	}
	for _, name := range m.Files {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return backupManifest{}, "", fmt.Errorf("备份 %s 缺少文件 %s: %v", id, name, err)
		}
		if want, ok := m.SHA256[name]; ok {
			sum := sha256.Sum256(b)
			if hex.EncodeToString(sum[:]) != want {
				return backupManifest{}, "", fmt.Errorf("备份 %s 文件 %s 校验和不一致（备份可能损坏）", id, name)
			}
		}
	}
	return m, dir, nil
}

// restoreBackup 把备份目录中的产物文件恢复到 output-dir（事务化，P2-4）：
//
//  1. 阶段一（暂存）：全部文件先复制到 staging 目录
//     （output-dir/.restore-staging-<pid>/，与 output-dir 同目录同设备），
//     在 staging 内强制恢复 join 约定的权限（env 0600 / service 0644 /
//     install.sh 0755）并回读校验逐字节一致——此阶段不触碰目标文件；
//  2. 阶段二（提交）：全部暂存成功后才逐个 os.Rename 原子替换目标文件
//     （同设备 Rename，不会跨设备；每个文件要么完整替换、要么不动）；
//  3. 任一文件失败：清理 staging、备份目录保留不动、目标文件保持原状
//     （不会被部分覆盖），返回错误由调用方输出人工介入提示。
//
// 只恢复白名单内的产物文件（P2-5 纵深防御：即使清单被篡改，也不会写出
// 白名单之外的文件）。
func restoreBackup(outputDir, dir string, m backupManifest) error {
	staging := filepath.Join(outputDir, fmt.Sprintf(".restore-staging-%d", os.Getpid()))
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return fmt.Errorf("创建恢复暂存目录 %s 失败: %w", staging, err)
	}
	// 成功/失败路径都清理 staging（defer 兜底，防止异常路径残留）。
	defer func() { _ = os.RemoveAll(staging) }()

	// 阶段一：全部复制到 staging + 强制权限 + 回读校验（不触碰目标文件）。
	var files []string
	for _, name := range m.Files {
		if !backupWhitelist[name] {
			continue // 只恢复白名单产物（P2-5）
		}
		src := filepath.Join(dir, name)
		stg := filepath.Join(staging, name)
		b, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("读取备份文件 %s 失败: %w", src, err)
		}
		perm := os.FileMode(0o644)
		switch name {
		case "edgecore.env":
			perm = 0o600
		case "install.sh":
			perm = 0o755
		}
		if err := os.WriteFile(stg, b, perm); err != nil {
			return fmt.Errorf("暂存文件 %s 失败: %w", stg, err)
		}
		if err := os.Chmod(stg, perm); err != nil {
			return fmt.Errorf("恢复文件权限 %s 失败: %w", stg, err)
		}
		// 回读校验：暂存结果必须与备份内容逐字节一致。
		got, err := os.ReadFile(stg)
		if err != nil {
			return fmt.Errorf("回读校验 %s 失败: %w", stg, err)
		}
		if !bytes.Equal(got, b) {
			return fmt.Errorf("回读校验 %s 不一致（暂存结果与备份不同）", stg)
		}
		files = append(files, name)
	}

	// 阶段二：全部就绪后原子替换（同设备 Rename，逐个覆盖目标）。
	for _, name := range files {
		dst := filepath.Join(outputDir, name)
		if err := os.Rename(filepath.Join(staging, name), dst); err != nil {
			return fmt.Errorf("替换文件 %s 失败: %w", dst, err)
		}
	}
	return nil
}

// rollbackHint 返回人工介入路径提示（回滚失败/无法自动恢复时输出，
// 给出逐文件的 cp 命令示例；备份目录不会被删除）。
func rollbackHint(outputDir, backupID string) string {
	var b strings.Builder
	b.WriteString("人工介入路径（备份目录未被删除，可手动恢复）:\n")
	for _, name := range backupFiles {
		fmt.Fprintf(&b, "  cp %s %s\n",
			filepath.Join(outputDir, backupsDir, backupID, name),
			filepath.Join(outputDir, name))
	}
	return b.String()
}

// runRollback 实现 keadm rollback：定位备份 → 校验完整性 → 恢复产物 → 台账记录。
func runRollback(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("rollback", stderr)
	opts := rollbackOptions{OutputDir: "./keadm-out"}
	fs.StringVar(&opts.BackupID, "backup", "", "指定备份 id（backups/ 下的目录名，如 20260814-190701）")
	fs.BoolVar(&opts.Latest, "latest", false, "取最新有效备份回滚（与 --backup 互斥）")
	fs.StringVar(&opts.OutputDir, "output-dir", opts.OutputDir, "产物输出目录")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "错误: rollback 不接受位置参数 %q\n", fs.Arg(0))
		return exitUsage
	}
	if opts.Latest && opts.BackupID != "" {
		_, _ = fmt.Fprintln(stderr, "错误: --backup 与 --latest 互斥，只能二选一")
		return exitUsage
	}
	if !opts.Latest && opts.BackupID == "" {
		_, _ = fmt.Fprintln(stderr, "错误: 必须指定 --backup=<id> 或 --latest（可用 keadm ops-ledger 查询备份 id）")
		return exitUsage
	}

	current := envVersion(opts.OutputDir)
	// fail 统一兜底：台账记 failed + 返回运行时错误码。
	fail := func(step, note string) int {
		_ = appendLedger(opts.OutputDir, ledgerEntry{
			TS: time.Now().Format(time.RFC3339), Action: "rollback",
			From: current, To: "", Result: "failed", Note: step + ": " + note,
		})
		return exitRuntime
	}

	// 读取备份列表（含无效备份警告）。
	backups, warnings, err := listBackups(opts.OutputDir)
	if err != nil {
		msg := fmt.Sprintf("读取备份目录失败: %v", err)
		_, _ = fmt.Fprintln(stderr, "错误: "+msg)
		return fail("读取备份", msg)
	}
	for _, w := range warnings {
		_, _ = fmt.Fprintf(stderr, "提示: %s\n", w)
	}
	if len(backups) == 0 {
		msg := "没有可用备份（backups/ 为空或全部损坏），无法回滚"
		_, _ = fmt.Fprintln(stderr, "错误: "+msg)
		return fail("无可用备份", msg)
	}

	// 选择目标备份：--latest 取列表第一个（最新），否则按 id 精确匹配。
	var m backupManifest
	if opts.Latest {
		m = backups[0]
	} else {
		found := false
		for _, b := range backups {
			if b.ID == opts.BackupID {
				m = b
				found = true
				break
			}
		}
		if !found {
			msg := fmt.Sprintf("备份 %s 不存在（可用 keadm ops-ledger 查询备份 id）", opts.BackupID)
			_, _ = fmt.Fprintln(stderr, "错误: "+msg)
			return fail("备份不存在", msg)
		}
	}

	// 校验备份完整性（manifest + 文件清单 + sha256）。
	_, dir, err := findBackup(opts.OutputDir, m.ID)
	if err != nil {
		msg := fmt.Sprintf("备份 %s 完整性校验失败: %v", m.ID, err)
		_, _ = fmt.Fprintln(stderr, "错误: "+msg)
		return fail("完整性校验", msg)
	}

	// 执行恢复（逐个复制 + 回读校验）。
	if err := restoreBackup(opts.OutputDir, dir, m); err != nil {
		msg := fmt.Sprintf("回滚恢复失败: %v\n%s", err, rollbackHint(opts.OutputDir, m.ID))
		_, _ = fmt.Fprintln(stderr, "错误: "+msg)
		return fail("恢复失败", err.Error())
	}

	// 刷新 reset 校验清单（P2-② 协调点）：恢复后的产物哈希与清单一致，
	//     保证回滚后 reset 仍可按校验删除。
	if err := refreshManifest(opts.OutputDir, backupFiles); err != nil {
		_, _ = fmt.Fprintf(stderr, "错误: 刷新校验清单失败: %v（回滚本身已完成）\n", err)
		return exitRuntime
	}

	// 台账记录成功。
	if err := appendLedger(opts.OutputDir, ledgerEntry{
		TS: time.Now().Format(time.RFC3339), Action: "rollback",
		From: current, To: m.Version, Result: "ok",
		Note: "备份=" + m.ID,
	}); err != nil {
		_, _ = fmt.Fprintf(stderr, "错误: 写入台账失败: %v（回滚本身已完成）\n", err)
		return exitRuntime
	}

	_, _ = fmt.Fprintf(stdout, "keadm rollback 完成: %s -> %s（恢复自备份 %s）\n", current, m.Version, m.ID)
	return exitOK
}
