package main

import (
	"os"
	"strings"
	"testing"

	"edgeflow/pkg/alarm"
	"edgeflow/pkg/mediaup"
	"edgeflow/pkg/protocol"
)

// unsetenv 是 os.Unsetenv 的测试包装（保持用例可读）。
func unsetenv(key string) error {
	return os.Unsetenv(key)
}

// v0.45.0（spec 0018 US-3/US-5）困难样本采集与加速卡 env 测试。

func TestV0450EnvAccelList(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want []string
	}{
		{"未设置", "", nil},
		{"单条目", "gpu:cuda-12.4", []string{"gpu:cuda-12.4"}},
		{"多条目+空白", " gpu:cuda-12.4 , npu:rockchip-9996 ", []string{"gpu:cuda-12.4", "npu:rockchip-9996"}},
		{"非法条目剔除", "gpu:cuda, junk", []string{"gpu:cuda"}},
		{"仅逗号", ",,,", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env == "" {
				_ = unsetenv(EnvEdgeCoreAccels)
			} else {
				t.Setenv(EnvEdgeCoreAccels, tc.env)
			}
			got := envAccelList()
			if len(got) != len(tc.want) {
				t.Fatalf("envAccelList() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("envAccelList()[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestV0450HardSampleCollectorGate(t *testing.T) {
	_ = unsetenv(EnvHardSampleEnabled)
	if c := newHardSampleCollector("n1", nil, nil); c != nil {
		t.Fatal("开关 off 应返回 nil（零行为）")
	}
	t.Setenv(EnvHardSampleEnabled, "on")
	c := newHardSampleCollector("n1", nil, nil)
	if c == nil {
		t.Fatal("开关 on 应返回采集器")
	}
	// 非法 max 回退默认 1。
	t.Setenv(EnvHardSampleMaxPerAlarm, "99")
	c2 := newHardSampleCollector("n1", nil, nil)
	if c2 == nil || c2.maxPerAlarm != 1 {
		t.Fatalf("非法 max 应回退 1，got %v", c2)
	}
	t.Setenv(EnvHardSampleMaxPerAlarm, "3")
	c3 := newHardSampleCollector("n1", nil, nil)
	if c3 == nil || c3.maxPerAlarm != 3 {
		t.Fatalf("合法 max=3 应生效，got %v", c3)
	}
}

func TestV0450HardSampleAdmitPerAlarm(t *testing.T) {
	t.Setenv(EnvHardSampleEnabled, "on")
	c := newHardSampleCollector("n1", nil, nil)
	for i := 0; i < 5; i++ {
		if got := c.admit("alm-1"); got != (i < 1) {
			t.Fatalf("admit #%d = %v（max=1 应只放行 1 次）", i, got)
		}
	}
	if c.admit("alm-2") != true {
		t.Fatal("新告警应放行")
	}
}

func TestV0450HardSampleOnAlarmNoFrameSafe(t *testing.T) {
	t.Setenv(EnvHardSampleEnabled, "on")
	// holder 无 sink：LatestSnapshot 返回 !ok → 跳过且不 panic。
	c := newHardSampleCollector("n1", &mediaSinkHolder{}, nil)
	c.OnAlarm(alarm.Alarm{AlarmID: "alm-x", DeviceName: "cam-01"})
	if c.droppedCount.Load() != 1 {
		t.Fatalf("无帧源应 dropped=1，got %d", c.droppedCount.Load())
	}
}

// v0450fakeQueue 是 mediaup.Enqueuer 的本包假实现（仅覆盖空快照防御路径）。
type v0450fakeQueue struct{}

func (v0450fakeQueue) EnqueueUplink(_ int, _ *protocol.Message) (int64, error) { return 0, nil }

func (v0450fakeQueue) PendingUplinkIDs(ids []int64) ([]int64, error) { return nil, nil }

func TestV0450EnqueueHardSampleEmptySafe(t *testing.T) {
	// EnqueueHardSample 空快照返回 nil（防御）——不经 spool。
	u := mediaup.NewUploader(t.TempDir(), "n1", v0450fakeQueue{}, nil, 0)
	if err := u.EnqueueHardSample(mediaup.Clip{DeviceName: "cam"}); err != nil {
		t.Fatalf("空快照应 nil，got %v", err)
	}
	_ = strings.TrimSpace("")
}

func TestV0450AddLinkageOrder(t *testing.T) {
	var order []string
	m := &alarmManager{active: map[string]*alarmEpisode{}}
	m.AddLinkage(linkageFunc(func(a alarm.Alarm) { order = append(order, "first") }))
	m.AddLinkage(linkageFunc(func(a alarm.Alarm) { order = append(order, "second") }))
	m.linkage.OnAlarm(alarm.Alarm{})
	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Fatalf("联动顺序 = %v", order)
	}
	m.AddLinkage(nil) // nil 安全
}

type linkageFunc func(alarm.Alarm)

func (f linkageFunc) OnAlarm(a alarm.Alarm) { f(a) }
