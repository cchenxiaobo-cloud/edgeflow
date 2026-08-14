// 升级/回滚共享机制（WBS 10.2，见 docs/UPGRADE.md）：
//
//   - 备份模型：output-dir/backups/<id>/ 保存 edgecore.env / edgecore.service /
//     install.sh 三个产物文件与 manifest.json。id 为时间戳目录名（如
//     20260814-190701），manifest 含版本/时间/文件清单/sha256，
//     rollback 依据 manifest 校验备份完整性；
//   - 台账模型：output-dir/ops-ledger.jsonl，逐行 JSON 追加写（不覆盖历史），
//     字段 {ts, action, from, to, result, operator, note}；
//   - 版本标记：edgecore.env 中以注释行 "# keadm 产物版本: vX.Y.Z" 标记产物版本
//     （join 写入，upgrade 更新，rollback 恢复；兼容旧 join 无标记的产物）。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	// edgeEnvVersion 是 keadm join 生成产物的版本标记值
	// （与 init 默认镜像 tag edgeflow/cloudcore:v0.1.0 对齐）。
	edgeEnvVersion = "v0.1.0"
	// envVersionPrefix / envVersionSuffix 组成 edgecore.env 的版本标记行：
	// "# keadm 产物版本: v0.1.0（...）"。upgrade 替换该行，rollback 依据备份恢复。
	envVersionPrefix = "# keadm 产物版本: "
	envVersionSuffix = "（keadm join 写入，keadm upgrade 更新，keadm rollback 恢复）"
	// ledgerFile 是操作台账文件名（位于 output-dir 下，追加写）。
	ledgerFile = "ops-ledger.jsonl"
	// backupsDir 是备份根目录名（位于 output-dir 下）。
	backupsDir = "backups"
)

// backupFiles 是 upgrade 备份 / rollback 恢复的产物文件清单
// （与任务边界一致：env/service/install.sh，不碰 README.md 与其他文件）。
var backupFiles = []string{"edgecore.env", "edgecore.service", "install.sh"}

// envVersionLine 渲染完整的版本标记行（join 模板与 upgrade 共用，保证单一来源）。
func envVersionLine(version string) string {
	return envVersionPrefix + version + envVersionSuffix
}

// ledgerEntry 是操作台账的一行（JSON，逐行追加）。
type ledgerEntry struct {
	TS       string `json:"ts"`       // 操作时间（RFC3339 本地时区）
	Action   string `json:"action"`   // upgrade / rollback
	From     string `json:"from"`     // 操作前版本（读取不到时为 unknown）
	To       string `json:"to"`       // 操作目标版本（失败且未知时为空串）
	Result   string `json:"result"`   // ok / failed
	Operator string `json:"operator"` // 操作人（env KEADM_OPERATOR，默认 unknown）
	Note     string `json:"note"`     // 备注（备份 id、失败原因等）
}

// appendLedger 向 output-dir/ops-ledger.jsonl 追加一条记录（目录不存在时自动创建）。
// 追加写不覆盖历史；operator 未显式指定时取 env KEADM_OPERATOR。
func appendLedger(outputDir string, e ledgerEntry) error {
	if e.Operator == "" {
		e.Operator = operatorName()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("序列化台账记录失败: %w", err)
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("创建台账目录失败: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(outputDir, ledgerFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("打开台账文件失败: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("追加台账记录失败: %w", err)
	}
	return nil
}

// operatorName 读取操作人（env KEADM_OPERATOR，缺省 unknown）。
func operatorName() string {
	if v := os.Getenv("KEADM_OPERATOR"); v != "" {
		return v
	}
	return "unknown"
}

// versionRe 是版本格式约束：vX.Y.Z（X/Y/Z 为非负整数，如 v0.2.0）。
var versionRe = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// validVersion 校验版本格式（vX.Y.Z）。
func validVersion(v string) bool { return versionRe.MatchString(v) }

// envVersion 读取 edgecore.env 中的版本标记；文件缺失或无标记时返回 unknown
// （兼容旧版 join 生成的产物）。标记行格式为
// "# keadm 产物版本: vX.Y.Z（说明后缀）"，只取版本号本身。
func envVersion(outputDir string) string {
	b, err := os.ReadFile(filepath.Join(outputDir, "edgecore.env"))
	if err != nil {
		return "unknown"
	}
	for _, line := range strings.Split(string(b), "\n") {
		v, ok := strings.CutPrefix(line, envVersionPrefix)
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		// 行尾可能带说明后缀（envVersionSuffix），只取版本号本身。
		if i := strings.Index(v, "（"); i >= 0 {
			v = v[:i]
		}
		if v != "" {
			return v
		}
	}
	return "unknown"
}

// setEnvVersion 更新 edgecore.env 的版本标记：已有标记行则整行替换，
// 否则在文件末尾追加（兼容旧产物）；权限保持 join 生成的 0600。
func setEnvVersion(outputDir, version string) error {
	path := filepath.Join(outputDir, "edgecore.env")
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(b), "\n")
	replaced := false
	for i, line := range lines {
		if strings.HasPrefix(line, envVersionPrefix) {
			lines[i] = envVersionLine(version)
			replaced = true
			break
		}
	}
	content := strings.Join(lines, "\n")
	if !replaced {
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		content += envVersionLine(version) + "\n"
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

// backupManifest 是备份目录的清单文件（rollback 校验备份完整性的依据）。
type backupManifest struct {
	ID       string            `json:"id"`       // 备份 id（= 备份目录名，时间戳）
	TS       string            `json:"ts"`       // 备份时间（RFC3339）
	Version  string            `json:"version"`  // 备份时的产物版本（env 版本标记）
	Operator string            `json:"operator"` // 备份操作人
	Files    []string          `json:"files"`    // 备份文件清单
	SHA256   map[string]string `json:"sha256"`   // 各文件 sha256（完整性校验）
}

// createBackup 把产物文件备份到 backups/<id>/ 并写入 manifest.json。
// 返回备份 id；id 取到秒精度的时间戳，同一秒内多次备份时追加序号避免覆盖。
// manifest 最后写入，中途失败留下的无 manifest 目录会被 rollback
// 视为无效备份跳过（不删除，保留现场）。
func createBackup(outputDir, version string) (string, error) {
	base := time.Now().Format("20060102-150405")
	id := base
	for i := 2; ; i++ {
		if _, err := os.Stat(filepath.Join(outputDir, backupsDir, id)); os.IsNotExist(err) {
			break
		}
		id = fmt.Sprintf("%s-%d", base, i)
	}
	dir := filepath.Join(outputDir, backupsDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建备份目录 %s 失败: %w", dir, err)
	}
	m := backupManifest{
		ID:       id,
		TS:       time.Now().Format(time.RFC3339),
		Version:  version,
		Operator: operatorName(),
		Files:    append([]string{}, backupFiles...),
		SHA256:   map[string]string{},
	}
	for _, name := range backupFiles {
		src := filepath.Join(outputDir, name)
		b, err := os.ReadFile(src)
		if err != nil {
			return "", fmt.Errorf("读取待备份文件 %s 失败: %w", src, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			return "", fmt.Errorf("写入备份文件 %s 失败: %w", name, err)
		}
		sum := sha256.Sum256(b)
		m.SHA256[name] = hex.EncodeToString(sum[:])
	}
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", fmt.Errorf("序列化备份清单失败: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), mb, 0o644); err != nil {
		return "", fmt.Errorf("写入备份清单失败: %w", err)
	}
	return id, nil
}

// upgradeOptions 是 keadm upgrade 的参数集合。
type upgradeOptions struct {
	// Version 是目标版本（vX.Y.Z）。
	Version string
	// OutputDir 是产物输出目录。
	OutputDir string
	// SimulateFailure 是演练模式：备份完成后模拟升级失败（用于演练回滚）。
	SimulateFailure bool
}

// runUpgrade 实现 keadm upgrade：
//
//	① 校验版本格式（vX.Y.Z）→ ② 备份当前产物 → ③ 更新 env 版本标记 → ④ 台账记录。
//
// 任一环节失败：台账记录 failed，并提示可用 keadm rollback --latest 恢复。
func runUpgrade(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("upgrade", stderr)
	opts := upgradeOptions{OutputDir: "./keadm-out"}
	fs.StringVar(&opts.Version, "version", "", "目标版本（格式 vX.Y.Z，如 v0.2.0）")
	fs.StringVar(&opts.OutputDir, "output-dir", opts.OutputDir, "产物输出目录")
	fs.BoolVar(&opts.SimulateFailure, "simulate-failure", false, "演练模式：备份完成后模拟升级失败（用于演练回滚）")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "错误: upgrade 不接受位置参数 %q\n", fs.Arg(0))
		return exitUsage
	}

	// ① 版本格式校验（vX.Y.Z）。
	if !validVersion(opts.Version) {
		msg := fmt.Sprintf("--version=%q 格式非法，需 vX.Y.Z（如 v0.2.0）", opts.Version)
		_, _ = fmt.Fprintln(stderr, "错误: "+msg)
		_ = appendLedger(opts.OutputDir, ledgerEntry{
			TS: time.Now().Format(time.RFC3339), Action: "upgrade",
			From: envVersion(opts.OutputDir), To: opts.Version, Result: "failed",
			Note: "版本格式校验失败: " + msg,
		})
		return exitUsage
	}

	current := envVersion(opts.OutputDir)
	if current == opts.Version {
		msg := fmt.Sprintf("目标版本 %s 与当前版本相同，无需升级", opts.Version)
		_, _ = fmt.Fprintln(stderr, "错误: "+msg)
		_ = appendLedger(opts.OutputDir, ledgerEntry{
			TS: time.Now().Format(time.RFC3339), Action: "upgrade",
			From: current, To: opts.Version, Result: "failed",
			Note: "目标版本与当前版本相同",
		})
		return exitUsage
	}

	// 产物存在性预检（缺文件时备份环节也会失败，这里先给出明确错误）。
	for _, name := range backupFiles {
		if _, err := os.Stat(filepath.Join(opts.OutputDir, name)); err != nil {
			msg := fmt.Sprintf("缺少产物 %s，请先执行 keadm join 生成产物（执行 keadm rollback --latest 可恢复历史备份）",
				filepath.Join(opts.OutputDir, name))
			_, _ = fmt.Fprintln(stderr, "错误: "+msg)
			_ = appendLedger(opts.OutputDir, ledgerEntry{
				TS: time.Now().Format(time.RFC3339), Action: "upgrade",
				From: current, To: opts.Version, Result: "failed", Note: "产物缺失: " + msg,
			})
			return exitRuntime
		}
	}

	// ② 备份当前产物（backups/<时间戳>/，manifest 含版本/时间/文件清单/sha256）。
	backupID, err := createBackup(opts.OutputDir, current)
	if err != nil {
		msg := fmt.Sprintf("备份失败: %v（执行 keadm rollback --latest 可恢复历史备份）", err)
		_, _ = fmt.Fprintln(stderr, "错误: "+msg)
		_ = appendLedger(opts.OutputDir, ledgerEntry{
			TS: time.Now().Format(time.RFC3339), Action: "upgrade",
			From: current, To: opts.Version, Result: "failed", Note: msg,
		})
		return exitRuntime
	}
	backupNote := "备份=" + backupID

	// 演练模式：在第 ② 步后模拟失败（env 尚未更新，产物未被修改）。
	if opts.SimulateFailure {
		msg := fmt.Sprintf("[演练] 在备份完成后模拟升级失败（产物未修改；执行 keadm rollback --latest 可恢复，备份=%s）", backupID)
		_, _ = fmt.Fprintln(stderr, "错误: "+msg)
		_ = appendLedger(opts.OutputDir, ledgerEntry{
			TS: time.Now().Format(time.RFC3339), Action: "upgrade",
			From: current, To: opts.Version, Result: "failed", Note: "simulate-failure; " + backupNote,
		})
		return exitRuntime
	}

	// ③ 更新 env 版本标记。
	if err := setEnvVersion(opts.OutputDir, opts.Version); err != nil {
		msg := fmt.Sprintf("更新版本标记失败: %v（执行 keadm rollback --latest 可恢复）", err)
		_, _ = fmt.Fprintln(stderr, "错误: "+msg)
		_ = appendLedger(opts.OutputDir, ledgerEntry{
			TS: time.Now().Format(time.RFC3339), Action: "upgrade",
			From: current, To: opts.Version, Result: "failed", Note: backupNote + "; " + msg,
		})
		return exitRuntime
	}

	// ③b 刷新 reset 校验清单（P2-② 协调点）：env 版本标记已更新，
	//     其 sha256 与 keadm-manifest.json 记录不一致会让 reset 跳过并提示
	//     人工确认——这里重新登记当前产物哈希，保持升级后 reset 可用。
	if err := refreshManifest(opts.OutputDir, backupFiles); err != nil {
		_, _ = fmt.Fprintf(stderr, "错误: 刷新校验清单失败: %v（升级已完成；执行 keadm rollback --latest 可回滚）\n", err)
		// M4D P2-1：退出码非 0 的路径台账必须记 failed（台账与退出码一致性）
		_ = appendLedger(opts.OutputDir, ledgerEntry{
			TS: time.Now().Format(time.RFC3339), Action: "upgrade",
			From: current, To: opts.Version, Result: "failed", Note: backupNote + "; 校验清单刷新失败",
		})
		return exitRuntime
	}

	// ④ 台账记录成功。
	if err := appendLedger(opts.OutputDir, ledgerEntry{
		TS: time.Now().Format(time.RFC3339), Action: "upgrade",
		From: current, To: opts.Version, Result: "ok", Note: backupNote,
	}); err != nil {
		_, _ = fmt.Fprintf(stderr, "错误: 写入台账失败: %v（升级本身已完成；执行 keadm rollback --latest 可回滚）\n", err)
		return exitRuntime
	}

	_, _ = fmt.Fprintf(stdout, "keadm upgrade 完成: %s -> %s（备份: %s/%s；执行 keadm rollback --latest 可回滚）\n",
		current, opts.Version, backupsDir, backupID)
	return exitOK
}
