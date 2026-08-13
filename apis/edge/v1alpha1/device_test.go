package v1alpha1

import (
	"testing"
)

// TestDeviceRequiredFieldsNotDefaulted 验证必填字段不会被默认值掩盖：
// DeviceModelRef 与 NodeName 必须显式填写（防止"看起来能跑、实际绑错对象"）。
func TestDeviceRequiredFieldsNotDefaulted(t *testing.T) {
	d := &Device{}
	if d.Spec.DeviceModelRef != "" {
		t.Errorf("DeviceModelRef 被默认填充: %q", d.Spec.DeviceModelRef)
	}
	if d.Spec.NodeName != "" {
		t.Errorf("NodeName 被默认填充: %q", d.Spec.NodeName)
	}
}

// TestDeviceDeepCopy 验证深拷贝：修改副本的 map / slice / 指针字段，
// 不影响原对象。
func TestDeviceDeepCopy(t *testing.T) {
	orig := &Device{
		ObjectMeta: ObjectMeta{
			Name:   "sensor-01",
			Labels: map[string]string{"site": "s1"},
		},
		Spec: DeviceSpec{
			DeviceModelRef: "temp-model",
			NodeName:       "edge-1",
			Protocol: ProtocolConfig{
				ProtocolName: "modbus",
				Config:       map[string]string{"serialPort": "1", "baudRate": "115200"},
			},
			Properties: []DevicePropertySpec{
				{
					Name: "temperature",
					Desired: &PropertyValue{
						Value:    "30",
						Metadata: map[string]string{"unit": "celsius"},
					},
				},
			},
		},
		Status: DeviceStatus{
			Twins: []TwinProperty{
				{
					PropertyName: "temperature",
					Desired:      &PropertyValue{Value: "30"},
					Reported:     &PropertyValue{Value: "29.5"},
				},
			},
			LastUpdatedTime: "2026-08-13T12:00:00Z",
		},
	}

	cp := orig.DeepCopy()

	// 修改副本：map 键值、slice 元素与长度、指针字段指向的内容
	cp.Labels["site"] = "s2"
	cp.Spec.Protocol.Config["baudRate"] = "9600"
	cp.Spec.Properties[0].Desired.Value = "99"
	cp.Spec.Properties[0].Desired.Metadata["unit"] = "fahrenheit"
	cp.Spec.Properties = append(cp.Spec.Properties, DevicePropertySpec{Name: "extra"})
	cp.Status.Twins[0].Reported.Value = "0"
	cp.Status.LastUpdatedTime = "2026-08-13T13:00:00Z"

	// 断言原对象不受影响
	if orig.Labels["site"] != "s1" {
		t.Errorf("副本修改 Labels 影响了原对象: %q", orig.Labels["site"])
	}
	if orig.Spec.Protocol.Config["baudRate"] != "115200" {
		t.Errorf("副本修改 Protocol.Config 影响了原对象: %q", orig.Spec.Protocol.Config["baudRate"])
	}
	if orig.Spec.Properties[0].Desired.Value != "30" {
		t.Errorf("副本修改 Desired.Value 影响了原对象: %q", orig.Spec.Properties[0].Desired.Value)
	}
	if orig.Spec.Properties[0].Desired.Metadata["unit"] != "celsius" {
		t.Errorf("副本修改 Desired.Metadata 影响了原对象: %q", orig.Spec.Properties[0].Desired.Metadata["unit"])
	}
	if len(orig.Spec.Properties) != 1 {
		t.Errorf("副本追加 Properties 影响了原对象长度: %d", len(orig.Spec.Properties))
	}
	if orig.Status.Twins[0].Reported.Value != "29.5" {
		t.Errorf("副本修改 Reported.Value 影响了原对象: %q", orig.Status.Twins[0].Reported.Value)
	}
	if orig.Status.LastUpdatedTime != "2026-08-13T12:00:00Z" {
		t.Errorf("副本修改 LastUpdatedTime 影响了原对象: %q", orig.Status.LastUpdatedTime)
	}
}
