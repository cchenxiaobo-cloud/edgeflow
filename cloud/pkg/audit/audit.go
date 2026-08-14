// Package audit 实现 cloudcore 管理 API 的审计日志（WBS 7.5）。
//
// 存储选型：JSONL（每行一条 JSON 记录，追加写、绝不覆盖），与
// edge/pkg/metamanager 的 op_ledger（SQLite 台账）同模式、异介质：
//   - JSONL 零依赖、可用 tail/grep/jq 直接排查，适合审计量小的 API 调用场景；
//   - 追加写天然免去 UPDATE 路径（审计记录不可变），无需建表/索引；
//   - 单文件轮转留给日志采集层（如 Fluent Bit）处理，本包只管追加。
//
// 记录字段：{ts, action, path, method, status, operator, ip}。
//
// 中间件协作（身份传递）：审计中间件在请求 context 中挂一个身份槽
// （IdentityAnonymous 起步），认证中间件（cloud/pkg/auth）校验通过后
// 调用 SetIdentity 写入身份；请求结束后审计中间件读取身份槽落盘。
// 这样认证失败（401）也会被审计（operator=anonymous），安全审计不丢
// 未授权访问尝试。
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"edgeflow/pkg/log"
)

// 常量定义。
const (
	// DefaultPath 是审计台账默认路径（可用环境变量 EDGEFLOW_CLOUDCORE_AUDIT_PATH 覆盖）。
	DefaultPath = "data/audit-ledger.jsonl"
	// EnvPath 是覆盖审计台账路径的环境变量名。
	EnvPath = "EDGEFLOW_CLOUDCORE_AUDIT_PATH"
	// IdentityAnonymous 是未认证请求的审计身份标识。
	IdentityAnonymous = "anonymous"
	// filePerm 是审计台账文件权限（0600：含来源 IP 等敏感信息）。
	filePerm = 0o600
	// dirPerm 是自动创建的目录权限（0700）。
	dirPerm = 0o700
)

// Entry 是一条审计记录（JSONL 的一行）。
type Entry struct {
	Ts       int64  `json:"ts"`       // 请求完成时间（毫秒时间戳）
	Action   string `json:"action"`   // 动作：方法 + 路由模式（如 "GET /api/v1/nodes/{nodeID}"）
	Path     string `json:"path"`     // 实际请求路径（如 /api/v1/nodes/node-1）
	Method   string `json:"method"`   // HTTP 方法
	Status   int    `json:"status"`   // 响应状态码
	Operator string `json:"operator"` // 操作者（认证身份；未认证为 anonymous）
	IP       string `json:"ip"`       // 客户端 IP（不含端口）
}

// Ledger 是云端 API 审计台账：JSONL 追加写。
// 并发安全：mu 串行化 编码+写入，多 goroutine（HTTP 并发请求）可同时调用。
type Ledger struct {
	mu   sync.Mutex
	file *os.File
	path string
}

// NewLedger 打开（或创建）审计台账文件并初始化：
//   - 父目录不存在时自动创建（0700）；
//   - 文件以 O_APPEND 打开（追加写），权限 0600；
//   - 已存在的文件继续追加（重启不丢历史审计记录）。
func NewLedger(path string) (*Ledger, error) {
	if path == "" {
		return nil, errors.New("审计台账路径不能为空")
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return nil, fmt.Errorf("创建审计台账目录 %s 失败: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		return nil, fmt.Errorf("打开审计台账 %s 失败: %w", path, err)
	}
	return &Ledger{file: f, path: path}, nil
}

// Path 返回台账文件路径（用于日志输出/测试断言）。
func (l *Ledger) Path() string {
	return l.path
}

// Record 追加一条审计记录（JSON + 换行）。
func (l *Ledger) Record(e Entry) error {
	if l == nil || l.file == nil {
		return errors.New("审计台账未初始化")
	}
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("编码审计记录失败: %w", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.file.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("写入审计记录失败: %w", err)
	}
	return nil
}

// Close 关闭台账文件。
func (l *Ledger) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	return l.file.Close()
}

// identityKey 是请求身份槽在 context 中的键（本包私有，经 SetIdentity 写入）。
type identityKey struct{}

// identitySlot 是挂在请求 context 上的身份槽：认证中间件写入，审计中间件读取。
// 用指针而非直接存 string，是为了让内层中间件（认证）能就地修改、
// 外层中间件（审计）在请求结束后读取到最新值。
type identitySlot struct {
	identity string
}

// SetIdentity 把操作者身份写入请求 context（认证中间件调用）。
// context 中没有身份槽（如未经过审计中间件包装）时静默忽略。
func SetIdentity(ctx context.Context, identity string) {
	if slot, ok := ctx.Value(identityKey{}).(*identitySlot); ok {
		slot.identity = identity
	}
}

// Middleware 包装 HTTP handler：每个请求完成后落一条审计记录。
//
// 记录动作（action）= 方法 + 路由模式（r.Pattern，低基数），路径（path）=
// 实际请求路径。审计失败只记错误日志、不阻断业务（可用性优先，
// 但日志持续报错即暴露审计链路故障）。
func (l *Ledger) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if l == nil || l.file == nil {
			// 台账未初始化（防御）：退化为透传，不拦业务
			next.ServeHTTP(w, r)
			return
		}
		// 挂身份槽：内层认证中间件通过 SetIdentity 写入操作者。
		// 注意：WithContext 返回请求副本，后续 Pattern/URL 一律从 r2 读取
		// （ServeMux 在匹配时把 Pattern 写到它收到的那个请求上）。
		slot := &identitySlot{identity: IdentityAnonymous}
		r2 := r.WithContext(context.WithValue(r.Context(), identityKey{}, slot))

		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r2)

		// 内层 ServeMux 把路由模式写在 r2（WithContext 副本）上；
		// 回写到原始请求 r，让外层中间件（如 metrics 计数）能取到
		// 具体路由模式（而非外层前缀 /api/v1/）。两处读写同 goroutine
		// 顺序执行，无并发问题。
		if r2.Pattern != "" {
			r.Pattern = r2.Pattern
		}

		action := r2.Pattern
		if action == "" {
			action = r2.URL.Path // 未匹配路由（404）回退实际路径
		} else if i := strings.IndexByte(action, ' '); i >= 0 {
			action = action[i+1:] // 去掉 ServeMux 模式的方法前缀，由 Method 字段单独表达
		}
		entry := Entry{
			Ts:       time.Now().UnixMilli(),
			Action:   r2.Method + " " + action,
			Path:     r2.URL.Path,
			Method:   r2.Method,
			Status:   rec.statusCode(),
			Operator: slot.identity,
			IP:       clientIP(r2),
		}
		if err := l.Record(entry); err != nil {
			log.Errorf("审计记录写入失败: %v", err)
		}
	})
}

// statusRecorder 记录响应状态码（与 metrics 包同模式；独立实现避免包间耦合）。
type statusRecorder struct {
	http.ResponseWriter
	code        int
	wroteHeader bool
}

func (r *statusRecorder) statusCode() int {
	if !r.wroteHeader {
		return http.StatusOK
	}
	return r.code
}

// WriteHeader 记录状态码并透传；重复调用忽略（与 net/http 行为一致）。
func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.code = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

// Write 未显式 WriteHeader 时按 200 计，然后透传写入。
func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.code = http.StatusOK
		r.wroteHeader = true
	}
	return r.ResponseWriter.Write(b)
}

// clientIP 提取客户端 IP（去掉端口；代理头不信任，直接用 RemoteAddr）。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
