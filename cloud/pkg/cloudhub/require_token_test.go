package cloudhub

import (
	"testing"

	"edgeflow/pkg/protocol"
)

// v0.21.0（SEC-02）：requireNodeToken 空令牌强制拒绝开关。
// 语义边界：
//   - nodeToken 非空 → 既有校验路径完全不受影响（WithRequireNodeToken 无额外效果）；
//   - nodeToken 为空 + enforce 关（默认）→ 向后兼容，携带令牌的注册照常接受（v0.20.0 行为）；
//   - nodeToken 为空 + enforce 开 → 携带令牌的注册一律拒绝（accepted=false），
//     未携带令牌的注册仍按裸奔兼容模式接受（不校验——完全关闭接入面需配合
//     nodeToken 或 mTLS，属部署决策，见 CHN-06 启动告警）。
func TestRequireNodeTokenEnforce(t *testing.T) {
	register := func(nodeID, token string) *protocol.Message {
		m, _ := protocol.NewMessage(protocol.TypeRegister, nodeID, "cloud", RegisterPayload{
			NodeID: nodeID, Arch: "arm64", OS: "linux", EdgecoreVersion: "v0.21.0",
			CPU: 2, Memory: 1 << 30, Token: token,
		})
		return m
	}
	ackOf := func(t *testing.T, srv *Server, url, nodeID, token string) RegisterAckPayload {
		t.Helper()
		ws := dial(t, url)
		defer ws.Close()
		sendMsg(t, ws, register(nodeID, token))
		ack := readMsg(t, ws)
		var p RegisterAckPayload
		if err := ack.DecodePayload(&p); err != nil {
			t.Fatalf("解析 RegisterAck payload 失败: %v", err)
		}
		return p
	}

	t.Run("默认关闭_空token服务端_携带令牌注册仍接受_兼容v0200", func(t *testing.T) {
		_, url := newTestServer(t) // 无 nodeToken、无 enforce（v0.20.0 缺省形态）
		p := ackOf(t, nil, url, "edge-enf-0", "any-token")
		if !p.Accepted {
			t.Errorf("默认关闭 enforce 时不应拒绝携带令牌的注册: %s", p.Message)
		}
	})

	t.Run("enforce开启_空token服务端_携带令牌注册被拒绝", func(t *testing.T) {
		srv, url := newTestServer(t, WithRequireNodeToken(true))
		p := ackOf(t, srv, url, "edge-enf-1", "forged-token")
		if p.Accepted {
			t.Errorf("enforce 开启后携带令牌的注册必须被拒绝")
		}
		if got := srv.NodeCount(); got != 0 {
			t.Errorf("拒绝后 NodeCount = %d，期望 0", got)
		}
	})

	t.Run("enforce开启_空token服务端_无令牌注册仍接受_裸奔兼容模式不受影响", func(t *testing.T) {
		_, url := newTestServer(t, WithRequireNodeToken(true))
		p := ackOf(t, nil, url, "edge-enf-2", "")
		if !p.Accepted {
			t.Errorf("enforce 只针对携带令牌的注册，无令牌注册不应被本开关拒绝: %s", p.Message)
		}
	})

	t.Run("enforce开启_nodeToken非空_正确令牌不受影响", func(t *testing.T) {
		_, url := newTestServer(t, WithNodeToken("s3cret"), WithRequireNodeToken(true))
		p := ackOf(t, nil, url, "edge-enf-3", "s3cret")
		if !p.Accepted {
			t.Errorf("nodeToken 非空时 enforce 无额外效果，正确 token 应通过: %s", p.Message)
		}
	})
}
