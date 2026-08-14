package main

// 升级/回滚/台账 测试（WBS 10.2，见 docs/UPGRADE.md）。

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// joinOnce 在输出目录生成 join 产物（helper）。
func joinOnce(t *testing.T, out string) {
	t.Helper()
	code, _, stderr := runCapture([]string{
		"join", "--cloudcore-ip=192.168.1.10", "--token=abc123",
		"--node-id=edge-worker-01", "--output-dir=" + out,
	}, "")
	if code != 0 {
		t.Fatalf("join 失败: %s", stderr)
	}
}

// readLedger 读取台账并解析为结构化记录列表（按文件顺序）。
func readLedger(t *testing.T, out string) []ledgerEntry {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(out, ledgerFile))
	if err != nil {
		t.Fatalf("读取台账失败: %v", err)
	}
	var entries []ledgerEntry
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var e ledgerEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("台账行解析失败 %q: %v", line, err)
		}
		entries = append(entries, e)
	}
	return entries
}

// listBackupDirs 列出 backups/ 下的备份目录名。
func listBackupDirs(t *testing.T, out string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(out, backupsDir))
	if err != nil {
		t.Fatalf("读取备份目录失败: %v", err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	return dirs
}

// readManifest 读取指定备份目录的 manifest.json。
func readManifest(t *testing.T, out, id string) backupManifest {
	t.Helper()
	mb, err := os.ReadFile(filepath.Join(out, backupsDir, id, "manifest.json"))
	if err != nil {
		t.Fatalf("读取备份清单失败: %v", err)
	}
	var m backupManifest
	if err := json.Unmarshal(mb, &m); err != nil {
		t.Fatalf("备份清单解析失败: %v", err)
	}
	return m
}

// TestJoinEnvHasVersionMarker 验证 join 生成 env 含版本标记行（upgrade 的输入前提）。
func TestJoinEnvHasVersionMarker(t *testing.T) {
	out, _ := tmpOut(t)
	joinOnce(t, out)
	envText := readFile(t, filepath.Join(out, "edgecore.env"))
	if !strings.Contains(envText, envVersionLine(edgeEnvVersion)) {
		t.Errorf("join 产物应含版本标记 %q\n完整内容:\n%s", envVersionLine(edgeEnvVersion), envText)
	}
	if v := envVersion(out); v != "v0.1.0" {
		t.Errorf("envVersion 读取 = %q，期望 v0.1.0", v)
	}
}

// TestUpgradeSuccess 验证升级成功路径：备份 + 版本更新 + 台账。
func TestUpgradeSuccess(t *testing.T) {
	out, _ := tmpOut(t)
	joinOnce(t, out)

	code, stdout, stderr := runCapture([]string{"upgrade", "--version=v0.2.0", "--output-dir=" + out}, "")
	if code != 0 {
		t.Fatalf("upgrade 退出码 = %d，期望 0；stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "v0.1.0 -> v0.2.0") {
		t.Errorf("stdout 应含版本迁移: %q", stdout)
	}

	// env 版本标记已更新。
	envText := readFile(t, filepath.Join(out, "edgecore.env"))
	if !strings.Contains(envText, envVersionLine("v0.2.0")) {
		t.Errorf("env 版本标记未更新: %s", envText)
	}

	// 备份：1 个目录，manifest 记录 v0.1.0 + 文件清单 + sha256。
	dirs := listBackupDirs(t, out)
	if len(dirs) != 1 {
		t.Fatalf("应有 1 个备份目录，实际 %v", dirs)
	}
	m := readManifest(t, out, dirs[0])
	if m.Version != "v0.1.0" || m.TS == "" || len(m.Files) != 3 {
		t.Errorf("manifest 字段不完整: %+v", m)
	}
	for _, name := range backupFiles {
		if m.SHA256[name] == "" {
			t.Errorf("manifest 缺少 %s 的 sha256: %+v", name, m.SHA256)
		}
	}
	// 备份内容 = 升级前的 env（v0.1.0 标记）。
	backupEnv := readFile(t, filepath.Join(out, backupsDir, dirs[0], "edgecore.env"))
	if !strings.Contains(backupEnv, envVersionLine("v0.1.0")) {
		t.Errorf("备份 env 应为升级前内容: %s", backupEnv)
	}

	// 台账：1 条 ok 记录，字段完整。
	entries := readLedger(t, out)
	if len(entries) != 1 {
		t.Fatalf("台账应有 1 条记录，实际 %+v", entries)
	}
	e := entries[0]
	if e.Action != "upgrade" || e.Result != "ok" || e.From != "v0.1.0" || e.To != "v0.2.0" {
		t.Errorf("台账记录字段错误: %+v", e)
	}
	if e.Operator != "unknown" || e.TS == "" {
		t.Errorf("台账 operator/ts 字段错误: %+v", e)
	}
}

// TestUpgradeSimulateFailure 验证演练模式：备份完成后模拟失败 + 台账 failed + 恢复提示。
func TestUpgradeSimulateFailure(t *testing.T) {
	out, _ := tmpOut(t)
	joinOnce(t, out)

	code, _, stderr := runCapture([]string{
		"upgrade", "--version=v0.2.0", "--simulate-failure", "--output-dir=" + out,
	}, "")
	if code != 1 {
		t.Fatalf("simulate-failure 退出码 = %d，期望 1；stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "rollback --latest") {
		t.Errorf("stderr 应提示可执行 keadm rollback --latest: %q", stderr)
	}

	// 产物未被修改（备份已生成，env 未更新）。
	envText := readFile(t, filepath.Join(out, "edgecore.env"))
	if !strings.Contains(envText, envVersionLine("v0.1.0")) || strings.Contains(envText, envVersionLine("v0.2.0")) {
		t.Errorf("演练失败后 env 不应变化: %s", envText)
	}
	if dirs := listBackupDirs(t, out); len(dirs) != 1 {
		t.Errorf("演练失败后应有 1 个备份，实际 %v", dirs)
	}

	// 台账：1 条 failed，note 标注 simulate-failure。
	entries := readLedger(t, out)
	if len(entries) != 1 || entries[0].Action != "upgrade" || entries[0].Result != "failed" {
		t.Fatalf("台账应有 1 条 upgrade failed，实际 %+v", entries)
	}
	if !strings.Contains(entries[0].Note, "simulate-failure") {
		t.Errorf("台账 note 应标注 simulate-failure: %+v", entries[0])
	}
}

// TestRollbackLatest 验证回滚成功路径：版本恢复 + 文件与备份一致 + 台账。
func TestRollbackLatest(t *testing.T) {
	out, _ := tmpOut(t)
	joinOnce(t, out)
	// 预置：真实升级到 v0.2.0（env 内容发生变化）。
	if code, _, stderr := runCapture([]string{"upgrade", "--version=v0.2.0", "--output-dir=" + out}, ""); code != 0 {
		t.Fatalf("预置 upgrade 失败: %s", stderr)
	}
	if !strings.Contains(readFile(t, filepath.Join(out, "edgecore.env")), envVersionLine("v0.2.0")) {
		t.Fatalf("预置升级未生效")
	}

	code, stdout, stderr := runCapture([]string{"rollback", "--latest", "--output-dir=" + out}, "")
	if code != 0 {
		t.Fatalf("rollback 退出码 = %d，期望 0；stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "v0.2.0 -> v0.1.0") {
		t.Errorf("rollback stdout 应含版本恢复: %q", stdout)
	}

	// env 版本回到 v0.1.0。
	envText := readFile(t, filepath.Join(out, "edgecore.env"))
	if !strings.Contains(envText, envVersionLine("v0.1.0")) || strings.Contains(envText, envVersionLine("v0.2.0")) {
		t.Errorf("回滚后 env 应回到 v0.1.0: %s", envText)
	}

	// 文件内容与备份逐字节一致（diff 等价校验）。
	dirs := listBackupDirs(t, out)
	if len(dirs) != 1 {
		t.Fatalf("应有 1 个备份目录，实际 %v", dirs)
	}
	for _, name := range backupFiles {
		backupBytes, err := os.ReadFile(filepath.Join(out, backupsDir, dirs[0], name))
		if err != nil {
			t.Fatalf("读备份 %s 失败: %v", name, err)
		}
		curBytes, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatalf("读当前 %s 失败: %v", name, err)
		}
		if !bytes.Equal(backupBytes, curBytes) {
			t.Errorf("%s 回滚后与备份不一致", name)
		}
	}
	// install.sh 权限恢复为可执行。
	info, err := os.Stat(filepath.Join(out, "install.sh"))
	if err != nil {
		t.Fatalf("stat install.sh 失败: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("回滚后 install.sh 应可执行，实际 %v", info.Mode().Perm())
	}

	// 台账：upgrade ok + rollback ok 两条。
	entries := readLedger(t, out)
	if len(entries) != 2 {
		t.Fatalf("台账应有 2 条记录，实际 %+v", entries)
	}
	r := entries[1]
	if r.Action != "rollback" || r.Result != "ok" || r.From != "v0.2.0" || r.To != "v0.1.0" {
		t.Errorf("rollback 台账字段错误: %+v", r)
	}
}

// TestRollbackSpecificBackup 验证指定备份回滚：--backup=<id> 恢复对应版本快照。
func TestRollbackSpecificBackup(t *testing.T) {
	out, _ := tmpOut(t)
	joinOnce(t, out)
	if code, _, stderr := runCapture([]string{"upgrade", "--version=v0.2.0", "--output-dir=" + out}, ""); code != 0 {
		t.Fatalf("upgrade v0.2.0 失败: %s", stderr)
	}
	if code, _, stderr := runCapture([]string{"upgrade", "--version=v0.3.0", "--output-dir=" + out}, ""); code != 0 {
		t.Fatalf("upgrade v0.3.0 失败: %s", stderr)
	}
	dirs := listBackupDirs(t, out)
	if len(dirs) != 2 {
		t.Fatalf("应有 2 个备份，实际 %v", dirs)
	}
	// os.ReadDir 按文件名升序：dirs[0] 是较早备份（v0.1.0 快照），
	// dirs[1] 是较新备份（v0.2.0 快照，id 带 -2 序号后缀）。
	older := dirs[0]
	if m := readManifest(t, out, older); m.Version != "v0.1.0" {
		t.Fatalf("较早备份应为 v0.1.0 快照，实际 %+v", m)
	}

	// 回滚到最早备份：env 应回到 v0.1.0。
	if code, _, stderr := runCapture([]string{"rollback", "--backup=" + older, "--output-dir=" + out}, ""); code != 0 {
		t.Fatalf("rollback --backup 失败: %s", stderr)
	}
	envText := readFile(t, filepath.Join(out, "edgecore.env"))
	if !strings.Contains(envText, envVersionLine("v0.1.0")) || strings.Contains(envText, envVersionLine("v0.2.0")) {
		t.Errorf("回滚到 v0.1.0 快照后 env 错误: %s", envText)
	}

	// 回滚到 v0.2.0 快照：env 应为 v0.2.0。
	newer := dirs[1]
	if m := readManifest(t, out, newer); m.Version != "v0.2.0" {
		t.Fatalf("较新备份应为 v0.2.0 快照，实际 %+v", m)
	}
	if code, _, stderr := runCapture([]string{"rollback", "--backup=" + newer, "--output-dir=" + out}, ""); code != 0 {
		t.Fatalf("第二次 rollback 失败: %s", stderr)
	}
	envText = readFile(t, filepath.Join(out, "edgecore.env"))
	if !strings.Contains(envText, envVersionLine("v0.2.0")) || strings.Contains(envText, envVersionLine("v0.3.0")) {
		t.Errorf("回滚到 v0.2.0 快照后 env 错误: %s", envText)
	}

	// 台账追加不覆盖：共 4 条（2 upgrade ok + 2 rollback ok）。
	entries := readLedger(t, out)
	if len(entries) != 4 {
		t.Fatalf("台账应有 4 条记录，实际 %d: %+v", len(entries), entries)
	}
}

// TestRollbackNoBackup 验证无备份时 rollback 报错 + 台账 failed。
func TestRollbackNoBackup(t *testing.T) {
	out, _ := tmpOut(t)
	joinOnce(t, out)

	code, _, stderr := runCapture([]string{"rollback", "--latest", "--output-dir=" + out}, "")
	if code != 1 {
		t.Fatalf("无备份 rollback 退出码 = %d，期望 1；stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "没有可用备份") {
		t.Errorf("应提示无可用备份: %q", stderr)
	}

	entries := readLedger(t, out)
	if len(entries) != 1 || entries[0].Action != "rollback" || entries[0].Result != "failed" {
		t.Fatalf("台账应有 1 条 rollback failed，实际 %+v", entries)
	}
}

// TestRollbackFlags 验证 rollback 参数异常：互斥/缺失/未知备份 id。
func TestRollbackFlags(t *testing.T) {
	out, _ := tmpOut(t)
	joinOnce(t, out)

	// --backup 与 --latest 互斥。
	code, _, stderr := runCapture([]string{"rollback", "--backup=x", "--latest", "--output-dir=" + out}, "")
	if code != 2 || !strings.Contains(stderr, "互斥") {
		t.Errorf("互斥参数应 exit=2 并提示互斥: code=%d stderr=%q", code, stderr)
	}
	// 两者都未指定。
	code, _, stderr = runCapture([]string{"rollback", "--output-dir=" + out}, "")
	if code != 2 || !strings.Contains(stderr, "必须指定") {
		t.Errorf("缺参数应 exit=2 并提示: code=%d stderr=%q", code, stderr)
	}
	// 有备份但指定不存在的备份 id（exit 1，运行时错误）。
	if code, _, stderr := runCapture([]string{"upgrade", "--version=v0.2.0", "--output-dir=" + out}, ""); code != 0 {
		t.Fatalf("预置 upgrade 失败: %s", stderr)
	}
	code, _, stderr = runCapture([]string{"rollback", "--backup=20200101-000000", "--output-dir=" + out}, "")
	if code != 1 || !strings.Contains(stderr, "不存在") {
		t.Errorf("未知备份应 exit=1 并提示不存在: code=%d stderr=%q", code, stderr)
	}
}

// TestUpgradeInvalidVersion 验证版本格式校验：非 vX.Y.Z 一律拒绝。
func TestUpgradeInvalidVersion(t *testing.T) {
	out, _ := tmpOut(t)
	joinOnce(t, out)
	for _, v := range []string{"", "0.2.0", "v0.2", "v0.2.0-rc1", "v0.2.0.1", "vabc", "v1.2", "V0.2.0"} {
		code, _, stderr := runCapture([]string{"upgrade", "--version=" + v, "--output-dir=" + out}, "")
		if code != 2 {
			t.Errorf("--version=%q 退出码 = %d，期望 2；stderr=%s", v, code, stderr)
		}
		if !strings.Contains(stderr, "格式非法") {
			t.Errorf("--version=%q 应提示格式非法: %q", v, stderr)
		}
	}
}

// TestUpgradeSameVersion 验证目标版本与当前版本相同时拒绝。
func TestUpgradeSameVersion(t *testing.T) {
	out, _ := tmpOut(t)
	joinOnce(t, out)
	code, _, stderr := runCapture([]string{"upgrade", "--version=v0.1.0", "--output-dir=" + out}, "")
	if code != 2 || !strings.Contains(stderr, "相同") {
		t.Errorf("同版本升级应 exit=2 并提示: code=%d stderr=%q", code, stderr)
	}
}

// TestUpgradeMissingArtifacts 验证缺少产物时 upgrade 报错 + 台账 failed。
func TestUpgradeMissingArtifacts(t *testing.T) {
	out, _ := tmpOut(t) // 目录不存在
	code, _, stderr := runCapture([]string{"upgrade", "--version=v0.2.0", "--output-dir=" + out}, "")
	if code != 1 || !strings.Contains(stderr, "缺少产物") {
		t.Errorf("缺产物 upgrade 应 exit=1 并提示缺少产物: code=%d stderr=%q", code, stderr)
	}
	// 失败也落台账（目录自动创建）。
	entries := readLedger(t, out)
	if len(entries) != 1 || entries[0].Result != "failed" {
		t.Fatalf("台账应有 1 条 failed，实际 %+v", entries)
	}
}

// TestOpsLedger 验证台账查询：空台账、limit 截断、逐行 JSON 完整性。
func TestOpsLedger(t *testing.T) {
	out, _ := tmpOut(t)

	// 空台账：输出空数组，退出 0。
	code, stdout, _ := runCapture([]string{"ops-ledger", "--output-dir=" + out}, "")
	if code != 0 || !strings.Contains(stdout, "[]") {
		t.Errorf("空台账应输出 [] 且 exit=0: code=%d stdout=%q", code, stdout)
	}

	joinOnce(t, out)
	// 制造 3 条记录：upgrade failed（演练）、rollback ok、upgrade ok。
	if code, _, _ := runCapture([]string{"upgrade", "--version=v0.2.0", "--simulate-failure", "--output-dir=" + out}, ""); code != 1 {
		t.Fatalf("预置 simulate-failure 失败")
	}
	if code, _, _ := runCapture([]string{"rollback", "--latest", "--output-dir=" + out}, ""); code != 0 {
		t.Fatalf("预置 rollback 失败")
	}
	if code, _, _ := runCapture([]string{"upgrade", "--version=v0.2.0", "--output-dir=" + out}, ""); code != 0 {
		t.Fatalf("预置 upgrade 失败")
	}

	// 默认输出全部（3 ≤ 20）。
	code, stdout, stderr := runCapture([]string{"ops-ledger", "--output-dir=" + out}, "")
	if code != 0 {
		t.Fatalf("ops-ledger 退出码 = %d，期望 0；stderr=%s", code, stderr)
	}
	var all []ledgerEntry
	if err := json.Unmarshal([]byte(stdout), &all); err != nil {
		t.Fatalf("ops-ledger 输出不是合法 JSON 数组: %v\n%s", err, stdout)
	}
	if len(all) != 3 {
		t.Fatalf("应有 3 条记录，实际 %d: %+v", len(all), all)
	}
	if all[0].Action != "upgrade" || all[0].Result != "failed" {
		t.Errorf("第 1 条应为 upgrade failed: %+v", all[0])
	}
	if all[1].Action != "rollback" || all[1].Result != "ok" {
		t.Errorf("第 2 条应为 rollback ok: %+v", all[1])
	}

	// limit=2 只输出最近 2 条。
	code, stdout, _ = runCapture([]string{"ops-ledger", "--limit=2", "--output-dir=" + out}, "")
	if code != 0 {
		t.Fatalf("ops-ledger --limit=2 失败")
	}
	var recent []ledgerEntry
	if err := json.Unmarshal([]byte(stdout), &recent); err != nil {
		t.Fatalf("limit 输出解析失败: %v", err)
	}
	if len(recent) != 2 {
		t.Fatalf("limit=2 应输出 2 条，实际 %d", len(recent))
	}
	if recent[0].Action != "rollback" || recent[1].Action != "upgrade" {
		t.Errorf("最近 2 条应为 rollback + upgrade: %+v", recent)
	}
}
