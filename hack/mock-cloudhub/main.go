// mock-cloudhub 是 EdgeHub 联调/冒烟用的本地模拟 CloudHub 服务端。
//
// 用法：
//
//	go run ./hack/mock-cloudhub            # 默认监听 :10000
//	go run ./hack/mock-cloudhub -port 10000
//
// 行为：监听 /v1/edge，自动应答 Register（accepted=true）与 Heartbeat
// （nodeStatus=Ready），并把收到的消息打印到 stdout，便于人工核对
// 注册/心跳/重连是否按契约工作。
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"

	"github.com/gorilla/websocket"
)

const channelPath = "/v1/edge"

func main() {
	port := flag.Int("port", 10000, "监听端口")
	flag.Parse()

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	http.HandleFunc(channelPath, func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Errorf("WebSocket 升级失败: %v", err)
			return
		}
		log.Infof("边缘节点已连接: %s", r.RemoteAddr)
		defer func() {
			_ = conn.Close()
			log.Warnf("连接断开: %s", r.RemoteAddr)
		}()

		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			msg, err := protocol.Decode(data)
			if err != nil {
				log.Errorf("无法解析消息: %v", err)
				continue
			}
			_, _ = fmt.Printf("[mock-cloudhub] 收到 %s 来自 %s: %s\n", msg.Type, msg.Source, string(msg.Payload))
			switch msg.Type {
			case protocol.TypeRegister:
				ack, _ := protocol.NewMessage(protocol.TypeRegisterAck, "cloud", msg.Source,
					map[string]any{"accepted": true, "nodeName": "mock-" + msg.Source, "message": "ok"})
				if err := send(conn, ack); err != nil {
					return
				}
			case protocol.TypeHeartbeat:
				ack, _ := protocol.NewMessage(protocol.TypeHeartbeatAck, "cloud", msg.Source,
					map[string]any{"nodeStatus": "Ready"})
				if err := send(conn, ack); err != nil {
					return
				}
			}
		}
	})

	addr := fmt.Sprintf(":%d", *port)
	log.Infof("mock-cloudhub 监听 %s（通道 %s）", addr, channelPath)
	srv := &http.Server{Addr: addr}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Errorf("HTTP 服务异常退出: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	_ = srv.Close()
	log.Infof("mock-cloudhub exited")
}

// send 以文本帧发送一条消息。
func send(conn *websocket.Conn, msg *protocol.Message) error {
	data, err := protocol.Encode(msg)
	if err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, data)
}
