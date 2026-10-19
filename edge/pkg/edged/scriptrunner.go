// scriptrunner.go —— v0.47.0（spec 0020 US-4/G07）边缘脚本化应用执行器。
//
// opt-in：EDGEFLOW_EDGED_SCRIPTRUNNER=on 时启用，默认 off 零行为（仅 Warn 留痕）。
// 脚本应用经 ConfigSync 下发（ConfigMap data: scriptTar(base64) + sha256 +
// scriptRef + 生命周期声明），本执行器：
//   - sha256 硬校验（云端登记 ↔ 边缘落地一致，不一致拒绝执行并记 failed）；
//   - 包落缓存（EDGEFLOW_EDGED_APPPKG_DIR/<app>/<version>.tgz）+ 解包到专用
//     工作目录（<dir>/work/<app>-<version>/）；
//   - 生命周期动作（install/start/stop/uninstall）经受控白名单执行：
//     sh -e <script>，超时 60s，输出截断 2KB 留痕，专用工作目录 cwd；
//   - 状态回报：PodStatus 形态复用（pod.name=edgeflow-script-<app>，
//     phase 映射脚本生命周期：Running/Failed/Succeeded）——契约零新增消息；
//   - 审计：每次动作（动作/退出码/耗时/输出摘要）进结构化日志。
//
// 安全硬规则：非 opt-in 时脚本配置仅落盘不执行；执行面只碰包内四个生命周期
// 脚本文件名（非通用 shell 托管，spec §3 边界 ③）。
package edged

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"edgeflow/pkg/log"
)

// ScriptRunner 环境开关（opt-in，默认关）。
const EnvScriptRunner = "EDGEFLOW_EDGED_SCRIPTRUNNER"

// ScriptRunner 包缓存/工作目录环境变量。
const EnvAppPkgDir = "EDGEFLOW_EDGED_APPPKG_DIR"

// scriptExecTimeout 单个生命周期动作超时（spec US-4）。
const scriptExecTimeout = 60 * time.Second

// scriptOutputLimit 输出留痕截断（字节）。
const scriptOutputLimit = 2048

// lifecycleScripts 生命周期白名单（包根文件名；缺失的动作跳过）。
var lifecycleScripts = []string{"install", "start", "stop", "uninstall"}

// ScriptState 单脚本应用生命周期状态（PodStatus phase 映射的源）。
type ScriptState struct {
	App     string `json:"app"`
	Version string `json:"version"`
	Phase   string `json:"phase"` // running|failed|stopped
	Detail  string `json:"detail"`
	At      int64  `json:"at"`
}

// scriptTask 队列任务（HandleConfigSync 投递，worker 串行消费——EdgeHub 读循环
// 不被 install/start 最长 ~120s 执行阻塞，v0.47 复核 P2-2）。
type scriptTask struct {
	raw []byte
}

// scriptTaskQueueCap 任务队列容量（满时丢弃 + Warn 留痕——边缘过载保护）。
const scriptTaskQueueCap = 64

// ScriptRunner 边缘脚本应用执行器（并发安全；ConfigSync 回调投递 + 单 worker 串行执行）。
type ScriptRunner struct {
	enabled bool
	dir     string // 缓存/工作根目录

	tasks chan scriptTask // 仅 enabled 时非 nil

	// sink 是可选生命周期事件汇（v0.48.0 日志链路接线点：logcollector.Emit）；
	// nil 安全，调用不得阻塞（logcollector 满缓冲丢最旧不阻塞）。
	sink func(source, level, message string)

	mu     sync.Mutex
	states map[string]*ScriptState // key = app
}

// NewScriptRunner 读环境装配：EDGEFLOW_EDGED_SCRIPTRUNNER=on 启用；
// 缓存目录 EDGEFLOW_EDGED_APPPKG_DIR（缺省系统临时目录下 edgeflow-edged-apppkg）。
// 未启用时返回零行为实例（HandleConfigSync 仅 Warn）。
func NewScriptRunner() *ScriptRunner {
	enabled := os.Getenv(EnvScriptRunner) == "on"
	dir := os.Getenv(EnvAppPkgDir)
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "edgeflow-edged-apppkg")
	}
	s := &ScriptRunner{enabled: enabled, dir: dir, states: map[string]*ScriptState{}}
	if enabled {
		s.tasks = make(chan scriptTask, scriptTaskQueueCap)
		go s.worker()
	}
	return s
}

// SetEventSink 注册生命周期事件汇（v0.48.0 日志链路；nil 取消）。并发安全。
func (s *ScriptRunner) SetEventSink(fn func(source, level, message string)) {
	s.mu.Lock()
	s.sink = fn
	s.mu.Unlock()
}

// emit 尽力投递一条生命周期事件（锁外调用 sink——sink 自身并发安全）。
func (s *ScriptRunner) emit(source, level, msg string) {
	s.mu.Lock()
	fn := s.sink
	s.mu.Unlock()
	if fn != nil {
		fn(source, level, msg)
	}
}

// worker 串行消费配置任务（FIFO 保序：先 deploy 后 delete 不会乱序）。
func (s *ScriptRunner) worker() {
	for t := range s.tasks {
		s.apply(t.raw)
	}
}

// Enabled 报告是否启用（装配日志用）。
func (s *ScriptRunner) Enabled() bool { return s.enabled }

// scriptConfigPayload 是 ConfigSync 消息的边缘侧最小解析形态（与云端下发一致）。
type scriptConfigPayload struct {
	Operation string `json:"operation"`
	Config    struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Kind      string            `json:"kind"`
		Data      map[string]string `json:"data"`
	} `json:"config"`
}

// scriptAppData 是脚本应用 ConfigMap data 的约定键集（spec US-4）。
type scriptAppData struct {
	App       string // data["app"]
	Version   string // data["version"]
	ScriptRef string // data["scriptRef"]
	SHA256    string // data["sha256"]（包内容十六进制摘要）
	ScriptTar string // data["scriptTar"]（tar.gz 字节流 base64）
}

// HandleConfigSync 处理一条 ConfigSync 消息（在 MetaManager 落盘成功后调用）：
// 非脚本配置零行为；未启用时 Warn 仅落盘（由 MetaManager 完成）；
// 脚本配置按生命周期执行（失败记状态不回错——配置落盘本身已成功，
// 执行态经 PodStatus 回报）。
func (s *ScriptRunner) HandleConfigSync(raw []byte) {
	if !s.enabled {
		var probe scriptConfigPayload
		if err := json.Unmarshal(raw, &probe); err == nil {
			d := probe.Config.Data
			if d["scriptRef"] != "" || d["scriptTar"] != "" {
				log.Warnf("[scriptrunner] 脚本应用配置仅落盘不执行（opt-in %s=on 开启）", EnvScriptRunner)
			}
		}
		return
	}
	select {
	case s.tasks <- scriptTask{raw: raw}:
	default:
		log.Warnf("[scriptrunner] 任务队列已满（%d）——配置丢弃留痕（边缘过载）", scriptTaskQueueCap)
	}
}

// apply 同步执行一条配置任务（worker 内串行调用）。
func (s *ScriptRunner) apply(raw []byte) {
	var p scriptConfigPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return // 非约定形态：零行为（MetaManager 已自行落盘/报错）
	}
	d := p.Config.Data
	if p.Operation == "delete" {
		// delete 负载只带 name/namespace（无 data）——按对象名前缀识别脚本应用
		app := d["app"]
		if app == "" {
			app = strings.TrimPrefix(p.Config.Name, "edgeflow-app-")
		}
		if !s.enabled {
			log.Warnf("[scriptrunner] 脚本应用 %s 删除仅落盘（opt-in %s=on 开启）", app, EnvScriptRunner)
			return
		}
		s.uninstall(app)
		return
	}
	if d["scriptRef"] == "" && d["scriptTar"] == "" {
		return // 非脚本应用配置：零行为
	}
	app := d["app"]
	if app == "" {
		app = strings.TrimPrefix(p.Config.Name, "edgeflow-app-")
	}
	if !s.enabled {
		log.Warnf("[scriptrunner] 脚本应用 %s 配置仅落盘不执行（opt-in %s=on 开启）", app, EnvScriptRunner)
		return
	}
	s.deploy(&scriptAppData{
		App: app, Version: d["version"], ScriptRef: d["scriptRef"],
		SHA256: d["sha256"], ScriptTar: d["scriptTar"],
	})
}

// deploy 执行脚本应用落地全链：解码 → sha256 校验 → 落缓存 → 解包 →
// install → start。任一步失败：状态 failed + 明确 detail（PodStatus 回报）。
func (s *ScriptRunner) deploy(d *scriptAppData) {
	key := d.App
	fail := func(format string, args ...any) {
		log.Errorf("[scriptrunner] %s 部署失败: %s", d.App, fmt.Sprintf(format, args...))
		s.setState(key, &ScriptState{App: d.App, Version: d.Version, Phase: "failed",
			Detail: fmt.Sprintf(format, args...), At: time.Now().UnixMilli()})
		s.emit("scriptrunner", "error", d.App+"@"+d.Version+" 部署失败: "+fmt.Sprintf(format, args...))
	}
	if d.ScriptTar == "" {
		fail("配置缺少 scriptTar（包内联体）")
		return
	}
	blob, err := base64.StdEncoding.DecodeString(d.ScriptTar)
	if err != nil {
		fail("scriptTar base64 解码失败: %v", err)
		return
	}
	// sha256 硬校验（fail-closed）：缺字段/不一致均拒绝执行（spec 0020 安全硬规则；
	// v0.47 复核 P1-1：原实现缺 checksum 时跳过校验构成 fail-open）
	if d.SHA256 == "" {
		fail("配置缺少 sha256——拒绝执行（fail-closed）")
		return
	}
	{
		sum := sha256.Sum256(blob)
		if got := hex.EncodeToString(sum[:]); got != d.SHA256 {
			fail("sha256 不一致（云端登记 %.12s… ↔ 边缘落地 %.12s…）——拒绝执行", d.SHA256, got)
			return
		}
	}
	// 缓存目录落包（离线重启用）
	appDir := filepath.Join(s.dir, filepath.Base(d.App))
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		fail("缓存目录创建失败: %v", err)
		return
	}
	ver := filepath.Base(d.Version)
	pkgPath := filepath.Join(appDir, ver+".tgz")
	if err := os.WriteFile(pkgPath, blob, 0o600); err != nil {
		fail("包缓存写盘失败: %v", err)
		return
	}
	// 专用工作目录（每次部署清空重建；App/ver 经 Base 收敛——防目录逃逸，
	// v0.47 复核 P1-2：缓存路径已 Base 而 work 路径漏收，自相矛盾）
	work := filepath.Join(s.dir, "work", filepath.Base(d.App)+"-"+filepath.Base(ver))
	_ = os.RemoveAll(work)
	if err := extractTarGz(blob, work); err != nil {
		fail("包解包失败: %v", err)
		return
	}
	// 生命周期：install → start（缺失动作跳过；失败即中止）
	for _, act := range []string{"install", "start"} {
		if out, err := s.runScript(work, act); err != nil {
			fail("%s 动作失败（exit=%v）: %.256s", act, exitCode(err), out)
			return
		}
	}
	log.Infof("[scriptrunner] %s@%s 已部署并启动（work=%s）", d.App, d.Version, work)
	s.setState(key, &ScriptState{App: d.App, Version: d.Version, Phase: "running",
		Detail: "", At: time.Now().UnixMilli()})
	s.emit("scriptrunner", "info", d.App+"@"+d.Version+" 已部署并启动")
}

// uninstall 执行 stop → uninstall → 清理工作目录与状态（尽力；失败记日志）。
func (s *ScriptRunner) uninstall(app string) {
	s.mu.Lock()
	st := s.states[app]
	s.mu.Unlock()
	if st == nil {
		return // 未知应用：零行为
	}
	ver := st.Version
	work := filepath.Join(s.dir, "work", filepath.Base(app)+"-"+filepath.Base(ver))
	for _, act := range []string{"stop", "uninstall"} {
		if _, err := s.runScript(work, act); err != nil {
			log.Warnf("[scriptrunner] %s %s 动作失败（继续清理）: %v", app, act, err)
		}
	}
	_ = os.RemoveAll(work)
	log.Infof("[scriptrunner] %s 已卸载（工作目录已清理）", app)
	s.emit("scriptrunner", "info", app+"@"+ver+" 已卸载")
	s.mu.Lock()
	delete(s.states, app)
	s.mu.Unlock()
}

// runScript 受控执行包内生命周期脚本：sh -e <act>，60s 超时、专用工作目录、
// 输出截断留痕（审计：动作/退出码/耗时/输出摘要）。
func (s *ScriptRunner) runScript(work, act string) (string, error) {
	path := filepath.Join(work, act)
	if _, err := os.Stat(path); err != nil {
		return "", nil // 动作缺失：跳过（白名单最小化，非全量强制）
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), scriptExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-e", act)
	cmd.Dir = work
	// 最小 env（v0.47 复核 P2-3）：不继承守护进程全量环境——防凭证类变量
	// 泄漏进包内脚本；额外变量按声明式注入留后续版本。
	cmd.Env = []string{
		"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + work,
		"TMPDIR=" + os.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	if len(out) > scriptOutputLimit {
		out = out[:scriptOutputLimit]
	}
	log.Infof("[scriptrunner] 审计 action=%s exit=%v cost=%dms out=%.128s", act, exitCode(err), time.Since(start).Milliseconds(), out)
	return string(out), err
}

// StatusPayloads 把脚本应用状态映射为 PodStatus 形态（契约零新增消息；
// pod.name = edgeflow-script-<app>，phase：running→Running / failed→Failed /
// stopped→Stopped；failed→Error 与 PodStatus 既有 Error 相位对齐）。未启用/无状态返回 nil。
func (s *ScriptRunner) StatusPayloads(nodeID string) []PodStatusPayload {
	if !s.enabled {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PodStatusPayload, 0, len(s.states))
	for _, st := range s.states {
		phase := "Running"
		switch st.Phase {
		case "failed":
			phase = "Error" // PodStatus 无 failed 相位——映射既有 Error
		case "stopped":
			phase = "Stopped"
		}
		out = append(out, PodStatusPayload{
			NodeID: nodeID, PodName: "edgeflow-script-" + st.App,
			Namespace: "edgeflow", Phase: phase, Message: st.Detail,
			LastReconcileAt: st.At,
		})
	}
	return out
}

// setState 登记应用状态（并发安全）。
func (s *ScriptRunner) setState(app string, st *ScriptState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[app] = st
}

// extractTarGz 解包 tar.gz 到目标目录（防路径穿越：拒绝 .. / 绝对路径成员）。
func extractTarGz(blob []byte, dest string) error {
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return err
	}
	zr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return fmt.Errorf("gzip 打开失败: %w", err)
	}
	defer func() { _ = zr.Close() }()
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar 读取失败: %w", err)
		}
		name := hdr.Name
		if strings.Contains(name, "..") || filepath.IsAbs(name) {
			return fmt.Errorf("包成员路径非法（拒绝路径穿越）: %s", name)
		}
		target := filepath.Join(dest, filepath.FromSlash(name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			data, err := io.ReadAll(io.LimitReader(tr, 64<<20))
			if err != nil {
				return err
			}
			if err := os.WriteFile(target, data, 0o700); err != nil {
				return err
			}
		default:
			// 链接/设备等：跳过（执行面只需要普通文件）
		}
	}
}

// exitCode 提取进程退出码（-1 = 非退出错误，如超时/启动失败）。
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}
