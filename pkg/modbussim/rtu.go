// RTU-over-TCP 模拟模式（v0.41.0，spec 0014 US-1）。
//
// 与 MBAP 模式（sim.go）共享同一寄存器/线圈模型与 handlePDU 处理器，
// 差异仅在帧封装：RTU 帧 = `[从站地址(1)][PDU][CRC16(2,LE)]`（无 MBAP 头）。
// 用途：mappers/modbus 的 rtutcp:// 路径联调（RTU 协议实现验证）——
// 覆盖「串口-网关」主流部署形态（网关把串口 RTU 帧透传为 TCP 字节流）；
// 真串口 transport 登记为后续候选（串口硬件依赖，KI §42）。
package modbussim

import (
	"errors"
	"fmt"
	"net"
	"time"

	"edgeflow/pkg/modbusrtu"
)

// StartRTU 启动 RTU-over-TCP 监听（须在 Start 之后调用——共享寄存器模型与
// 波动 goroutine）。返回实际监听地址（测试传 "127.0.0.1:0"）。随 Stop 统一关闭。
func (s *Simulator) StartRTU(listenAddr string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return "", errors.New("modbus 模拟器未启动（先 Start）")
	}
	if s.rtuLn != nil {
		return "", errors.New("modbus RTU 监听已启动")
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return "", fmt.Errorf("modbus RTU 模拟器监听 %s 失败: %w", listenAddr, err)
	}
	s.rtuLn = ln
	s.wg.Add(1)
	go s.rtuAcceptLoop(ln)
	return ln.Addr().String(), nil
}

// rtuAcceptLoop 接受 RTU 连接（并发上限与连接登记与 MBAP 路径同语义）。
func (s *Simulator) rtuAcceptLoop(ln net.Listener) {
	defer s.wg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return // 监听已关闭
		}
		s.mu.Lock()
		if len(s.conns) >= s.maxConns {
			s.mu.Unlock()
			_ = conn.Close() // 超上限拒绝（与 MBAP 路径同语义）
			continue
		}
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go s.rtuHandleConn(conn)
	}
}

// rtuHandleConn 处理一条 RTU 连接：读请求帧（CRC/长度校验）→ 从站校验 →
// 复用 handlePDU → RTU 帧应答。CRC 不符/非法帧：RTU 无帧边界重同步语义，
// 直接断开连接（与主流网关行为一致）；从站越界按 MBAP 路径同语义回
// 异常码 0x0B（网关目标设备无响应）。
func (s *Simulator) rtuHandleConn(conn net.Conn) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		_ = conn.Close()
	}()
	for {
		if err := conn.SetReadDeadline(time.Now().Add(connIdleTimeout)); err != nil {
			return
		}
		frame, err := modbusrtu.ReadRequestFrame(conn)
		if err != nil {
			return // 超时/对端关闭/非法功能码：断开（静默，与 MBAP 路径一致）
		}
		addr, pdu, perr := modbusrtu.ParseFrame(frame)
		if perr != nil {
			return // CRC 不符/帧残缺：无法重同步，断开
		}
		fc := pdu[0]
		var respData []byte
		var exc byte
		if addr < unitIDMin || addr > unitIDMax {
			exc = excGatewayTarget
		} else {
			respData, exc = s.handlePDU(fc, pdu[1:])
		}
		var respPDU []byte
		if exc != 0 {
			respPDU = []byte{fc | 0x80, exc}
		} else {
			respPDU = make([]byte, 0, 1+len(respData))
			respPDU = append(respPDU, fc)
			respPDU = append(respPDU, respData...)
		}
		if _, err := conn.Write(modbusrtu.AppendFrame(addr, respPDU)); err != nil {
			return
		}
	}
}
