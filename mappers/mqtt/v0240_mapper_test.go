package mqtt

// v0.24.0 MQTT Mapper 单元测试（N-R2 接力交付）。
//
// 策略：全部经 pkg/mqttsim 起真实 broker（127.0.0.1:0），Mapper 走真实
// pkg/mqtt 协议栈（Dial/Subscribe/Publish/读泵分发），不做协议 mock；
// 断言一律以 mqtt_mapper.go 实际实现为准：
//   - CmdTopicFromFilter 取字面段拼 "/cmd"（"devices/+/state" →
//     "devices/state/cmd"）；
//   - parsePayload 双格式容错：JSON 对象顶层可数值化字段（含字符串数字）
//     与 "k=v" 文本均接受，解析不出任何属性才整条跳过；
//   - Start/Stop 幂等，Stop 清空快照，supervise 随 ctx 取消退出。
//
// 真实断线重连不在单测覆盖：sim broker 端口随机无法原址替换，且
// Probe/Reconnect 间隔 2s 会拖慢门禁；重连循环的退出骨架（ctx 取消、
// 连接换出、goroutine 回收）由 TestSuperviseCtxCancelExit 覆盖。

import (
	"context"
	"encoding/json"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"edgeflow/edge/pkg/mapper"
	"edgeflow/edge/pkg/metamanager"
	"edgeflow/pkg/mqttsim"
)

// ---- 测试基建 ----

// fakeLedger 台账桩：记录全部 SaveOp 调用供断言（OpLedger 最小实现）。
type fakeLedger struct {
	mu   sync.Mutex
	recs []metamanager.OpRecord
}

func (f *fakeLedger) SaveOp(rec metamanager.OpRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recs = append(f.recs, rec)
	return nil
}

// all 返回台账副本。
func (f *fakeLedger) all() []metamanager.OpRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]metamanager.OpRecord(nil), f.recs...)
}

// count 统计指定方向的台账条数。
func (f *fakeLedger) count(direction string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.recs {
		if r.Direction == direction {
			n++
		}
	}
	return n
}

// waitFor 轮询直至 cond 为真（≤2s），超时 Fatal。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

// startWithSim 起 sim broker、清空相关环境变量（hermetic）、Start Mapper
// 并等待连接建立。Cleanup 顺序（LIFO）：先 Stop Mapper 再关 broker。
func startWithSim(t *testing.T, opts ...Option) (*mqttsim.Broker, *MQTTMapper) {
	t.Helper()
	for _, k := range []string{EnvTopics, EnvCmdTopic, EnvDeviceName, EnvNamespace} {
		t.Setenv(k, "")
	}
	sim, err := mqttsim.NewBroker()
	if err != nil {
		t.Fatalf("起 sim broker 失败: %v", err)
	}
	t.Cleanup(func() { _ = sim.Close() })
	m := New(sim.Addr(), opts...)
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop() })
	waitFor(t, "mapper 连上 broker", func() bool { return m.clientSnapshot() != nil })
	return sim, m
}

// mustPublish 经 sim broker 发布一条设备上报。
func mustPublish(t *testing.T, sim *mqttsim.Broker, topic, payload string) {
	t.Helper()
	if err := sim.Publish(topic, []byte(payload)); err != nil {
		t.Fatalf("sim.Publish(%s) 失败: %v", topic, err)
	}
}

// ---- 纯函数：主题解析与命令主题推导 ----

func TestParseTopics(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"空串返回nil", "", nil},
		{"纯空白返回nil", "   ", nil},
		{"全逗号返回nil", ",,,", nil},
		{"单值", "a/b", []string{"a/b"}},
		{"两值", "a/+,b/#", []string{"a/+", "b/#"}},
		{"混合空格与空条目", " a , b ,, c ", []string{"a", "b", "c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseTopics(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("ParseTopics(%q) = %v, 期望 %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("ParseTopics(%q) = %v, 期望 %v", tc.in, got, tc.want)
				}
			}
		})
	}
}

func TestCmdTopicFromFilter(t *testing.T) {
	cases := []struct {
		filter string
		want   string
	}{
		{"devices/+/state", "devices/state/cmd"},
		{"edgeflow/#", "edgeflow/cmd"},
		{"a/b", "a/b/cmd"},
		{" demo / + / state ", "demo/state/cmd"},
		{"+", DefaultCmdTopic},
		{"#", DefaultCmdTopic},
		{"", DefaultCmdTopic},
	}
	for _, tc := range cases {
		if got := CmdTopicFromFilter(tc.filter); got != tc.want {
			t.Errorf("CmdTopicFromFilter(%q) = %q, 期望 %q", tc.filter, got, tc.want)
		}
	}
}

// ---- 构造默认值 / 环境变量回退 / 选项副本语义 ----

func TestNewDefaultsAndEnvFallback(t *testing.T) {
	// 清空全部相关环境变量 → 默认值路径
	for _, k := range []string{EnvBroker, EnvTopics, EnvDeviceName, EnvNamespace, EnvCmdTopic} {
		t.Setenv(k, "")
	}
	m := New("")
	if m.Name() != "mqtt" {
		t.Errorf("Name() = %q, 期望 mqtt", m.Name())
	}
	if m.Broker() != "" {
		t.Errorf("Broker() = %q, 期望空", m.Broker())
	}
	if got := m.DeviceNames(); len(got) != 1 || got[0] != DefaultDeviceName {
		t.Errorf("DeviceNames() = %v, 期望 [%s]", got, DefaultDeviceName)
	}
	if m.DeviceNamespace() != DefaultNamespace {
		t.Errorf("DeviceNamespace() = %q, 期望 %q", m.DeviceNamespace(), DefaultNamespace)
	}
	if got := m.Topics(); !reflect.DeepEqual(got, DefaultTopics) {
		t.Errorf("Topics() = %v, 期望 %v", got, DefaultTopics)
	}
	// cmd 主题按首个 filter 推导（字面段拼接），不是 DefaultCmdTopic
	if want := "devices/state/cmd"; m.CommandTopic() != want {
		t.Errorf("CommandTopic() = %q, 期望 %q", m.CommandTopic(), want)
	}

	// env 回退：broker/topics 经环境变量注入（选项未设置时生效）
	t.Setenv(EnvBroker, "127.0.0.1:18883")
	t.Setenv(EnvTopics, "demo/+/state , demo/#")
	m2 := New("")
	if m2.Broker() != "127.0.0.1:18883" {
		t.Errorf("env broker 回退失败: %q", m2.Broker())
	}
	if got := m2.Topics(); !reflect.DeepEqual(got, []string{"demo/+/state", "demo/#"}) {
		t.Errorf("env topics 回退失败: %v", got)
	}
	if want := "demo/state/cmd"; m2.CommandTopic() != want {
		t.Errorf("CommandTopic() = %q, 期望 %q", m2.CommandTopic(), want)
	}
}

func TestOptionsCopySemantics(t *testing.T) {
	ts := []string{"t/+/a", "t2/#"}
	m := New("b0",
		WithBroker("127.0.0.1:19999"),
		WithTopics(ts),
		WithDeviceName("dev-x"),
		WithNamespace("ns-x"),
		WithKeepAlive(7*time.Second),
	)
	if m.Broker() != "127.0.0.1:19999" {
		t.Errorf("WithBroker 未生效: %q", m.Broker())
	}
	if got := m.DeviceNames(); len(got) != 1 || got[0] != "dev-x" {
		t.Errorf("WithDeviceName 未生效: %v", got)
	}
	if m.DeviceNamespace() != "ns-x" {
		t.Errorf("WithNamespace 未生效: %q", m.DeviceNamespace())
	}
	// WithTopics 深拷贝入参：事后改动原切片不影响 Mapper
	ts[0] = "hacked/#"
	if got := m.Topics(); got[0] != "t/+/a" {
		t.Errorf("WithTopics 未拷贝入参: %v", got)
	}
	// Topics() 返回副本：改动返回值不影响 Mapper
	got := m.Topics()
	got[1] = "hacked2/#"
	if m.Topics()[1] != "t2/#" {
		t.Errorf("Topics() 未返回副本: %v", m.Topics())
	}
}

// ---- 端到端：Start → sim 上报 → 快照 → Collect ----

func TestStartCollectSnapshotE2E(t *testing.T) {
	sim, m := startWithSim(t)

	// JSON 数字字段
	mustPublish(t, sim, "devices/dev1/state", `{"temp":21.5,"hum":60}`)
	waitFor(t, "快照更新 temp/hum", func() bool {
		props, _ := m.Collect()
		return props["temp"] == 21.5 && props["hum"] == 60
	})

	// JSON 字符串数字（固件容错路径）
	mustPublish(t, sim, "devices/dev1/state", `{"volt":"3.3"}`)
	waitFor(t, "快照更新 volt", func() bool {
		props, _ := m.Collect()
		return props["volt"] == 3.3
	})

	// "k=v" 文本（容错第二格式，实现支持）
	mustPublish(t, sim, "devices/dev1/state", "bat=88.5")
	waitFor(t, "快照更新 bat", func() bool {
		props, _ := m.Collect()
		return props["bat"] == 88.5
	})

	// JSON 中不可数值化字段跳过、可数值化字段照收
	mustPublish(t, sim, "devices/dev1/state", `{"lux":12,"note":"hi","flag":true,"nested":{"a":1}}`)
	waitFor(t, "快照更新 lux", func() bool {
		props, _ := m.Collect()
		return props["lux"] == 12
	})

	// 坏输入整条跳过：快照不变、不崩（空 payload / 无 k=v 文本 / 坏 JSON / 全不可数值 JSON）
	before, _ := m.Collect()
	for _, bad := range []string{"", "hello world", "{invalid", `{"a":"x"}`, "{}"} {
		mustPublish(t, sim, "devices/dev1/state", bad)
	}
	time.Sleep(300 * time.Millisecond)
	after, _ := m.Collect()
	if !reflect.DeepEqual(after, before) {
		t.Errorf("坏输入污染快照: before=%v after=%v", before, after)
	}
}

// ---- 指令下发链路：HandleCommand → cmdTopic 发布 → 校验路径 ----

func TestHandleCommandPublishAndValidation(t *testing.T) {
	sim, m := startWithSim(t)
	cmdTopic := m.CommandTopic()

	cmd := mapper.DeviceCommand{DeviceName: DefaultDeviceName, Property: "switch", Value: 1}
	rep, err := m.HandleCommand(cmd)
	if err != nil {
		t.Fatalf("HandleCommand 失败: %v", err)
	}
	// DeviceReport 非空（订阅型无同步回读，返回当前快照）
	if rep.DeviceName != DefaultDeviceName || rep.Namespace != DefaultNamespace {
		t.Errorf("DeviceReport 设备/命名空间不符: %+v", rep)
	}
	if rep.ReportedAt <= 0 {
		t.Errorf("DeviceReport.ReportedAt 未填充: %d", rep.ReportedAt)
	}
	if rep.Properties == nil {
		t.Error("DeviceReport.Properties 为 nil")
	}

	// sim.Received 出现 cmdTopic 消息且 payload 与云边契约序列化一致
	want, _ := json.Marshal(cmd)
	waitFor(t, "cmd 主题出现指令消息", func() bool {
		for _, msg := range sim.Received() {
			if msg.Topic == cmdTopic && string(msg.Payload) == string(want) {
				return true
			}
		}
		return false
	})

	// 校验路径：设备名不符 / 缺 property 均报错
	if _, err := m.HandleCommand(mapper.DeviceCommand{DeviceName: "other-dev", Property: "x"}); err == nil {
		t.Error("设备名不符应报错")
	}
	if _, err := m.HandleCommand(mapper.DeviceCommand{DeviceName: DefaultDeviceName}); err == nil {
		t.Error("缺 property 应报错")
	}
}

// ---- 生命周期：Start/Stop 幂等 + goroutine 不泄漏 ----

func TestStartStopIdempotent(t *testing.T) {
	base := runtime.NumGoroutine()
	lg := &fakeLedger{}
	sim, m := startWithSim(t, WithLedger(lg))

	// 二次 Start 幂等：不重建监管循环、不重复订阅（单条上报只产生一条 up 台账）
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("二次 Start 应幂等成功: %v", err)
	}
	mustPublish(t, sim, "devices/d1/state", `{"t":7.25}`)
	waitFor(t, "快照更新 t", func() bool {
		props, _ := m.Collect()
		return props["t"] == 7.25
	})
	if n := lg.count(metamanager.DirUp); n != 1 {
		t.Errorf("二次 Start 后单条上报产生 %d 条 up 台账（期望 1，>1 即重复订阅）", n)
	}

	// Stop 幂等：两次均成功；快照清空；停止后上报不再入快照
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("二次 Stop 应幂等成功: %v", err)
	}
	if props, _ := m.Collect(); len(props) != 0 {
		t.Errorf("Stop 后快照未清空: %v", props)
	}
	mustPublish(t, sim, "devices/d1/state", `{"t":9.9}`)
	time.Sleep(250 * time.Millisecond)
	if props, _ := m.Collect(); len(props) != 0 {
		t.Errorf("Stop 后上报仍入快照: %v", props)
	}

	// goroutine 回收（监管循环 + 读泵退出），容差 2
	waitFor(t, "goroutine 数回落", func() bool { return runtime.NumGoroutine() <= base+2 })
}

// ---- 监管循环：ctx 取消退出（真实重连裁决见报告） ----

func TestSuperviseCtxCancelExit(t *testing.T) {
	base := runtime.NumGoroutine()
	sim, err := mqttsim.NewBroker()
	if err != nil {
		t.Fatalf("起 sim broker 失败: %v", err)
	}
	t.Cleanup(func() { _ = sim.Close() })

	m := New(sim.Addr())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	waitFor(t, "mapper 连上 broker", func() bool { return m.clientSnapshot() != nil })

	// ctx 取消 → supervise 退出（done 关闭）→ 连接换出（client nil）
	cancel()
	select {
	case <-m.done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 supervise 未退出")
	}
	if cl := m.clientSnapshot(); cl != nil {
		t.Error("ctx 取消后 client 未换出")
	}
	waitFor(t, "goroutine 数回落", func() bool { return runtime.NumGoroutine() <= base+2 })
}

// ---- 台账触点：up(ok/error) 与 down(ok) 均落账，校验失败不落账 ----

func TestLedgerTouchpoints(t *testing.T) {
	lg := &fakeLedger{}
	sim, m := startWithSim(t, WithLedger(lg))
	cmdTopic := m.CommandTopic()

	// up-ok：合法 JSON 上报
	mustPublish(t, sim, "devices/d1/state", `{"temp":1.5}`)
	waitFor(t, "up 台账 1 条", func() bool { return lg.count(metamanager.DirUp) == 1 })
	rec := lg.all()[0]
	if rec.Direction != metamanager.DirUp || rec.Result != "ok" ||
		rec.DeviceID != DefaultDeviceName || rec.RegAddr != "devices/d1/state" ||
		rec.Value != "temp=1.5" || rec.Ts <= 0 {
		t.Errorf("up-ok 台账字段不符: %+v", rec)
	}

	// up-error：坏 payload 整条跳过并记 error 台账
	mustPublish(t, sim, "devices/d1/state", "no-kv-token")
	waitFor(t, "up 台账 2 条", func() bool { return lg.count(metamanager.DirUp) == 2 })
	recs := lg.all()
	if recs[1].Result != "error" || recs[1].Value != "no-kv-token" {
		t.Errorf("up-error 台账字段不符: %+v", recs[1])
	}

	// down-ok：HandleCommand 发布指令落账
	cmd := mapper.DeviceCommand{DeviceName: DefaultDeviceName, Property: "targetTemp", Value: 22.5}
	if _, err := m.HandleCommand(cmd); err != nil {
		t.Fatalf("HandleCommand 失败: %v", err)
	}
	waitFor(t, "down 台账 1 条", func() bool { return lg.count(metamanager.DirDown) == 1 })
	want, _ := json.Marshal(cmd)
	for _, r := range lg.all() {
		if r.Direction == metamanager.DirDown &&
			(r.RegAddr != cmdTopic || r.Result != "ok" || r.Value != string(want)) {
			t.Errorf("down-ok 台账字段不符: %+v", r)
		}
	}

	// down 校验失败路径（设备名不符）不落账
	if _, err := m.HandleCommand(mapper.DeviceCommand{DeviceName: "other", Property: "x"}); err == nil {
		t.Fatal("设备名不符应报错")
	}
	if n := lg.count(metamanager.DirDown); n != 1 {
		t.Errorf("校验失败不应新增 down 台账: %d", n)
	}
}
