// v0590_test.go：转发出口增强单测（v0.59.0，spec 0032）——测试投递 + mqtt sink。
package forwardstore

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"edgeflow/pkg/mqtt"
	"edgeflow/pkg/mqttsim"
)

// US-1 测试投递（webhook）：同步返回 delivered；payload event=test；台账含 test 条目。
func TestV0590TestDeliveryWebhook(t *testing.T) {
	var mu sync.Mutex
	var gotBody []byte
	rcv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = body
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer rcv.Close()

	s := New("")
	r, err := s.Create("td", "", rcv.URL, "", "", "", "", false, []string{"alarm.raised"}, true, 1000)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.TestDelivery(r.ID, 2000)
	if err != nil {
		t.Fatalf("TestDelivery: %v", err)
	}
	if d.Status != "delivered" || d.Event != "test" || d.Attempts != 1 {
		t.Fatalf("测试投递台账不符: %+v", d)
	}
	mu.Lock()
	defer mu.Unlock()
	var p payload
	if err := json.Unmarshal(gotBody, &p); err != nil {
		t.Fatalf("payload 解析: %v", err)
	}
	if p.Event != "test" || p.AlarmID != "test-"+r.ID {
		t.Fatalf("测试 payload 不符: %+v", p)
	}
	list := s.Deliveries(r.ID, 10)
	if len(list) != 1 || list[0].Event != "test" || list[0].Status != "delivered" {
		t.Fatalf("台账应含 test 条目: %+v", list)
	}
}

// US-2 禁用规则测试投递 → ErrInvalid（启用后再测）；未知规则 → ErrNotFound。
func TestV0590TestDeliveryGuards(t *testing.T) {
	s := New("")
	r, err := s.Create("off", "", "http://a.com/h", "", "", "", "", false, []string{"alarm.raised"}, false, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TestDelivery(r.ID, 2000); err == nil {
		t.Fatalf("禁用规则应拒绝测试投递")
	}
	if _, err := s.TestDelivery("fw-nope", 2000); err != ErrNotFound {
		t.Fatalf("未知规则应 ErrNotFound: %v", err)
	}
}

// US-3 mqtt sink：mqttsim broker + pkg/mqtt 订阅者 QoS1 → Dispatch 到达订阅者
// （QoS1 帧，topic 正确），台账 delivered（mqtt 无 respCode）。
func TestV0590MqttSinkDispatch(t *testing.T) {
	b, err := mqttsim.NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	type recv struct {
		topic   string
		payload []byte
	}
	ch := make(chan recv, 4)
	sub, err := mqtt.Dial(b.Addr(), mqtt.Options{ClientID: "fw-mqtt-sub", ProtocolVersion5: true})
	if err != nil {
		t.Fatalf("subscriber dial: %v", err)
	}
	defer sub.Close()
	if err := sub.Subscribe("t/fw", 1, func(topic string, payload []byte) {
		ch <- recv{topic: topic, payload: payload}
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	s := New("")
	r, err := s.Create("mqtt-sink", "mqtt", b.Addr(), "t/fw", "", "", "", false, []string{"alarm.raised"}, true, 1000)
	if err != nil {
		t.Fatal(err)
	}
	s.Dispatch("alarm.raised", sampleAlarm("alm-mqtt"))

	deadline := time.Now().Add(5 * time.Second)
	var got recv
	for {
		select {
		case got = <-ch:
			goto got
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("5s 未收到 mqtt sink 投递")
		}
		time.Sleep(100 * time.Millisecond)
	}
got:
	if got.topic != "t/fw" {
		t.Fatalf("mqtt 下行主题不符: %s", got.topic)
	}
	var p payload
	if err := json.Unmarshal(got.payload, &p); err != nil {
		t.Fatalf("mqtt payload 解析: %v", err)
	}
	if p.Event != "alarm.raised" || p.AlarmID != "alm-mqtt" {
		t.Fatalf("mqtt payload 不符: %+v", p)
	}
	deliveries := waitDelivery(t, s, r.ID, func(d []Delivery) bool {
		return len(d) == 1 && d[0].Status == "delivered"
	}, 5*time.Second)
	if deliveries[0].RespCode != 0 {
		t.Fatalf("mqtt 台账不应有 HTTP 状态码: %+v", deliveries[0])
	}
}

// US-4 sinkType 校验：mqtt 无端口/含路径/非法形态拒绝；webhook 语义不变。
func TestV0590SinkValidation(t *testing.T) {
	s := New("")
	if _, err := s.Create("m1", "mqtt", "127.0.0.1", "", "", "", "", false, []string{"alarm.raised"}, true, 1000); err == nil {
		t.Fatalf("mqtt 无端口应拒绝")
	}
	if _, err := s.Create("m2", "mqtt", "tcp://127.0.0.1:1883/x", "", "", "", "", false, []string{"alarm.raised"}, true, 1000); err == nil {
		t.Fatalf("mqtt 含路径应拒绝")
	}
	if _, err := s.Create("m3", "kafka", "http://a.com", "", "", "", "", false, []string{"alarm.raised"}, true, 1000); err == nil {
		t.Fatalf("非法 sinkType 应拒绝")
	}
	if _, err := s.Create("w1", "mqtt", "tcp://127.0.0.1:1883", "edgeflow/fw", "", "", "", false, []string{"alarm.raised"}, true, 1000); err != nil {
		t.Fatalf("合法 mqtt 创建失败: %v", err)
	}
}
