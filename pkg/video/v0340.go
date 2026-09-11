// v0.34.0（specs/0007）视频流阶段二：实源接入——MJPEG over HTTP 直连源
// 与外部进程桥源（零第三方依赖：stdlib + os/exec；桥接路线唯一外部依赖
// 是用户自行安装的 ffmpeg 等命令，不引入 Go 模块依赖）。
package video

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// SourceConfig 是帧源配置（阶段二三型源；synthetic 为阶段一默认）。
type SourceConfig struct {
	Type        string          `json:"type"`                  // synthetic | mjpeg | bridge
	URL         string          `json:"url,omitempty"`         // mjpeg：http(s)://（可含内嵌凭证）
	Command     string          `json:"command,omitempty"`     // bridge：外部命令（如 ffmpeg）
	Args        []string        `json:"args,omitempty"`        // bridge：命令参数
	ReconnectMs int             `json:"reconnectMs,omitempty"` // mjpeg/bridge 断流重连间隔（默认 1000）
	TimeoutMs   int             `json:"timeoutMs,omitempty"`   // mjpeg：响应头超时（默认 0=不设）
	Synth       SyntheticConfig `json:"synthetic"`
}

// defaultReconnectMs 是断流重连的默认间隔（毫秒）。
const defaultReconnectMs = 1000

// NewSource 按配置构造帧源：synthetic/mjpeg/bridge；未知 type 显式拒绝
// （延续「不做静默降级」纪律，spec 0004 → 0007）。
func NewSource(cfg SourceConfig) (FrameSource, error) {
	switch cfg.Type {
	case "synthetic":
		return NewSyntheticSource(cfg.Synth), nil
	case "mjpeg":
		if cfg.URL == "" {
			return nil, errors.New("video: source.type=mjpeg 需要 url")
		}
		// URL 预检（v0340 复核 P2-3）：scheme/host 类永久性错误在构造期
		// 显式拒绝，不进入运行期重连循环（与「配置性错误显式返回」一致）。
		if u, err := url.Parse(cfg.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("video: source.type=mjpeg 的 url 非法（需 http(s)://host/...）: %q", cfg.URL)
		}
		return NewMJPEGSource(MJPEGConfig{URL: cfg.URL, ReconnectMs: cfg.ReconnectMs, TimeoutMs: cfg.TimeoutMs}), nil
	case "bridge":
		if cfg.Command == "" {
			return nil, errors.New("video: source.type=bridge 需要 command")
		}
		return NewBridgeSource(BridgeConfig{Command: cfg.Command, Args: cfg.Args, ReconnectMs: cfg.ReconnectMs}), nil
	default:
		return nil, fmt.Errorf("video: source.type=%q 不支持（synthetic/mjpeg/bridge）", cfg.Type)
	}
}

// ---------------------------------------------------------------------------
// MJPEG over HTTP 直连源
// ---------------------------------------------------------------------------

// MJPEGConfig 是 MJPEG 源参数。
type MJPEGConfig struct {
	URL         string // http(s)://host/path（multipart/x-mixed-replace 流）
	ReconnectMs int    // 断流/连接错误重连间隔（<=0 取默认 1000）
	TimeoutMs   int    // 响应头超时（毫秒）；0=不设（流读取不受限）
}

// MJPEGSource 是 MJPEG over HTTP 帧源：GET 长连接按 multipart boundary
// 分帧；网络错误/断流按间隔自动重连；流内容配置性错误（非 multipart、
// HTTP != 200）显式返回错误（供上层可见降级）。Frame.Seq 内部单调计数。
// 单 goroutine 独占使用（与 FrameSource 契约一致）。
type MJPEGSource struct {
	cfg        MJPEGConfig
	client     *http.Client
	seq        uint64
	reconnects atomic.Uint64
	badFrames  atomic.Uint64

	body io.ReadCloser
	mr   *multipart.Reader
}

// NewMJPEGSource 创建 MJPEG 源。
func NewMJPEGSource(cfg MJPEGConfig) *MJPEGSource {
	if cfg.ReconnectMs <= 0 {
		cfg.ReconnectMs = defaultReconnectMs
	}
	tr := &http.Transport{}
	if cfg.TimeoutMs > 0 {
		tr.ResponseHeaderTimeout = time.Duration(cfg.TimeoutMs) * time.Millisecond
	}
	return &MJPEGSource{cfg: cfg, client: &http.Client{Transport: tr}}
}

// Reconnects 返回累计重连次数（测试/诊断）。
func (s *MJPEGSource) Reconnects() uint64 { return s.reconnects.Load() }

// BadFrames 返回累计丢弃的坏帧数（JPEG 校验失败）。
func (s *MJPEGSource) BadFrames() uint64 { return s.badFrames.Load() }

// Next 产出下一帧：连接/断流自动重连（ctx 取消退出）；配置性错误返回。
func (s *MJPEGSource) Next(ctx context.Context) (*Frame, error) {
	for {
		if s.mr == nil {
			err := s.connect(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				var retry *retryableError
				if !errors.As(err, &retry) {
					return nil, err // 配置性错误：显式（上层可见降级）
				}
				s.reconnects.Add(1)
				if !s.sleepOrCancel(ctx) {
					return nil, ctx.Err()
				}
				continue // 重连后重试
			}
		}
		part, err := s.mr.NextPart()
		if err != nil {
			s.closeBody()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// 流结束/中断 → 重连。
			s.reconnects.Add(1)
			if !s.sleepOrCancel(ctx) {
				return nil, ctx.Err()
			}
			continue
		}
		data, rerr := io.ReadAll(io.LimitReader(part, 16<<20))
		if rerr != nil {
			s.closeBody()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			s.reconnects.Add(1)
			if !s.sleepOrCancel(ctx) {
				return nil, ctx.Err()
			}
			continue
		}
		w, h, ok := jpegDim(data)
		if !ok {
			s.badFrames.Add(1)
			continue // 坏帧跳过
		}
		s.seq++
		return &Frame{Seq: s.seq, TsMs: time.Now().UnixMilli(), Width: w, Height: h, JPEG: data}, nil
	}
}

// retryableError 标记可重试的连接错误（网络层；配置性错误不标）。
type retryableError struct{ err error }

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

// connect 建立 MJPEG 流连接。
func (s *MJPEGSource) connect(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.URL, nil)
	if err != nil {
		return fmt.Errorf("video: MJPEG 请求构造失败: %w", err)
	}
	req.Header.Set("Accept", "multipart/x-mixed-replace")
	resp, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &retryableError{fmt.Errorf("video: MJPEG 连接失败: %w", err)}
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return fmt.Errorf("video: MJPEG 流返回 HTTP %d", resp.StatusCode)
	}
	mt, params, perr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if perr != nil || !strings.HasPrefix(mt, "multipart/") || params["boundary"] == "" {
		resp.Body.Close()
		return fmt.Errorf("video: 非 multipart/x-mixed-replace 流（Content-Type=%q）", resp.Header.Get("Content-Type"))
	}
	s.body = resp.Body
	s.mr = multipart.NewReader(bufio.NewReader(resp.Body), params["boundary"])
	return nil
}

func (s *MJPEGSource) closeBody() {
	if s.body != nil {
		s.body.Close()
		s.body = nil
	}
	s.mr = nil
}

// sleepOrCancel 等待重连间隔；ctx 取消返回 false。
func (s *MJPEGSource) sleepOrCancel(ctx context.Context) bool {
	t := time.NewTimer(time.Duration(s.cfg.ReconnectMs) * time.Millisecond)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ---------------------------------------------------------------------------
// 外部进程桥源
// ---------------------------------------------------------------------------

// BridgeConfig 是进程桥源参数。
type BridgeConfig struct {
	Command     string   // 外部命令（如 "ffmpeg"）
	Args        []string // 命令参数
	ReconnectMs int      // 进程退出后重启间隔（<=0 取默认 1000；进程退出即错误）
}

// BridgeSource 是外部进程桥帧源：启动命令（如 ffmpeg 将 RTSP 转 MJPEG
// 经 stdout 输出），stdout 按 JPEG SOI/EOI 定界分帧；进程退出/启动失败
// 显式报错（含退出状态与 stderr 尾部 ≤512B）；ReconnectMs>0 时进程退出
// 自动重启（含正常退出——桥命令常以一次性拉流脚本形式提供）；ctx 取消
// Kill 回收。单 goroutine 独占使用。
type BridgeSource struct {
	cfg        BridgeConfig
	seq        uint64
	reconnects atomic.Uint64
	badFrames  atomic.Uint64

	// 进程句柄组由 procMu 保护（Next 读循环取局部句柄后锁外阻塞读；
	// watcher 在 ctx 取消时 Kill + Close 读端解除阻塞）。
	procMu       sync.Mutex
	cmd          *exec.Cmd
	stdout       io.ReadCloser
	stderr       *tailBuffer
	procEnd      chan struct{} // 当前进程 Wait 完成信号（watcher 退出用）
	procStartSeq uint64        // 当前进程启动时的帧序号（判「本进程是否产帧」）
	scanner      jpegScanner
	pending      [][]byte // 已切出未返回的帧（一次读块可含多帧）
}

// NewBridgeSource 创建进程桥源。
func NewBridgeSource(cfg BridgeConfig) *BridgeSource {
	if cfg.ReconnectMs <= 0 {
		cfg.ReconnectMs = defaultReconnectMs
	}
	return &BridgeSource{cfg: cfg}
}

// Reconnects 返回累计重启进程次数。
func (s *BridgeSource) Reconnects() uint64 { return s.reconnects.Load() }

// BadFrames 返回累计丢弃的坏帧数。
func (s *BridgeSource) BadFrames() uint64 { return s.badFrames.Load() }

// Next 产出下一帧：启动/重启进程并按流分帧；ctx 取消 Kill 回收。
// 一次读块可切出多帧——已切出未返回的帧进 pending 队列（防丢帧）。
func (s *BridgeSource) Next(ctx context.Context) (*Frame, error) {
	var buf [32 << 10]byte
	for {
		// 先消化已切出的帧（可能一次读块含多帧）。
		for len(s.pending) > 0 {
			fr := s.pending[0]
			s.pending = s.pending[1:]
			w, h, ok := jpegDim(fr)
			if !ok {
				s.badFrames.Add(1)
				continue
			}
			s.seq++
			return &Frame{Seq: s.seq, TsMs: time.Now().UnixMilli(), Width: w, Height: h, JPEG: fr}, nil
		}
		s.procMu.Lock()
		stdout, tail := s.stdout, s.stderr
		s.procMu.Unlock()
		if stdout == nil {
			if err := s.startProc(ctx); err != nil {
				return nil, err
			}
			s.procMu.Lock()
			stdout, tail = s.stdout, s.stderr
			s.procMu.Unlock()
		}
		n, err := stdout.Read(buf[:])
		if n > 0 {
			s.pending = append(s.pending, s.scanner.feed(buf[:n])...)
			continue // 回顶部消化（EOF 留待 pending 空后处理）
		}
		if err != nil {
			startSeq, werr := s.waitProc()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if s.seq == startSeq {
				// 本进程未产出任何帧即退出：显式错误（含退出状态 +
				// stderr 尾部 ≤512B）——上层可见降级（spec 0007 US-2）。
				return nil, fmt.Errorf("video: bridge 进程退出（未产出帧）: %v; stderr: %s", werr, tail.String())
			}
			// 曾产帧后退出：流中断语义 → 重连重启。
			s.reconnects.Add(1)
			if !s.sleepOrCancel(ctx) {
				return nil, ctx.Err()
			}
			continue
		}
	}
}

// startProc 启动桥命令进程。
//
// stderr 走 StderrPipe + 自读 goroutine（而非 cmd.Stderr=Writer）：后者由
// exec 内部拷贝 goroutine 承担且被 cmd.Wait 等待——当桥命令是 shell 包装
// （sh -c '...'）时，孙进程继承 stderr 写端，取消场景 Kill 只及直接子进程，
// Wait 会等待写端全关（卡到孙进程自然结束）。自读方案下 Wait 只等进程退出，
// 读端由 Wait/BridgeSource 关闭回收。
func (s *BridgeSource) startProc(ctx context.Context) error {
	cmd := exec.Command(s.cfg.Command, s.cfg.Args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("video: bridge stdout 管道失败: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("video: bridge stderr 管道失败: %w", err)
	}
	tail := &tailBuffer{limit: 512}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("video: bridge 命令启动失败（%s）: %w", s.cfg.Command, err)
	}
	go func() {
		// 读至 EOF/读端关闭（Wait 会关闭 parentIOPipes 读端解除本 goroutine）。
		_, _ = io.Copy(tail, stderrPipe)
	}()
	done := make(chan struct{})
	s.procMu.Lock()
	s.cmd, s.stdout, s.stderr = cmd, stdout, tail
	s.procStartSeq = s.seq
	s.procEnd = done
	s.procMu.Unlock()
	s.scanner.reset()
	go func() {
		select {
		case <-ctx.Done():
			s.procMu.Lock()
			if s.cmd == cmd { // 仍是本进程（未被 waitProc 回收）
				_ = cmd.Process.Kill()
				if s.stdout != nil {
					// 关闭读端解除阻塞 Read：Kill 可能只杀到直接子进程，
					// 孙进程（shell 包装场景）仍持管道写端——不依赖其退出。
					_ = s.stdout.Close()
				}
			}
			s.procMu.Unlock()
		case <-done:
		}
	}()
	return nil
}

// takeProc 锁内一次性接管当前进程句柄组（cmd/stdout/procEnd 全清）。
// waitProc 与 Close 经此协调：对同一进程「Wait + close(done)」恰好一次。
func (s *BridgeSource) takeProc() (*exec.Cmd, io.ReadCloser, chan struct{}) {
	s.procMu.Lock()
	cmd, stdout, done := s.cmd, s.stdout, s.procEnd
	s.cmd, s.stdout, s.stderr, s.procEnd = nil, nil, nil, nil
	s.procMu.Unlock()
	return cmd, stdout, done
}

// waitProc 回收当前进程（等待退出并关闭 watcher）；返回启动帧序号快照。
// 已被 Close 接管回收时直接返回。
func (s *BridgeSource) waitProc() (uint64, error) {
	s.procMu.Lock()
	startSeq := s.procStartSeq
	s.procMu.Unlock()
	cmd, _, done := s.takeProc()
	if cmd == nil {
		return startSeq, nil
	}
	err := cmd.Wait()
	close(done)
	return startSeq, err
}

// Close 停止当前桥进程并回收（幂等；实现 io.Closer，供上层放弃源时调用）。
// 场景（v0340 复核 P1）：出帧循环可在「帧交付后 select <-stop 退出」路径
// 直接终止而不再调用 Next——若无本方法，ctx 取消路径（watcher）只 Kill 不
// Wait，直接子进程将成为僵尸直至宿主进程退出。KILL 后 Wait 即时返回
// （stderr 为自读管道、stdout 读端主动关闭，不受孙进程写端影响）。
func (s *BridgeSource) Close() error {
	cmd, stdout, done := s.takeProc()
	if cmd == nil {
		return nil // 无在管进程（未启动或已回收）——幂等
	}
	_ = cmd.Process.Kill()
	if stdout != nil {
		_ = stdout.Close()
	}
	_ = cmd.Wait() // KILL 后回收；Wait 错误（如 signal: killed）不构成 Close 失败
	close(done)
	return nil
}

// sleepOrCancel 等待重启间隔；ctx 取消返回 false。
func (s *BridgeSource) sleepOrCancel(ctx context.Context) bool {
	t := time.NewTimer(time.Duration(s.cfg.ReconnectMs) * time.Millisecond)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// tailBuffer 是保留尾部 ≤limit 字节的并发安全写入缓冲（桥进程 stderr）。
type tailBuffer struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.limit:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// ---------------------------------------------------------------------------
// JPEG 定界工具
// ---------------------------------------------------------------------------

var (
	jpegSOI = []byte{0xFF, 0xD8}
	jpegEOI = []byte{0xFF, 0xD9}
)

// jpegScanner 是 JPEG 字节流定界状态机：跨读块累积、按 SOI/EOI 切出完整
// 帧（MJPEG 场景标准做法；合规编码器不在熵数据中产生裸 FFD9）。
type jpegScanner struct {
	buf []byte
}

// maxPendingFrameBytes 是未闭合帧的缓冲上限（畸形流防爆内存）。
const maxPendingFrameBytes = 16 << 20

func (s *jpegScanner) reset() { s.buf = s.buf[:0] }

// feed 喂入读块，返回本次切出的完整 JPEG 帧列表。
func (s *jpegScanner) feed(chunk []byte) [][]byte {
	var out [][]byte
	s.buf = append(s.buf, chunk...)
	for {
		i := bytes.Index(s.buf, jpegSOI)
		if i < 0 {
			// 无 SOI：保留尾部 1 字节（FF 可能跨块）。
			if len(s.buf) > 1 {
				s.buf = append(s.buf[:0], s.buf[len(s.buf)-1:]...)
			}
			break
		}
		if i > 0 {
			s.buf = append(s.buf[:0], s.buf[i:]...) // 丢弃 SOI 前垃圾
		}
		j := bytes.Index(s.buf[2:], jpegEOI)
		if j < 0 {
			if len(s.buf) > maxPendingFrameBytes {
				s.buf = s.buf[:0]
			}
			break
		}
		end := 2 + j + 2
		out = append(out, append([]byte(nil), s.buf[:end]...))
		s.buf = append(s.buf[:0], s.buf[end:]...)
	}
	return out
}

// jpegDim 校验 JPEG 并取宽高。
func jpegDim(b []byte) (int, int, bool) {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 || b[len(b)-2] != 0xFF || b[len(b)-1] != 0xD9 {
		return 0, 0, false
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return 0, 0, false
	}
	return cfg.Width, cfg.Height, true
}
