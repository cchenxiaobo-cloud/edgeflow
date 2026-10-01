// rest_mapper_test.go：REST 轮询采集器单测（spec 0014 US-2）。
package rest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"edgeflow/edge/pkg/mapper"
)

// newTestMapper 构造指向 httptest 服务端的 Mapper。
func newTestMapper(t *testing.T, handler http.HandlerFunc) (*RESTMapper, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	m := New(srv.URL, WithDeviceName("rest-test"), WithNamespace("plant-b"), WithTimeout(2*time.Second))
	return m, srv
}

// TestRESTCollectSuccess 覆盖正常采集路径（values 全量映射 + 设备声明）。
func TestRESTCollectSuccess(t *testing.T) {
	m, _ := newTestMapper(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"values":{"temperature":25.5,"humidity":60,"pressure":1013}}`))
	})
	props, err := m.Collect()
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	if len(props) != 3 || props["temperature"] != 25.5 || props["pressure"] != 1013 {
		t.Fatalf("属性映射不符: %v", props)
	}
	if m.DeviceNames()[0] != "rest-test" || m.DeviceNamespace() != "plant-b" {
		t.Fatalf("设备声明不符: %v / %s", m.DeviceNames(), m.DeviceNamespace())
	}
	if m.Name() != DefaultName {
		t.Fatalf("注册名不符: %s", m.Name())
	}
}

// TestRESTCollectErrors 覆盖失败路径：非 200 / 坏 JSON / 空 values。
func TestRESTCollectErrors(t *testing.T) {
	cases := []struct {
		name string
		code int
		body string
	}{
		{"非 200", http.StatusInternalServerError, `{"values":{"a":1}}`},
		{"坏 JSON", http.StatusOK, `{not-json`},
		{"空 values", http.StatusOK, `{"values":{}}`},
		{"缺 values", http.StatusOK, `{"other":1}`},
	}
	for _, tc := range cases {
		m, _ := newTestMapper(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.code)
			_, _ = w.Write([]byte(tc.body))
		})
		if _, err := m.Collect(); err == nil {
			t.Errorf("%s：应返回错误", tc.name)
		}
	}
}

// TestRESTCollectTimeout 覆盖超时路径（服务端阻塞 > 客户端超时）。
func TestRESTCollectTimeout(t *testing.T) {
	m := New("http://127.0.0.1:1/values", WithTimeout(300*time.Millisecond)) // 不可达端口
	if _, err := m.Collect(); err == nil {
		t.Fatalf("不可达端点应返回错误")
	}
}

// TestRESTHandleCommandUnsupported 覆盖只读语义（明确报错）。
func TestRESTHandleCommandUnsupported(t *testing.T) {
	m, _ := newTestMapper(t, func(w http.ResponseWriter, r *http.Request) {})
	if _, err := m.HandleCommand(mapper.DeviceCommand{
		DeviceName: "rest-test", Property: "targetTemp", Value: 30,
	}); err == nil {
		t.Fatalf("只读采集器应拒绝指令")
	}
}

// TestRESTStartStop 覆盖生命周期幂等。
func TestRESTStartStop(t *testing.T) {
	m, _ := newTestMapper(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatalf("重复启动应幂等: %v", err)
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("停止失败: %v", err)
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("重复停止应幂等: %v", err)
	}
}

// TestRESTEnvConfig 覆盖 env 解析路径（复核 P2-9）：URL/设备名/命名空间/超时
// 的回退链与选项优先级（option > env > default）。
func TestRESTEnvConfig(t *testing.T) {
	t.Setenv(EnvURL, "http://127.0.0.1:1/values")
	t.Setenv(EnvDevice, "rest-env-01")
	t.Setenv(EnvNamespace, "plant-c")
	t.Setenv(EnvTimeout, "7")

	m := New("") // url 空 → env 兜底
	if m.Addr() != "http://127.0.0.1:1/values" {
		t.Fatalf("URL env 兜底不符: %s", m.Addr())
	}
	if m.DeviceNames()[0] != "rest-env-01" {
		t.Fatalf("设备名 env 未生效: %v", m.DeviceNames())
	}
	if m.DeviceNamespace() != "plant-c" {
		t.Fatalf("命名空间 env 未生效: %s", m.DeviceNamespace())
	}
	if m.timeout != 7*time.Second {
		t.Fatalf("超时 env 未生效: %v", m.timeout)
	}

	// 选项优先级高于 env。
	m2 := New("", WithDeviceName("opt-x"), WithNamespace("opt-ns"), WithTimeout(time.Second))
	if m2.DeviceNames()[0] != "opt-x" || m2.DeviceNamespace() != "opt-ns" || m2.timeout != time.Second {
		t.Fatalf("选项应覆盖 env: %v / %s / %v", m2.DeviceNames(), m2.DeviceNamespace(), m2.timeout)
	}

	// 非法超时回退默认。
	t.Setenv(EnvTimeout, "0")
	m3 := New("")
	if m3.timeout != DefaultTimeout {
		t.Fatalf("非法超时应回退默认: %v", m3.timeout)
	}
}
