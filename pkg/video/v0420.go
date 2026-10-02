// RTSP 帧源（v0.42.0，specs/0015 US-3）：自研 RTSP 客户端拉流（RTP over TCP
// interleaved → H.264 AnnexB）+ 外部解码进程（H.264 AnnexB stdin → MJPEG
// stdout，如 ffmpeg）→ jpegScanner 出帧。
//
// 生命周期与自愈（对齐 MJPEGSource/BridgeSource 语义）：
//   - RTSP 断开（EOF/重置）或解码进程退出 → 强制回收 + 退避重连（Reconnects
//     计数；瞬时失败循环重试，直到成功或 ctx 取消）；
//   - 配置性错误（URL/decoder 缺失）构造期显式拒绝；
//   - 解码进程 stderr 尾部保留用于诊断（decoderStderr）；
//   - ctx 取消 → Kill 解码进程 + 关闭 stdout 读端解除 Read 阻塞。
//
// 单 goroutine 独占使用（Next 串行）。
package video

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"edgeflow/pkg/rtspclient"
)

// RTSPConfig 是 RTSP 帧源配置。
type RTSPConfig struct {
	URL         string // rtsp://[user:pass@]host[:port]/path
	Decoder     string // H.264→MJPEG 外部解码命令（如 ffmpeg）
	ReconnectMs int    // 断流重连间隔（默认 1000）
	TimeoutMs   int    // RTSP 信令超时（默认 5000）
}

// defaultRTSPTimeoutMs 是 RTSP 信令默认超时（毫秒）。
const defaultRTSPTimeoutMs = 5000

// RTSPSource 是 RTSP 帧源：RTSP 客户端（拉流）+ 外部解码进程（出帧）组合。
type RTSPSource struct {
	cfg        RTSPConfig
	seq        uint64
	reconnects atomic.Uint64
	badFrames  atomic.Uint64

	mu      sync.Mutex
	client  *rtspclient.Client
	procMu  sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	stdoutR *bufio.Reader
	stderr  *tailBuffer
	procEnd chan struct{} // 当前进程 Wait 完成信号
	scanner jpegScanner
	pending [][]byte
}

// NewRTSPSource 创建 RTSP 帧源（不连接——首次 Next 时建立）。
func NewRTSPSource(cfg RTSPConfig) *RTSPSource {
	if cfg.ReconnectMs <= 0 {
		cfg.ReconnectMs = defaultReconnectMs
	}
	if cfg.TimeoutMs <= 0 {
		cfg.TimeoutMs = defaultRTSPTimeoutMs
	}
	return &RTSPSource{cfg: cfg}
}

// Reconnects 返回累计重连次数。
func (s *RTSPSource) Reconnects() uint64 { return s.reconnects.Load() }

// BadFrames 返回累计丢弃的坏帧数。
func (s *RTSPSource) BadFrames() uint64 { return s.badFrames.Load() }

// Close 停止源：关闭 RTSP 连接 + 强制回收解码进程（幂等）。
func (s *RTSPSource) Close() error {
	s.mu.Lock()
	c := s.client
	s.client = nil
	s.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
	s.killDecoder()
	return nil
}

// Next 产出下一帧：建立/复用 RTSP 拉流与解码进程，AnnexB 喂 stdin，
// stdout 分帧交付；任何一环断开即整体重连（自愈——瞬时失败不终止）。
func (s *RTSPSource) Next(ctx context.Context) (*Frame, error) {
	var buf [32 << 10]byte
	for {
		// 1) 消化已切出的帧。
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
		// 2) 确保 RTSP 客户端与会话就绪。可重试失败（拨号/信令）在 ensureRTSP
		//    内退避后返回哨兵——继续重试（自愈；ctx 取消返回 ctx.Err 终止）。
		c, err := s.ensureRTSP(ctx)
		if err != nil {
			if errors.Is(err, errRetryRTSP) {
				continue
			}
			return nil, err
		}
		// 3) 确保解码进程就绪（句柄快照判空，避免未加锁读字段）。
		if s.stdinRef() == nil {
			if err := s.startDecoder(ctx); err != nil {
				return nil, err
			}
		}
		// 4) 泵 RTSP 媒体（AnnexB → 解码进程 stdin）；连接断开 → 整体重连。
		annexb, aerr := c.ReadAnnex()
		if aerr == nil && len(annexb) > 0 {
			stdin := s.stdinRef()
			if stdin == nil {
				return nil, fmt.Errorf("video: rtsp 解码进程 stdin 缺失（decoder stderr: %s）", s.decoderStderr())
			}
			if _, werr := stdin.Write(annexb); werr != nil {
				aerr = fmt.Errorf("video: 解码进程 stdin 写入失败: %w", werr)
			}
		}
		if aerr != nil {
			// RTSP 断流（EOF/重置）或解码进程 stdin 断开：回收 + 退避 + 重连。
			s.reconnects.Add(1)
			s.dropAll()
			if !s.sleepOrCancel(ctx) {
				return nil, ctx.Err()
			}
			continue
		}
		// 5) 从解码进程 stdout 读并分帧。
		stdoutR := s.stdoutRRef()
		if stdoutR == nil {
			return nil, fmt.Errorf("video: rtsp 解码进程 stdout 缺失（decoder stderr: %s）", s.decoderStderr())
		}
		n, rerr := stdoutR.Read(buf[:])
		if n > 0 {
			s.pending = append(s.pending, s.scanner.feed(buf[:n])...)
			continue
		}
		if rerr != nil {
			// stdout 读端被关（ctx 取消路径）或解码进程退出：回收 + 重连。
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			s.reconnects.Add(1)
			s.dropAll()
			if !s.sleepOrCancel(ctx) {
				return nil, ctx.Err()
			}
			continue
		}
	}
}

// ensureRTSP 建立 RTSP 连接与会话（已建立则复用）。失败路径：退避一次后
// 返回 errRetryRTSP（例外：ctx 取消返回 ctx.Err 终止）。
func (s *RTSPSource) ensureRTSP(ctx context.Context) (*rtspclient.Client, error) {
	s.mu.Lock()
	c := s.client
	s.mu.Unlock()
	if c != nil {
		return c, nil
	}
	nc, err := rtspclient.Dial(rtspclient.Config{
		URL:     s.cfg.URL,
		Timeout: time.Duration(s.cfg.TimeoutMs) * time.Millisecond,
	})
	if err != nil {
		s.reconnects.Add(1)
		if !s.sleepOrCancel(ctx) {
			return nil, ctx.Err()
		}
		return nil, errRetryRTSP
	}
	for _, step := range []func() error{nc.Options, nc.Describe, nc.Setup, nc.Play} {
		if err := step(); err != nil {
			_ = nc.Close()
			s.reconnects.Add(1)
			if !s.sleepOrCancel(ctx) {
				return nil, ctx.Err()
			}
			return nil, errRetryRTSP
		}
	}
	s.mu.Lock()
	s.client = nc
	s.mu.Unlock()
	return nc, nil
}

// startDecoder 启动外部解码进程（stdin/stdout 管道 + stderr 尾部缓冲）。
// watch-goroutine 只在 ctx 取消时 Kill（不重复 Wait——Wait 归 killDecoder 唯一调用）。
func (s *RTSPSource) startDecoder(ctx context.Context) error {
	if s.cfg.Decoder == "" {
		return errors.New("video: rtsp 解码命令为空（配置错误）")
	}
	parts := strings.Fields(s.cfg.Decoder)
	cmd := exec.Command(parts[0], parts[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("video: rtsp 解码 stdin 管道失败: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("video: rtsp 解码 stdout 管道失败: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("video: rtsp 解码 stderr 管道失败: %w", err)
	}
	tail := &tailBuffer{limit: 512}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("video: rtsp 解码命令启动失败（%s）: %w", s.cfg.Decoder, err)
	}
	go func() {
		_, _ = io.Copy(tail, stderrPipe)
	}()
	done := make(chan struct{})
	s.procMu.Lock()
	s.cmd = cmd
	s.stdin = stdin
	s.stdout = stdoutPipe
	s.stdoutR = bufio.NewReader(stdoutPipe)
	s.stderr = tail
	s.procEnd = done
	s.scanner.reset()
	s.procMu.Unlock()
	// watcher：ctx 取消 → Kill + 关闭 stdin/stdout 读端解除阻塞（照 BridgeSource）。
	go func() {
		select {
		case <-ctx.Done():
			s.procMu.Lock()
			if s.cmd == cmd { // 仍是本进程（未被 killDecoder 接管）
				_ = cmd.Process.Kill()
				_ = stdin.Close()
				if s.stdout != nil {
					_ = s.stdout.Close() // 关读端解除 stdout.Read 阻塞
				}
			}
			s.procMu.Unlock()
		case <-done:
		}
	}()
	return nil
}

// stdinRef / stdoutRRef 返回当前解码进程句柄（锁内快照——使用方仍需容忍短暂过期）。
func (s *RTSPSource) stdinRef() io.WriteCloser {
	s.procMu.Lock()
	defer s.procMu.Unlock()
	return s.stdin
}

func (s *RTSPSource) stdoutRRef() *bufio.Reader {
	s.procMu.Lock()
	defer s.procMu.Unlock()
	return s.stdoutR
}

// decoderStderr 返回解码进程 stderr 尾部（诊断用；锁内快照）。
func (s *RTSPSource) decoderStderr() string {
	s.procMu.Lock()
	defer s.procMu.Unlock()
	if s.stderr == nil {
		return ""
	}
	return s.stderr.String()
}

// takeProc 锁内一次性接管当前进程句柄组（cmd/stdin/stdout/done 全清）。
// killDecoder 与 startDecoder 经此协调：对同一进程「Kill + Wait + close(done)」
// 恰好一次。
func (s *RTSPSource) takeProc() (*exec.Cmd, io.WriteCloser, io.ReadCloser, chan struct{}) {
	s.procMu.Lock()
	cmd, stdin, stdout, done := s.cmd, s.stdin, s.stdout, s.procEnd
	s.cmd, s.stdin, s.stdout, s.stdoutR, s.procEnd = nil, nil, nil, nil, nil
	s.procMu.Unlock()
	return cmd, stdin, stdout, done
}

// killDecoder 接管并强制回收当前解码进程：Kill → 关 stdin/stdout → Wait →
// close(procEnd)。P2-9：Kill 先行（Wait 在后），避免「解码器关闭 stdin 却
// 仍存活」异常形态下 Wait 长阻塞。
func (s *RTSPSource) killDecoder() {
	cmd, stdin, stdout, done := s.takeProc()
	if cmd == nil {
		return
	}
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	if stdout != nil {
		_ = stdout.Close()
	}
	_ = cmd.Wait()
	if done != nil {
		close(done)
	}
}

// dropAll 整体重置：关闭 RTSP + 强制回收解码进程（自愈路径）。
func (s *RTSPSource) dropAll() {
	s.mu.Lock()
	c := s.client
	s.client = nil
	s.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
	s.killDecoder()
}

// sleepOrCancel 等待重连间隔；ctx 取消返回 false。
func (s *RTSPSource) sleepOrCancel(ctx context.Context) bool {
	t := time.NewTimer(time.Duration(s.cfg.ReconnectMs) * time.Millisecond)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// errRetryRTSP 是 RTSP 连接/信令阶段可重试错误的哨兵（Next 循环消费——
// 继续重试而非终止）。
var errRetryRTSP = errors.New("video: rtsp 会话建立失败（可重试）")
