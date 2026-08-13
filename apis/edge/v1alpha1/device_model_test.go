package v1alpha1

import (
	"testing"
)

// TestDeviceModelSetDefaults 验证默认值填充：
// 未声明 AccessMode 的属性默认 ReadWrite，已声明的保持不变。
func TestDeviceModelSetDefaults(t *testing.T) {
	m := &DeviceModel{
		Spec: DeviceModelSpec{
			Protocol: "modbus",
			Properties: []DeviceProperty{
				{Name: "temperature", DataType: "int"},
				{Name: "switch", DataType: "boolean", AccessMode: AccessModeReadOnly},
			},
		},
	}
	m.SetDefaults()

	if m.Spec.Properties[0].AccessMode != AccessModeReadWrite {
		t.Errorf("temperature 的 AccessMode = %q, 期望默认 %q",
			m.Spec.Properties[0].AccessMode, AccessModeReadWrite)
	}
	if m.Spec.Properties[1].AccessMode != AccessModeReadOnly {
		t.Errorf("switch 的 AccessMode = %q, 期望保持显式值 %q",
			m.Spec.Properties[1].AccessMode, AccessModeReadOnly)
	}
}

// TestDeviceModelDeepCopy 验证深拷贝：修改副本的 map / slice，
// 不影响原对象。
func TestDeviceModelDeepCopy(t *testing.T) {
	orig := &DeviceModel{
		ObjectMeta: ObjectMeta{
			Name:   "temp-model",
			Labels: map[string]string{"kind": "sensor"},
		},
		Spec: DeviceModelSpec{
			Protocol: "modbus",
			Properties: []DeviceProperty{
				{
					Name:        "temperature",
					DataType:    "int",
					AccessMode:  AccessModeReadWrite,
					Minimum:     "-10",
					Maximum:     "100",
					Unit:        "celsius",
					Description: "温度传感器",
				},
			},
		},
	}

	cp := orig.DeepCopy()

	// 修改副本：map 键值、slice 元素与长度
	cp.Labels["kind"] = "actuator"
	cp.Spec.Properties[0].Name = "humidity"
	cp.Spec.Properties = append(cp.Spec.Properties, DeviceProperty{Name: "extra", DataType: "string"})

	// 断言原对象不受影响
	if orig.Labels["kind"] != "sensor" {
		t.Errorf("副本修改 Labels 影响了原对象: %q", orig.Labels["kind"])
	}
	if orig.Spec.Properties[0].Name != "temperature" {
		t.Errorf("副本修改 Properties 影响了原对象: %q", orig.Spec.Properties[0].Name)
	}
	if len(orig.Spec.Properties) != 1 {
		t.Errorf("副本追加 Properties 影响了原对象长度: %d", len(orig.Spec.Properties))
	}
}
