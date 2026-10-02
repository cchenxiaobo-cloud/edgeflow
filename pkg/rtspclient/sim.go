// 模拟 RTSP 服务端（v0.42.0，spec 0015 测试面——导出面供 mapper 级测试
// 与 e2e 复用）：信令应答 + interleaved RTP 下发 + 合成 H.264（STAP-A
// SPS/PPS + FU-A 分片 IDR）+ 401 认证挑战 + 会话断流控制。
// 注意：测试辅助组件，非产品交付物——登记 spec 0015 边界。
package rtspclient

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SimConfig 是模拟服务端配置。
type SimConfig struct {
	RequireAuth   bool
	StreamGap     time.Duration // 每 RTP 包下发间隔（默认 50ms）
	ResetAfterDur time.Duration // 推流该时长后强制断开（自愈测试；0 = 不断）
	User, Pass    string
}

// SimServer 是模拟 RTSP 服务端。
type SimServer struct {
	ln    net.Listener
	cfg   SimConfig
	mu    sync.Mutex
	conns []net.Conn
}

// NewSimServer 启动模拟服务端（监听 127.0.0.1:0）。
func NewSimServer(cfg SimConfig) (*SimServer, string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", fmt.Errorf("rtspclient: 模拟服务端监听失败: %w", err)
	}
	s := &SimServer{ln: ln, cfg: cfg}
	go s.acceptLoop()
	return s, ln.Addr().String(), nil
}

// Close 关闭监听与全部连接。
func (s *SimServer) Close() {
	_ = s.ln.Close()
	s.mu.Lock()
	for _, c := range s.conns {
		_ = c.Close()
	}
	s.conns = nil
	s.mu.Unlock()
}

func (s *SimServer) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, conn)
		s.mu.Unlock()
		go s.serve(conn)
	}
}

// serve 处理一条连接的信令循环 + PLAY 后推流。
func (s *SimServer) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)
	session := "sim-session-1"
	var streamStop chan struct{}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		method := requestMethod(line)
		headers := readHeaders(br)
		if s.cfg.RequireAuth && method == "DESCRIBE" {
			if headers["authorization"] == "" {
				s.writeMsg(conn, "RTSP/1.0 401 Unauthorized", map[string]string{
					"WWW-Authenticate": `Basic realm="edgeflow-sim"`,
					"CSeq":             headers["cseq"],
				}, nil)
				continue
			}
			if !checkBasic(headers["authorization"], s.cfg.User, s.cfg.Pass) {
				s.writeMsg(conn, "RTSP/1.0 401 Unauthorized", map[string]string{"CSeq": headers["cseq"]}, nil)
				continue
			}
		}
		switch method {
		case "OPTIONS":
			s.writeMsg(conn, "RTSP/1.0 200 OK", map[string]string{
				"Server": "EdgeFlowSim/0.42",
				"Public": "OPTIONS, DESCRIBE, SETUP, PLAY, TEARDOWN",
				"CSeq":   headers["cseq"],
			}, nil)
		case "DESCRIBE":
			sdp := "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=EdgeFlowSim\r\n" +
				"m=video 0 RTP/AVP 96\r\na=control:trackID=0\r\n" +
				"a=rtpmap:96 H264/90000\r\n"
			s.writeMsg(conn, "RTSP/1.0 200 OK", map[string]string{
				"Content-Type":   "application/sdp",
				"Content-Length": strconv.Itoa(len(sdp)),
				"CSeq":           headers["cseq"],
			}, []byte(sdp))
		case "SETUP":
			s.writeMsg(conn, "RTSP/1.0 200 OK", map[string]string{
				"Session":   session + ";timeout=60",
				"Transport": "RTP/AVP/TCP;interleaved=0-1",
				"CSeq":      headers["cseq"],
			}, nil)
		case "PLAY":
			s.writeMsg(conn, "RTSP/1.0 200 OK", map[string]string{"Session": session, "CSeq": headers["cseq"]}, nil)
			stop := make(chan struct{})
			streamStop = stop
			go s.stream(conn, stop, s.cfg.ResetAfterDur)
		case "TEARDOWN":
			if streamStop != nil {
				close(streamStop)
			}
			s.writeMsg(conn, "RTSP/1.0 200 OK", map[string]string{"Session": session, "CSeq": headers["cseq"]}, nil)
			return
		default:
			s.writeMsg(conn, "RTSP/1.0 405 Method Not Allowed", map[string]string{"CSeq": headers["cseq"]}, nil)
		}
	}
}

// stream 向连接下发 interleaved RTP 视频流。
func (s *SimServer) stream(conn net.Conn, stop chan struct{}, resetAfter time.Duration) {
	gap := s.cfg.StreamGap
	if gap <= 0 {
		gap = 50 * time.Millisecond
	}
	var resetC <-chan time.Time
	if resetAfter > 0 {
		t := time.NewTimer(resetAfter)
		defer t.Stop()
		resetC = t.C
	}
	tk := time.NewTicker(gap)
	defer tk.Stop()
	frame := 0
	for {
		select {
		case <-stop:
			return
		case <-resetC:
			_ = conn.Close()
			return
		case <-tk.C:
		}
		seq := uint16(frame*3 + 1)
		ts := uint32(frame) * 3000
		if err := writeInterleaved(conn, 0, buildRTP(96, seq, ts, stapA(sps, pps))); err != nil {
			return
		}
		for _, pkt := range fuaPackets(buildFakeIDR(1200), 700) {
			seq++
			if err := writeInterleaved(conn, 0, buildRTP(96, seq, ts, pkt)); err != nil {
				return
			}
		}
		frame++
	}
}

// writeMsg 写一条信令响应。
func (s *SimServer) writeMsg(conn net.Conn, status string, headers map[string]string, body []byte) {
	b := status + "\r\n"
	for k, v := range headers {
		b += k + ": " + v + "\r\n"
	}
	if body != nil {
		b += fmt.Sprintf("Content-Length: %d\r\n", len(body))
	}
	b += "\r\n"
	_, _ = conn.Write([]byte(b))
	if body != nil {
		_, _ = conn.Write(body)
	}
}

// requestMethod 解析请求行方法名。
func requestMethod(line string) string {
	if i := strings.Index(line, " "); i > 0 {
		return line[:i]
	}
	return line
}

// readHeaders 读头部至空行。
func readHeaders(br *bufio.Reader) map[string]string {
	h := map[string]string{}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return h
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			return h
		}
		if i := strings.Index(line, ":"); i > 0 {
			h[strings.ToLower(strings.TrimSpace(line[:i]))] = strings.TrimSpace(line[i+1:])
		}
	}
}

// checkBasic 校验 Authorization: Basic 凭据。
func checkBasic(header, user, pass string) bool {
	const prefix = "Basic "
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return false
	}
	dec, err := base64.StdEncoding.DecodeString(header[len(prefix):])
	return err == nil && string(dec) == user+":"+pass
}

var (
	sps = []byte{0x67, 0x42, 0xC0, 0x1E, 0xAB, 0x40, 0xA0, 0xFD, 0xFF, 0xE8}
	pps = []byte{0x68, 0xCB, 0x83, 0xCB, 0x20}
)

// buildFakeIDR 构造合成 IDR NAL（type 5，确定性载荷；验证到 AnnexB 重组口径，
// 解码由外部 ffmpeg 承担、不进测试面）。
func buildFakeIDR(n int) []byte {
	nal := make([]byte, n)
	nal[0] = 0x65
	for i := 1; i < n; i++ {
		nal[i] = byte(i*31 + 7)
	}
	for i := 1; i < n-2; i++ {
		if nal[i] == 0 && nal[i+1] == 0 && nal[i+2] <= 3 {
			nal[i+2] = 0xF0
		}
	}
	return nal
}

// stapA 组 STAP-A 载荷。
func stapA(nals ...[]byte) []byte {
	p := []byte{0x78}
	for _, n := range nals {
		var sz [2]byte
		binary.BigEndian.PutUint16(sz[:], uint16(len(n)))
		p = append(p, sz[:]...)
		p = append(p, n...)
	}
	return p
}

// fuaPackets 分片 FU-A 载荷序列。
func fuaPackets(nal []byte, maxFrag int) [][]byte {
	indicator := byte(0x7C)
	typ := nal[0] & 0x1F
	body := nal[1:]
	if len(body) <= maxFrag {
		return [][]byte{nal}
	}
	var out [][]byte
	off := 0
	for off < len(body) {
		end := off + maxFrag
		last := end >= len(body)
		if last {
			end = len(body)
		}
		var fh byte
		switch {
		case off == 0:
			fh = 0x80 | typ
		case last:
			fh = 0x40 | typ
		default:
			fh = typ
		}
		p := []byte{indicator, fh}
		p = append(p, body[off:end]...)
		out = append(out, p)
		off = end
	}
	return out
}

// buildRTP 组一个 RTP 包。
func buildRTP(pt byte, seq uint16, ts uint32, payload []byte) []byte {
	pkt := make([]byte, 12, 12+len(payload))
	pkt[0] = 0x80
	pkt[1] = pt & 0x7F
	binary.BigEndian.PutUint16(pkt[2:4], seq)
	binary.BigEndian.PutUint32(pkt[4:8], ts)
	binary.BigEndian.PutUint32(pkt[8:12], 0xDEADBEEF)
	return append(pkt, payload...)
}

// writeInterleaved 写一帧 interleaved 数据。
func writeInterleaved(conn net.Conn, ch byte, rtp []byte) error {
	head := []byte{'$', ch, byte(len(rtp) >> 8), byte(len(rtp))}
	if _, err := conn.Write(head); err != nil {
		return err
	}
	_, err := conn.Write(rtp)
	return err
}
