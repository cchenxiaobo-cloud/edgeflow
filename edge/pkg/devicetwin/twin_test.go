// 设备影子存储（WBS 3.5）测试：增查、合并语义、快照排序、深拷贝与并发安全。
package devicetwin

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestSetDesiredCreatesTwin 验证 SetDesired 自动创建影子：命令即声明，
// 设备无需等待 Mapper 首次采样即可进入影子。
func TestSetDesiredCreatesTwin(t *testing.T) {
	s := NewStore()
	s.SetDesired("sensor-01", "", "targetTemp", 25)

	twin, ok := s.Get("sensor-01", "")
	if !ok {
		t.Fatal("SetDesired 后应能查到影子")
	}
	if twin.DeviceName != "sensor-01" || twin.Namespace != DefaultNamespace {
		t.Errorf("设备标识不符: %+v", twin)
	}
	if twin.Desired["targetTemp"] != 25 {
		t.Errorf("Desired.targetTemp = %v，期望 25", twin.Desired["targetTemp"])
	}
	// 新建影子不应有上报值
	if len(twin.Reported) != 0 {
		t.Errorf("新建影子 Reported 应为空: %v", twin.Reported)
	}
	if twin.LastReportedAt != 0 {
		t.Errorf("新建影子 LastReportedAt 应为 0: %d", twin.LastReportedAt)
	}
}

// TestSetDesiredOverwritesSameProperty 验证同属性重复下发以最新值为准，
// 且不影响其他属性。
func TestSetDesiredOverwritesSameProperty(t *testing.T) {
	s := NewStore()
	s.SetDesired("sensor-01", "default", "targetTemp", 25)
	s.SetDesired("sensor-01", "default", "targetTemp", 28)
	s.SetDesired("sensor-01", "default", "fanSpeed", 3)

	twin, _ := s.Get("sensor-01", "default")
	if twin.Desired["targetTemp"] != 28 {
		t.Errorf("targetTemp = %v，期望 28（后写覆盖）", twin.Desired["targetTemp"])
	}
	if twin.Desired["fanSpeed"] != 3 {
		t.Errorf("fanSpeed = %v，期望 3", twin.Desired["fanSpeed"])
	}
}

// TestUpsertReportedMerge 验证上报合并语义：属性按名合并（本次未上报的
// 属性保留原值），reportedAt 刷新 LastReportedAt，且不影响 Desired。
func TestUpsertReportedMerge(t *testing.T) {
	s := NewStore()
	s.SetDesired("sensor-01", "default", "targetTemp", 25)
	s.UpsertReported("sensor-01", "default", map[string]float64{"temperature": 25.5, "humidity": 60}, 1000)
	s.UpsertReported("sensor-01", "default", map[string]float64{"humidity": 62}, 2000)

	twin, _ := s.Get("sensor-01", "default")
	if twin.Reported["temperature"] != 25.5 {
		t.Errorf("temperature = %v，期望 25.5（未再上报应保留）", twin.Reported["temperature"])
	}
	if twin.Reported["humidity"] != 62 {
		t.Errorf("humidity = %v，期望 62（覆盖）", twin.Reported["humidity"])
	}
	if twin.LastReportedAt != 2000 {
		t.Errorf("LastReportedAt = %d，期望 2000", twin.LastReportedAt)
	}
	if twin.Desired["targetTemp"] != 25 {
		t.Errorf("上报不应影响 Desired: %v", twin.Desired)
	}
}

// TestUpsertReportedCreatesTwin 验证 Mapper 先采样（无指令）时也能创建影子。
func TestUpsertReportedCreatesTwin(t *testing.T) {
	s := NewStore()
	s.UpsertReported("sensor-02", "default", map[string]float64{"temperature": 30.1}, 500)

	twin, ok := s.Get("sensor-02", "default")
	if !ok {
		t.Fatal("UpsertReported 后应能查到影子")
	}
	if twin.Reported["temperature"] != 30.1 || twin.LastReportedAt != 500 {
		t.Errorf("上报内容不符: %+v", twin)
	}
}

// TestGetMissingAndNamespaceIsolation 验证缺省命名空间查询与跨命名空间隔离。
func TestGetMissingAndNamespaceIsolation(t *testing.T) {
	s := NewStore()
	s.SetDesired("sensor-01", "default", "targetTemp", 25)

	if _, ok := s.Get("nope", "default"); ok {
		t.Error("不存在的设备应返回 false")
	}
	// 同 deviceName 不同 namespace 是不同影子
	s.SetDesired("sensor-01", "factory-a", "targetTemp", 40)
	twinDefault, _ := s.Get("sensor-01", "")
	twinFactory, _ := s.Get("sensor-01", "factory-a")
	if twinDefault.Desired["targetTemp"] == twinFactory.Desired["targetTemp"] {
		t.Errorf("跨命名空间应隔离: default=%v factory=%v",
			twinDefault.Desired["targetTemp"], twinFactory.Desired["targetTemp"])
	}
}

// TestSnapshotAllSorted 验证快照全量输出：排序稳定、深拷贝（改快照不污染存储）。
func TestSnapshotAllSorted(t *testing.T) {
	s := NewStore()
	s.SetDesired("b-dev", "default", "p", 1)
	s.SetDesired("a-dev", "default", "p", 2)
	s.SetDesired("c-dev", "zone-x", "p", 3)

	snaps := s.SnapshotAll()
	if len(snaps) != 3 {
		t.Fatalf("快照数 = %d，期望 3", len(snaps))
	}
	// 排序：namespace → deviceName
	expect := []string{"default/a-dev", "default/b-dev", "zone-x/c-dev"}
	for i, e := range expect {
		got := snaps[i].Namespace + "/" + snaps[i].DeviceName
		if got != e {
			t.Errorf("snaps[%d] = %q，期望 %q", i, got, e)
		}
	}

	// 深拷贝验证：修改快照不影响存储
	snaps[0].Desired["p"] = 999
	snaps[0].Reported["x"] = 1
	got, _ := s.Get("a-dev", "default")
	if got.Desired["p"] != 2 {
		t.Errorf("修改快照污染了存储: %v", got.Desired)
	}
	if _, ok := got.Reported["x"]; ok {
		t.Error("修改快照污染了存储 Reported")
	}
}

// TestSnapshotAllEmpty 验证空存储快照为非 nil 空切片（JSON 编码为 []）。
func TestSnapshotAllEmpty(t *testing.T) {
	s := NewStore()
	snaps := s.SnapshotAll()
	if snaps == nil || len(snaps) != 0 {
		t.Errorf("空存储快照应为非 nil 空切片: %#v", snaps)
	}
}

// TestIgnoreInvalidInputs 验证空 deviceName/property/properties 被忽略（不 panic）。
func TestIgnoreInvalidInputs(t *testing.T) {
	s := NewStore()
	s.SetDesired("", "default", "p", 1)                            // 空设备名
	s.SetDesired("dev", "default", "", 1)                          // 空属性名
	s.UpsertReported("", "default", map[string]float64{"p": 1}, 1) // 空设备名
	s.UpsertReported("dev", "default", nil, 1)                     // 空属性表
	if len(s.SnapshotAll()) != 0 {
		t.Errorf("非法输入不应创建影子: %+v", s.SnapshotAll())
	}
}

// TestUpsertReportedMonotonicTimestamp 验证 LastReportedAt 单调性保护
// （M3A P2-4）：乱序上报（网络重排/多采集源/时钟回拨）不回退时间戳；
// 属性仍按名合并（合并语义不受影响）。
func TestUpsertReportedMonotonicTimestamp(t *testing.T) {
	s := NewStore()
	s.UpsertReported("d", "default", map[string]float64{"temperature": 25.5}, 2000)
	s.UpsertReported("d", "default", map[string]float64{"temperature": 26.0}, 1000) // 乱序：更旧的时间戳

	twin, ok := s.Get("d", "default")
	if !ok {
		t.Fatal("影子应存在")
	}
	// 时间戳不回退
	if twin.LastReportedAt != 2000 {
		t.Errorf("LastReportedAt = %d，期望 2000（单调不回退）", twin.LastReportedAt)
	}
	// 属性仍合并（最新写入的值生效，合并语义不变）
	if twin.Reported["temperature"] != 26.0 {
		t.Errorf("temperature = %v，期望 26.0（属性合并不受时间戳单调保护影响）", twin.Reported["temperature"])
	}

	// 后续更新更大的时间戳正常刷新
	s.UpsertReported("d", "default", map[string]float64{"humidity": 60}, 3000)
	twin, _ = s.Get("d", "default")
	if twin.LastReportedAt != 3000 {
		t.Errorf("LastReportedAt = %d，期望 3000", twin.LastReportedAt)
	}
	if twin.Reported["temperature"] != 26.0 {
		t.Errorf("temperature 应保留为 26.0: %v", twin.Reported["temperature"])
	}
}

// TestConcurrentAccess 验证并发安全：多 goroutine 混合读写
// （配合 go test -race 检测数据竞争）。
func TestConcurrentAccess(t *testing.T) {
	s := NewStore()
	const workers = 8
	const rounds = 200

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			name := fmt.Sprintf("dev-%d", w%4)
			for i := 0; i < rounds; i++ {
				s.SetDesired(name, "default", "p", float64(i))
				s.UpsertReported(name, "default", map[string]float64{"v": float64(i)}, int64(i))
				_, _ = s.Get(name, "default")
				_ = s.SnapshotAll()
				time.Sleep(time.Microsecond)
			}
		}(w)
	}
	wg.Wait()

	// 收尾校验：数据一致（不因竞争丢失/错乱）
	for w := 0; w < 4; w++ {
		twin, ok := s.Get(fmt.Sprintf("dev-%d", w), "default")
		if !ok {
			t.Fatalf("dev-%d 影子丢失", w)
		}
		if len(twin.Desired) != 1 || len(twin.Reported) != 1 {
			t.Errorf("dev-%d 属性数异常: desired=%v reported=%v", w, twin.Desired, twin.Reported)
		}
	}
}
