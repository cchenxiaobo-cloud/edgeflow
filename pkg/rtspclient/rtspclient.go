// Package rtspclient 提供自研 RTSP 客户端最小子集（v0.42.0，spec 0015 US-1/US-2，
// 零依赖：仅标准库）。
//
// 最小子集边界（发展规划 G20 + spec 0015 裁定）：
//   - 信令：OPTIONS（选项协商）/ DESCRIBE（SDP 最小解析）/ SETUP（RTP/AVP/TCP
//     interleaved）/ PLAY / TEARDOWN；CSeq 自增；基础认证（401 挑战 →
//     Authorization: Basic，URL 内嵌凭证或显式 SetCredentials）；超时控制；
//     单连接复用（信令与 interleaved 媒体数据同连接分派）。
//   - 数据：RTP over TCP（`$`+channel+length 定界）；RTP 头解析（V/CC/PT/SEQ/
//     TS/SSRC，X 扩展头与 CSRC 跳过）；video 载荷重组：H.264 FU-A 分片拼装 /
//     单 NAL 直通 / STAP-A 分解 → AnnexB 字节流（00 00 00 01 + NAL）。
//
// 边界（登记 KI §43）：不做 UDP 传输、重定向跟随、SRTP、H.265、音频轨、
// 多轨（取首个 video 轨）；AnnexB 输出不解码（由外部 ffmpeg 出帧）。
// GB28181 SIP 不在本版（单独立项评估）。
package rtspclient

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 信令方法常量。
const (
	MethodOptions  = "OPTIONS"
	MethodDescribe = "DESCRIBE"
	MethodSetup    = "SETUP"
	MethodPlay     = "PLAY"
	MethodTeardown = "TEARDOWN"

	interleavedMagic = 0x24 // '$'
	defaultRTSPPort  = "554"
)

// 业务错误（errors.Is 可判）。
var (
	ErrUnauthorized  = errors.New("rtspclient: 401 未授权（且未提供凭证）")
	ErrBadStatusLine = errors.New("rtspclient: 非法状态行")
	ErrBadResponse   = errors.New("rtspclient: 响应非法")
)

// Client 是 RTSP 客户端（单连接：信令与 interleaved RTP 数据复用）。
// 并发约束：信令方法与 ReadAnnex 由同一 goroutine 串行调用（PLAY 后读循环独占）。
type Client struct {
	conn net.Conn
	br   *bufio.Reader

	url      string // 归一化请求 URL（rtsp://host[:port]/path）
	baseURL  string // 控制路径拼接基准（DESCRIBE 的 URL）
	user     string
	pass     string
	cseq     int
	session  string
	server   string // OPTIONS 应答 Server 头
	public   string // OPTIONS 应答 Public 头
	control  string // SDP 会话/轨控制路径（TrackURL 基准）
	videoCh  int    // interleaved video channel（SETUP 协商，默认 0）
	timeout  time.Duration
	sendAuth bool // 收到 401 后本请求已带认证（防重试环）

	// RTP→AnnexB 组装态（复用包级函数 + 会话内分片态）。
	fuState  *fuAssembler
	dropped  uint64
	lastSeq  uint16
	haveSeq  bool
	annexbMu sync.Mutex
	annexb   []byte // ReadAnnex 取走的输出缓冲
}

// Config 是拨号配置。
type Config struct {
	URL     string        // rtsp://[user:pass@]host[:port]/path
	Timeout time.Duration // 信令读写超时（默认 5s）
}

// Dial 建立 RTSP 客户端（TCP 连接；不发信令）。
func Dial(cfg Config) (*Client, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	user, pass, hostport, path := parseRTSPURL(cfg.URL)
	if hostport == "" {
		return nil, fmt.Errorf("rtspclient: URL 缺 host: %q", cfg.URL)
	}
	conn, err := net.DialTimeout("tcp", hostport, cfg.Timeout)
	if err != nil {
		return nil, fmt.Errorf("rtspclient: 拨号 %s 失败: %w", hostport, err)
	}
	url := "rtsp://" + hostport + path
	if user != "" {
		url = cfg.URL // 保留内嵌凭证形态（服务端按原 URL 匹配资源）
	}
	return &Client{
		conn:    conn,
		br:      bufio.NewReader(conn),
		url:     url,
		baseURL: url,
		user:    user,
		pass:    pass,
		timeout: cfg.Timeout,
	}, nil
}

// Close 关闭连接（幂等）。
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// SetCredentials 显式设置基础认证凭证（URL 未内嵌时使用）。
func (c *Client) SetCredentials(user, pass string) {
	c.user, c.pass = user, pass
}

// Capabilities 返回 OPTIONS 协商结果（Server/Public；诊断用）。
func (c *Client) Capabilities() (server, public string) { return c.server, c.public }

// Session 返回当前会话 ID（SETUP 后非空）。
func (c *Client) Session() string { return c.session }

// DroppedRTP 返回 RTP 层丢弃计数（畸形包/非 video 通道）。
func (c *Client) DroppedRTP() uint64 { return c.dropped }

// Options 选项协商（记录 Server/Public）。
func (c *Client) Options() error {
	code, headers, _, err := c.roundTrip(MethodOptions, c.url, nil)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("rtspclient: OPTIONS %d", code)
	}
	c.server, c.public = headers["server"], headers["public"]
	return nil
}

// Describe 拉取 SDP 并做最小解析（首个 video 轨控制路径 → 会话拼接基准）。
func (c *Client) Describe() error {
	code, _, body, err := c.roundTrip(MethodDescribe, c.url, map[string]string{"Accept": "application/sdp"})
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("rtspclient: DESCRIBE %d", code)
	}
	c.control = parseSDPControl(string(body), c.baseURL)
	return nil
}

// Setup 建立 RTP/AVP/TCP interleaved 会话（Transport 头；video channel 取
// 服务端 interleaved=xx-yy 的 xx，缺省 0）。
func (c *Client) Setup() error {
	trackURL := c.control
	if trackURL == "" {
		trackURL = c.baseURL + "/trackID=0"
	}
	code, h, _, err := c.roundTrip(MethodSetup, trackURL, map[string]string{
		"Transport": "RTP/AVP/TCP;interleaved=0-1",
	})
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("rtspclient: SETUP %d", code)
	}
	c.session = h["session"]
	c.videoCh = parseInterleavedChannel(h["transport"])
	return nil
}

// Play 开始推流（带 Session）。
func (c *Client) Play() error {
	code, _, _, err := c.roundTrip(MethodPlay, c.url, nil)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("rtspclient: PLAY %d", code)
	}
	return nil
}

// Teardown 结束会话（尽力而为；错误仅返回不重试）。
func (c *Client) Teardown() error {
	code, _, _, err := c.roundTrip(MethodTeardown, c.url, nil)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("rtspclient: TEARDOWN %d", code)
	}
	return nil
}

// TrackURL 返回 SETUP 使用的轨地址（诊断）。
func (c *Client) TrackURL() string { return c.control }

// roundTrip 发送请求 + 读响应（401 且有凭证时补认证重试一次；
// 读过程中 interleaved RTP 数据帧透明分派至 AnnexB 缓冲）。
func (c *Client) roundTrip(method, uri string, extra map[string]string) (int, map[string]string, []byte, error) {
	code, h, body, err := c.doRequest(method, uri, extra, false)
	if err != nil {
		return 0, nil, nil, err
	}
	if code == 401 && c.user != "" && !c.sendAuth {
		// 带认证重试一次（CSeq 递增由 doRequest 自增）。
		c.sendAuth = true
		code2, h2, body2, err2 := c.doRequest(method, uri, extra, true)
		c.sendAuth = false
		if err2 != nil {
			return 0, nil, nil, err2
		}
		return code2, h2, body2, nil
	}
	if code == 401 {
		return code, h, body, ErrUnauthorized
	}
	return code, h, body, nil
}

// doRequest 编码发送一条请求并读取响应。
func (c *Client) doRequest(method, uri string, extra map[string]string, withAuth bool) (int, map[string]string, []byte, error) {
	c.cseq++
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s RTSP/1.0\r\n", method, uri)
	fmt.Fprintf(&b, "CSeq: %d\r\n", c.cseq)
	fmt.Fprintf(&b, "User-Agent: EdgeFlow-RTSP/0.42\r\n")
	if withAuth && c.user != "" {
		fmt.Fprintf(&b, "Authorization: Basic %s\r\n", basicAuth(c.user, c.pass))
	}
	if c.session != "" && method != MethodSetup {
		fmt.Fprintf(&b, "Session: %s\r\n", c.session)
	}
	for k, v := range extra {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	b.WriteString("\r\n")
	_ = c.conn.SetDeadline(time.Now().Add(c.timeout))
	if _, err := c.conn.Write([]byte(b.String())); err != nil {
		return 0, nil, nil, fmt.Errorf("rtspclient: 写 %s 失败: %w", method, err)
	}
	return c.readResponse()
}

// readResponse 读取一条 RTSP 响应；响应读取过程中出现的 interleaved 媒体
// 数据帧被透明分派（AnnexB 缓冲）——调用方拿到的是纯信令响应。
// P2-4：drain 改为循环消化——高码率推流下「响应之前」可能积压多帧媒体数据，
// 单帧消化会把接续的媒体帧误判为非法状态行。
func (c *Client) readResponse() (int, map[string]string, []byte, error) {
	for {
		drained, err := c.drainInterleavedOnce()
		if err != nil {
			return 0, nil, nil, err
		}
		if !drained {
			break
		}
	}
	status, headers, body, err := c.readSignalMessage()
	if err != nil {
		return 0, nil, nil, err
	}
	return status, headers, body, nil
}

// drainInterleavedOnce 若流中下一个字节是 '$'（interleaved 帧），读出该帧
// 并按 channel 分派；否则不做任何事（下一条是信令文本）。返回是否消化了一帧。
func (c *Client) drainInterleavedOnce() (bool, error) {
	peeked, err := c.br.Peek(1)
	if err != nil {
		return false, err
	}
	if peeked[0] != interleavedMagic {
		return false, nil
	}
	ch, payload, err := c.readInterleavedFrame()
	if err != nil {
		return false, err
	}
	c.dispatchInterleaved(ch, payload)
	return true, nil
}

// dispatchInterleaved 按 interleaved channel 分派媒体帧：video channel →
// RTP 解析/重组；其余（如音频 channel）计数丢弃（P2-4——不进入 RTP 解析，
// 避免污染 video 侧排序/丢弃计数语义）。
func (c *Client) dispatchInterleaved(ch byte, payload []byte) {
	if int(ch) != c.videoCh {
		c.dropped++
		return
	}
	c.dispatchRTP(payload)
}

// readInterleavedFrame 读取一帧媒体数据：'$' + channel(1) + length(2, 大端)
// + payload(length)；返回 channel 与载荷。
func (c *Client) readInterleavedFrame() (byte, []byte, error) {
	head := make([]byte, 4)
	if _, err := io.ReadFull(c.br, head); err != nil {
		return 0, nil, fmt.Errorf("rtspclient: 读 interleaved 帧头失败: %w", err)
	}
	length := int(head[2])<<8 | int(head[3])
	if length <= 0 || length > 65535 {
		return 0, nil, fmt.Errorf("rtspclient: interleaved 帧长非法: %d", length)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return 0, nil, fmt.Errorf("rtspclient: 读 interleaved 帧体失败: %w", err)
	}
	return head[1], payload, nil
}

// dispatchRTP 解析 RTP 包并把 video 通道的 H.264 载荷重组进 AnnexB 缓冲。
func (c *Client) dispatchRTP(pkt []byte) {
	if len(pkt) < 12 {
		c.dropped++
		return
	}
	v2 := pkt[0]>>6 == 2
	if !v2 {
		c.dropped++
		return
	}
	cc := int(pkt[0] & 0x0F)
	x := pkt[0]>>4&1 == 1
	m := pkt[1]>>7 == 1
	pt := pkt[1] & 0x7F
	seq := uint16(pkt[2])<<8 | uint16(pkt[3])
	off := 12 + cc*4
	if x {
		if len(pkt) < off+4 {
			c.dropped++
			return
		}
		xlen := (int(pkt[off+2])<<8 | int(pkt[off+3])) * 4
		off += 4 + xlen
	}
	if off > len(pkt) {
		c.dropped++
		return
	}
	payload := pkt[off:]
	_ = m
	_ = pt
	// TCP 已保序：seq 异常仅作链路质量观测（重复/跳跃均计数到 dropped 由
	// 上层读数诊断——不丢有效载荷）。
	if c.haveSeq && seq != c.lastSeq+1 && seq != c.lastSeq {
		c.dropped++
	}
	c.lastSeq, c.haveSeq = seq, true
	if pt != 96 && pt != 97 { // 动态 PT 96/97 视为 video（音频 0-95 静态段已排除）；其余丢弃
		c.dropped++
		return
	}
	// 仅 video channel 帧进入本函数（dispatchInterleaved 已按 channel 过滤，
	// 音频等其余 channel 计数丢弃）；PT 96/97 为 video 载荷精细白名单。
	nals, derr := demuxH264(payload, &c.fuState)
	if derr != nil {
		c.dropped++
		return
	}
	c.annexbMu.Lock()
	for _, nal := range nals {
		c.annexb = append(c.annexb, annexbStart...)
		c.annexb = append(c.annexb, nal...)
	}
	c.annexbMu.Unlock()
}

// ReadAnnex 主动泵取 AnnexB H.264 字节流（PLAY 后由单一 goroutine 持续调用）：
// 以短超时 Peek 循环消化连接上已到达的媒体帧（interleaved）与偶发异步信令
// 响应，然后返回本次积累的 AnnexB 输出（无新数据返回空——调用方自行节流）。
//
// 错误语义（P1-1）：读超时（无新数据）不是错误；连接层断开（EOF/重置/关闭）
// 返回非 nil 错误——调用方据此触发断流重连（此前把 EOF 与读超时一并静默
// 吞掉，导致静默断流永不检测）。
func (c *Client) ReadAnnex() ([]byte, error) {
	if c.conn == nil {
		c.annexbMu.Lock()
		out := c.annexb
		c.annexb = nil
		c.annexbMu.Unlock()
		return out, nil
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	var cerr error
	for {
		peeked, err := c.br.Peek(1)
		if err != nil {
			if connBroken(err) {
				cerr = fmt.Errorf("rtspclient: 媒体连接断开: %w", err)
			}
			break // 读超时（无新数据）或连接断开：退出本窗口
		}
		switch peeked[0] {
		case interleavedMagic:
			ch, payload, rerr := c.readInterleavedFrame()
			if rerr != nil {
				if connBroken(rerr) {
					cerr = fmt.Errorf("rtspclient: 媒体连接断开: %w", rerr)
					goto done
				}
				c.dropped++ // 畸形帧：计数丢弃，不断流
				continue
			}
			c.dispatchInterleaved(ch, payload)
		default:
			// 异步信令响应（如迟到的 PLAY 应答/心跳）——消化并继续。
			if _, _, _, rerr := c.readSignalMessage(); rerr != nil {
				if connBroken(rerr) {
					cerr = fmt.Errorf("rtspclient: 媒体连接断开: %w", rerr)
				}
				goto done
			}
		}
	}
done:
	_ = c.conn.SetReadDeadline(time.Time{})
	c.annexbMu.Lock()
	out := c.annexb
	c.annexb = nil
	c.annexbMu.Unlock()
	return out, cerr
}

// connBroken 判断错误是否为「连接层断开」类（EOF / 连接重置 / 已关闭），
// 区别于读超时（Timeout()=true，属正常无数据语义）与非网络错误（协议解析）。
func connBroken(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	return errors.As(err, &ne) // 非超时网络错误（如 ECONNRESET）
}

// readSignalMessage 读一条信令消息（Status-Line + 头 + 可选 Content-Length 体）。
// 前置约定：调用前已确认流首不是 '$'（媒体帧已消化）。
func (c *Client) readSignalMessage() (int, map[string]string, []byte, error) {
	line, err := readLine(c.br)
	if err != nil {
		return 0, nil, nil, err
	}
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "RTSP/") {
		return 0, nil, nil, fmt.Errorf("%w: %q", ErrBadStatusLine, line)
	}
	code, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%w: 状态码 %q", ErrBadResponse, parts[1])
	}
	headers := map[string]string{}
	for {
		line, err = readLine(c.br)
		if err != nil {
			return 0, nil, nil, err
		}
		if line == "" {
			break
		}
		if ci := strings.Index(line, ":"); ci > 0 {
			headers[strings.ToLower(strings.TrimSpace(line[:ci]))] = strings.TrimSpace(line[ci+1:])
		}
	}
	var body []byte
	if cl := headers["content-length"]; cl != "" {
		n, cerr := strconv.Atoi(cl)
		if cerr != nil || n < 0 || n > 1<<20 {
			return 0, nil, nil, fmt.Errorf("%w: Content-Length 非法 %q", ErrBadResponse, cl)
		}
		body = make([]byte, n)
		if _, err := io.ReadFull(c.br, body); err != nil {
			return 0, nil, nil, fmt.Errorf("rtspclient: 读响应体失败: %w", err)
		}
	}
	return code, headers, body, nil
}

// readLine 读一行（\r\n / \n 结尾）。
func readLine(br *bufio.Reader) (string, error) {
	s, err := br.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(s, "\r\n"), nil
}

// basicAuth 计算 Basic 凭据。
func basicAuth(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

// parseRTSPURL 解析 rtsp://[user:pass@]host[:port][/path]（无端口默认 554）。
func parseRTSPURL(raw string) (user, pass, hostport, path string) {
	s := strings.TrimPrefix(raw, "rtsp://")
	if at := strings.Index(s, "@"); at >= 0 {
		cred := s[:at]
		s = s[at+1:]
		if cp := strings.Index(cred, ":"); cp >= 0 {
			user, pass = cred[:cp], cred[cp+1:]
		} else {
			user = cred
		}
	}
	slash := strings.Index(s, "/")
	if slash < 0 {
		return user, pass, s, "/"
	}
	return user, pass, s[:slash], s[slash:]
}

// parseInterleavedChannel 从 Transport 头提取 video interleaved channel
// （interleaved=xx-yy → xx；缺省 0）。
func parseInterleavedChannel(transport string) int {
	for _, seg := range strings.Split(transport, ";") {
		seg = strings.TrimSpace(seg)
		if strings.HasPrefix(seg, "interleaved=") {
			v := strings.TrimPrefix(seg, "interleaved=")
			if ci := strings.Index(v, "-"); ci > 0 {
				if n, err := strconv.Atoi(v[:ci]); err == nil {
					return n
				}
			}
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
		}
	}
	return 0
}

// parseSDPControl 从 SDP 文本提取控制路径（会话级 a=control 优先，轨级
// a=control 首个次之）；相对路径按 RFC 2326 拼接基准。
func parseSDPControl(sdp, baseURL string) string {
	var sessionCtrl, trackCtrl string
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if !strings.HasPrefix(line, "a=control:") {
			continue
		}
		v := strings.TrimPrefix(line, "a=control:")
		if v == "*" {
			continue
		}
		if strings.Contains(line, "m=video") || trackCtrl == "" {
			if trackCtrl == "" {
				trackCtrl = v
			}
		}
		if sessionCtrl == "" && !strings.Contains(v, "trackID") && !strings.Contains(v, "streamID") {
			sessionCtrl = v
		}
	}
	// 优先会话级（* 之外的绝对/相对路径）；否则首个轨级。
	ctrl := sessionCtrl
	if ctrl == "" {
		ctrl = trackCtrl
	}
	if ctrl == "" {
		return ""
	}
	if strings.HasPrefix(ctrl, "rtsp://") {
		return ctrl
	}
	base := strings.TrimSuffix(baseURL, "/")
	if strings.HasPrefix(ctrl, "/") {
		if u := strings.SplitN(strings.TrimPrefix(base, "rtsp://"), "/", 2); len(u) == 2 {
			return "rtsp://" + u[0] + ctrl
		}
		return base + ctrl
	}
	return base + "/" + strings.TrimPrefix(ctrl, "/")
}
