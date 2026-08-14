// 云端设备状态存储（WBS 5.3）测试：增查、字段级合并、排序、深拷贝与并发安全。
package devicestatus

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestUpsertBasic 验证基本写入：nodeID 以参数为准（消息来源即权威）、
// namespace 缺省补 default、可整体覆盖。
func TestUpsertBasic(t *testing.T) {
	s := NewStore()
	// payload 伪造 nodeID 不生效
	s.Upsert("edge-1", DeviceStatus{NodeID: "evil", DeviceName: "sensor-01", Namespace: "default",
		Properties: map[string]float64{"temperature": 25.5}, LastReportedAt: 1000})

	got, ok := s.Get("edge-1", "default", "sensor-01")
	if !ok {
		t.Fatal("Upsert 后应能查到")
	}
	if got.NodeID != "edge-1" {
		t.Errorf("NodeID = %q，期望 edge-1（参数优先）", got.NodeID)
	}
	if got.Properties["temperature"] != 25.5 || got.LastReportedAt != 1000 {
		t.Errorf("内容不符: %+v", got)
	}

	// 缺省 namespace
	s.Upsert("edge-1", DeviceStatus{DeviceName: "sensor-02", Properties: map[string]float64{"humidity": 60}})
	got2, ok := s.Get("edge-1", "", "sensor-02")
	if !ok || got2.Namespace != DefaultNamespace {
		t.Errorf("缺省 namespace 应补 default: ok=%v %+v", ok, got2)
	}
}

// TestUpsertPreservesDesired 验证字段级合并：设备上报（Upsert）不得清空
// 云端指令写入的期望值（DeviceReport 不含 desired 字段）。
func TestUpsertPreservesDesired(t *testing.T) {
	s := NewStore()
	s.SetDesired("edge-1", "default", "sensor-01", "targetTemp", 25)

	// 设备上报一轮（无 desired 字段）
	s.Upsert("edge-1", DeviceStatus{DeviceName: "sensor-01", Namespace: "default",
		Properties: map[string]float64{"temperature": 25.5}, LastReportedAt: 2000})

	got, _ := s.Get("edge-1", "default", "sensor-01")
	if got.Desired["targetTemp"] != 25 {
		t.Errorf("上报不应清空期望值: %v", got.Desired)
	}
	if got.Properties["temperature"] != 25.5 || got.LastReportedAt != 2000 {
		t.Errorf("上报内容应更新: %+v", got)
	}
}

// TestSetDesired 验证指令落点：仅更新指定属性期望值、保留已上报属性、
// 记录不存在时自动创建（指令即声明）。
func TestSetDesired(t *testing.T) {
	s := NewStore()
	s.SetDesired("edge-1", "default", "sensor-01", "targetTemp", 25)
	s.Upsert("edge-1", DeviceStatus{DeviceName: "sensor-01", Namespace: "default",
		Properties: map[string]float64{"temperature": 26.0}, LastReportedAt: 3000})
	s.SetDesired("edge-1", "default", "sensor-01", "fanSpeed", 2)

	got, _ := s.Get("edge-1", "default", "sensor-01")
	if got.Desired["targetTemp"] != 25 || got.Desired["fanSpeed"] != 2 {
		t.Errorf("Desired 不符: %v", got.Desired)
	}
	if got.Properties["temperature"] != 26.0 || got.LastReportedAt != 3000 {
		t.Errorf("已上报属性应保留: %+v", got)
	}

	// 自动创建：只下过指令、从未上报的设备
	s.SetDesired("edge-1", "", "actuator-9", "power", 1)
	got2, ok := s.Get("edge-1", "default", "actuator-9")
	if !ok || got2.Desired["power"] != 1 || got2.Namespace != DefaultNamespace {
		t.Errorf("自动创建不符: ok=%v %+v", ok, got2)
	}
	if len(got2.Properties) != 0 {
		t.Errorf("新建记录 Properties 应为空: %v", got2.Properties)
	}
}

// TestGetMissing 验证不存在与空参数返回 false（不 panic）。
func TestGetMissing(t *testing.T) {
	s := NewStore()
	if _, ok := s.Get("edge-1", "default", "nope"); ok {
		t.Error("不存在的设备应返回 false")
	}
	if _, ok := s.Get("", "default", "dev"); ok {
		t.Error("空 nodeID 应返回 false")
	}
	if _, ok := s.Get("edge-1", "default", ""); ok {
		t.Error("空 deviceName 应返回 false")
	}
}

// TestDelete 验证删除语义：幂等、节点下无设备后清理空 map。
func TestDelete(t *testing.T) {
	s := NewStore()
	s.Upsert("edge-1", DeviceStatus{DeviceName: "sensor-01", Properties: map[string]float64{"p": 1}})

	if !s.Delete("edge-1", "default", "sensor-01") {
		t.Error("存在时应删除成功")
	}
	if s.Delete("edge-1", "default", "sensor-01") {
		t.Error("重复删除应返回 false（幂等）")
	}
	if _, ok := s.Get("edge-1", "default", "sensor-01"); ok {
		t.Error("删除后不应再查到")
	}
	// 空参数不 panic
	s.Delete("", "default", "dev")
	s.Delete("edge-1", "default", "")
}

// TestListAllSorted 验证全量列表：排序（nodeID → namespace → deviceName）、
// 深拷贝（改列表不污染存储）、空列表非 nil。
func TestListAllSorted(t *testing.T) {
	s := NewStore()
	s.Upsert("edge-2", DeviceStatus{DeviceName: "b", Namespace: "default", Properties: map[string]float64{"p": 1}})
	s.Upsert("edge-1", DeviceStatus{DeviceName: "a", Namespace: "zone-x", Properties: map[string]float64{"p": 2}})
	s.Upsert("edge-1", DeviceStatus{DeviceName: "a", Namespace: "default", Properties: map[string]float64{"p": 3}})

	all := s.ListAll()
	if len(all) != 3 {
		t.Fatalf("条目数 = %d，期望 3", len(all))
	}
	expect := []string{"edge-1/default/a", "edge-1/zone-x/a", "edge-2/default/b"}
	for i, e := range expect {
		got := all[i].NodeID + "/" + all[i].Namespace + "/" + all[i].DeviceName
		if got != e {
			t.Errorf("items[%d] = %q，期望 %q", i, got, e)
		}
	}

	// 深拷贝验证
	all[0].Properties["p"] = 999
	got, _ := s.Get("edge-1", "default", "a")
	if got.Properties["p"] != 3 {
		t.Errorf("修改列表污染了存储: %v", got.Properties)
	}

	if got2 := NewStore().ListAll(); got2 == nil || len(got2) != 0 {
		t.Errorf("空存储列表应为非 nil 空切片: %#v", got2)
	}
}

// TestListByNode 验证单节点列表：仅含该节点、排序、空列表非 nil。
func TestListByNode(t *testing.T) {
	s := NewStore()
	s.Upsert("edge-1", DeviceStatus{DeviceName: "b", Properties: map[string]float64{"p": 1}})
	s.Upsert("edge-1", DeviceStatus{DeviceName: "a", Properties: map[string]float64{"p": 2}})
	s.Upsert("edge-2", DeviceStatus{DeviceName: "c", Properties: map[string]float64{"p": 3}})

	items := s.ListByNode("edge-1")
	if len(items) != 2 {
		t.Fatalf("条目数 = %d，期望 2", len(items))
	}
	if items[0].DeviceName != "a" || items[1].DeviceName != "b" {
		t.Errorf("应按 deviceName 排序: %+v", items)
	}
	if got := s.ListByNode("nope"); got == nil || len(got) != 0 {
		t.Errorf("无设备节点列表应为非 nil 空切片: %#v", got)
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
				s.Upsert("edge-1", DeviceStatus{DeviceName: name,
					Properties: map[string]float64{"v": float64(i)}, LastReportedAt: int64(i)})
				s.SetDesired("edge-1", "default", name, "p", float64(i))
				_, _ = s.Get("edge-1", "default", name)
				_ = s.ListAll()
				_ = s.ListByNode("edge-1")
				time.Sleep(time.Microsecond)
			}
		}(w)
	}
	wg.Wait()

	for w := 0; w < 4; w++ {
		got, ok := s.Get("edge-1", "default", fmt.Sprintf("dev-%d", w))
		if !ok {
			t.Fatalf("dev-%d 记录丢失", w)
		}
		if len(got.Desired) != 1 || len(got.Properties) != 1 {
			t.Errorf("dev-%d 字段异常: desired=%v properties=%v", w, got.Desired, got.Properties)
		}
	}
}
