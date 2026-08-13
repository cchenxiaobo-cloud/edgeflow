package v1alpha1

import (
	"reflect"
	"testing"
)

// TestDeepCopyEquality 覆盖所有类型的 DeepCopy 方法：
// 拷贝结果应与原对象完全相等（reflect.DeepEqual）。
func TestDeepCopyEquality(t *testing.T) {
	cases := []struct {
		name string
		got  any // DeepCopy 的结果
		want any // 期望的相等对象
	}{
		{
			"TypeMeta",
			(&TypeMeta{Kind: "EdgeNode", APIVersion: "edgeflow.io/v1alpha1"}).DeepCopy(),
			&TypeMeta{Kind: "EdgeNode", APIVersion: "edgeflow.io/v1alpha1"},
		},
		{
			"ObjectMeta",
			(&ObjectMeta{
				Name:            "n",
				Namespace:       "ns",
				Labels:          map[string]string{"a": "1"},
				Annotations:     map[string]string{"b": "2"},
				UID:             "u",
				ResourceVersion: "3",
			}).DeepCopy(),
			&ObjectMeta{
				Name:            "n",
				Namespace:       "ns",
				Labels:          map[string]string{"a": "1"},
				Annotations:     map[string]string{"b": "2"},
				UID:             "u",
				ResourceVersion: "3",
			},
		},
		{
			"EdgeNodeSpec",
			(&EdgeNodeSpec{
				NodeID:    "id",
				Role:      NodeRoleEdge,
				Addresses: []NodeAddress{{Type: NodeAddressTypeInternalIP, Address: "10.0.0.1"}},
			}).DeepCopy(),
			&EdgeNodeSpec{
				NodeID:    "id",
				Role:      NodeRoleEdge,
				Addresses: []NodeAddress{{Type: NodeAddressTypeInternalIP, Address: "10.0.0.1"}},
			},
		},
		{
			"NodeAddress",
			(&NodeAddress{Type: NodeAddressTypeHostname, Address: "h1"}).DeepCopy(),
			&NodeAddress{Type: NodeAddressTypeHostname, Address: "h1"},
		},
		{
			"EdgeNodeStatus",
			(&EdgeNodeStatus{
				Phase:         NodePhaseRunning,
				HeartbeatTime: "2026-08-13T12:00:00Z",
				LastSeenTime:  "2026-08-13T12:00:01Z",
				Conditions:    []NodeCondition{{Type: NodeConditionReady, Status: "True"}},
				Version:       "v0.1.0",
			}).DeepCopy(),
			&EdgeNodeStatus{
				Phase:         NodePhaseRunning,
				HeartbeatTime: "2026-08-13T12:00:00Z",
				LastSeenTime:  "2026-08-13T12:00:01Z",
				Conditions:    []NodeCondition{{Type: NodeConditionReady, Status: "True"}},
				Version:       "v0.1.0",
			},
		},
		{
			"NodeCondition",
			(&NodeCondition{
				Type:               NodeConditionReady,
				Status:             "True",
				Reason:             "ok",
				Message:            "一切正常",
				LastTransitionTime: "2026-08-13T12:00:00Z",
			}).DeepCopy(),
			&NodeCondition{
				Type:               NodeConditionReady,
				Status:             "True",
				Reason:             "ok",
				Message:            "一切正常",
				LastTransitionTime: "2026-08-13T12:00:00Z",
			},
		},
		{
			"DeviceModelSpec",
			(&DeviceModelSpec{
				Protocol: "modbus",
				Properties: []DeviceProperty{
					{Name: "temperature", DataType: "int", AccessMode: AccessModeReadOnly, Unit: "celsius"},
				},
			}).DeepCopy(),
			&DeviceModelSpec{
				Protocol: "modbus",
				Properties: []DeviceProperty{
					{Name: "temperature", DataType: "int", AccessMode: AccessModeReadOnly, Unit: "celsius"},
				},
			},
		},
		{
			"DeviceProperty",
			(&DeviceProperty{
				Name: "temperature", Description: "温度", DataType: "int",
				AccessMode: AccessModeReadWrite, DefaultValue: "20", Minimum: "-10", Maximum: "100", Unit: "celsius",
			}).DeepCopy(),
			&DeviceProperty{
				Name: "temperature", Description: "温度", DataType: "int",
				AccessMode: AccessModeReadWrite, DefaultValue: "20", Minimum: "-10", Maximum: "100", Unit: "celsius",
			},
		},
		{
			"DeviceSpec",
			(&DeviceSpec{
				DeviceModelRef: "model",
				NodeName:       "node",
				Protocol:       ProtocolConfig{ProtocolName: "modbus", Config: map[string]string{"port": "1"}},
				Properties: []DevicePropertySpec{
					{Name: "temperature", Desired: &PropertyValue{Value: "30", Metadata: map[string]string{"unit": "celsius"}}},
				},
			}).DeepCopy(),
			&DeviceSpec{
				DeviceModelRef: "model",
				NodeName:       "node",
				Protocol:       ProtocolConfig{ProtocolName: "modbus", Config: map[string]string{"port": "1"}},
				Properties: []DevicePropertySpec{
					{Name: "temperature", Desired: &PropertyValue{Value: "30", Metadata: map[string]string{"unit": "celsius"}}},
				},
			},
		},
		{
			"ProtocolConfig",
			(&ProtocolConfig{ProtocolName: "mqtt", Config: map[string]string{"host": "127.0.0.1"}}).DeepCopy(),
			&ProtocolConfig{ProtocolName: "mqtt", Config: map[string]string{"host": "127.0.0.1"}},
		},
		{
			"DevicePropertySpec",
			(&DevicePropertySpec{Name: "temperature", Desired: &PropertyValue{Value: "30"}}).DeepCopy(),
			&DevicePropertySpec{Name: "temperature", Desired: &PropertyValue{Value: "30"}},
		},
		{
			"PropertyValue",
			(&PropertyValue{Value: "30", Metadata: map[string]string{"unit": "celsius"}}).DeepCopy(),
			&PropertyValue{Value: "30", Metadata: map[string]string{"unit": "celsius"}},
		},
		{
			"DeviceStatus",
			(&DeviceStatus{
				Twins: []TwinProperty{
					{PropertyName: "temperature", Desired: &PropertyValue{Value: "30"}, Reported: &PropertyValue{Value: "29.5"}},
				},
				LastUpdatedTime: "2026-08-13T12:00:00Z",
			}).DeepCopy(),
			&DeviceStatus{
				Twins: []TwinProperty{
					{PropertyName: "temperature", Desired: &PropertyValue{Value: "30"}, Reported: &PropertyValue{Value: "29.5"}},
				},
				LastUpdatedTime: "2026-08-13T12:00:00Z",
			},
		},
		{
			"TwinProperty",
			(&TwinProperty{PropertyName: "temperature", Reported: &PropertyValue{Value: "29.5"}}).DeepCopy(),
			&TwinProperty{PropertyName: "temperature", Reported: &PropertyValue{Value: "29.5"}},
		},
	}

	for _, tc := range cases {
		if !reflect.DeepEqual(tc.got, tc.want) {
			t.Errorf("%s: DeepCopy 结果与原对象不相等:\n got  = %#v\n want = %#v", tc.name, tc.got, tc.want)
		}
	}
}

// TestDeepCopyNilReceiver 验证 nil 接收者的 DeepCopy 返回 nil，且不 panic。
func TestDeepCopyNilReceiver(t *testing.T) {
	var (
		meta  *TypeMeta
		obj   *ObjectMeta
		node  *EdgeNode
		nspec *EdgeNodeSpec
		naddr *NodeAddress
		nstat *EdgeNodeStatus
		ncond *NodeCondition
		model *DeviceModel
		mspec *DeviceModelSpec
		prop  *DeviceProperty
		dev   *Device
		dspec *DeviceSpec
		pc    *ProtocolConfig
		pspec *DevicePropertySpec
		pv    *PropertyValue
		dstat *DeviceStatus
		twin  *TwinProperty
	)
	all := []struct {
		name string
		got  any
	}{
		{"TypeMeta", meta.DeepCopy()},
		{"ObjectMeta", obj.DeepCopy()},
		{"EdgeNode", node.DeepCopy()},
		{"EdgeNodeSpec", nspec.DeepCopy()},
		{"NodeAddress", naddr.DeepCopy()},
		{"EdgeNodeStatus", nstat.DeepCopy()},
		{"NodeCondition", ncond.DeepCopy()},
		{"DeviceModel", model.DeepCopy()},
		{"DeviceModelSpec", mspec.DeepCopy()},
		{"DeviceProperty", prop.DeepCopy()},
		{"Device", dev.DeepCopy()},
		{"DeviceSpec", dspec.DeepCopy()},
		{"ProtocolConfig", pc.DeepCopy()},
		{"DevicePropertySpec", pspec.DeepCopy()},
		{"PropertyValue", pv.DeepCopy()},
		{"DeviceStatus", dstat.DeepCopy()},
		{"TwinProperty", twin.DeepCopy()},
	}
	for _, tc := range all {
		// 注意：typed nil 指针装入 any 后不等于 nil，需用反射判断
		if v := reflect.ValueOf(tc.got); !v.IsNil() {
			t.Errorf("%s: nil 接收者的 DeepCopy 应返回 nil, got %#v", tc.name, tc.got)
		}
	}
}

// TestDeepCopyNilCollections 验证零值对象（map / slice / 指针均为 nil）
// 的深拷贝不 panic，且 nil 保持为 nil。
func TestDeepCopyNilCollections(t *testing.T) {
	node := (&EdgeNode{}).DeepCopy()
	if node.Labels != nil || node.Annotations != nil || node.Spec.Addresses != nil || node.Status.Conditions != nil {
		t.Errorf("EdgeNode: nil 集合未被保留为 nil")
	}

	model := (&DeviceModel{}).DeepCopy()
	if model.Spec.Properties != nil {
		t.Errorf("DeviceModel: nil Properties 未被保留为 nil")
	}

	dev := (&Device{}).DeepCopy()
	if dev.Spec.Protocol.Config != nil || dev.Spec.Properties != nil || dev.Status.Twins != nil {
		t.Errorf("Device: nil 集合未被保留为 nil")
	}

	// 指针字段为 nil 时不 panic，且保持 nil
	twin := (&TwinProperty{PropertyName: "x"}).DeepCopy()
	if twin.Desired != nil || twin.Reported != nil {
		t.Errorf("TwinProperty: nil 指针未被保留为 nil")
	}
}

// TestDeepCopyIndependence 验证常见嵌套结构深拷贝的独立性
// （补足资源级测试未覆盖的路径：ProtocolConfig、PropertyValue 的 map）。
func TestDeepCopyIndependence(t *testing.T) {
	orig := &ProtocolConfig{
		ProtocolName: "modbus",
		Config:       map[string]string{"baudRate": "115200"},
	}
	cp := orig.DeepCopy()
	cp.Config["baudRate"] = "9600"
	if orig.Config["baudRate"] != "115200" {
		t.Errorf("副本修改 ProtocolConfig.Config 影响了原对象: %q", orig.Config["baudRate"])
	}

	origPv := &PropertyValue{Value: "30", Metadata: map[string]string{"unit": "celsius"}}
	cpPv := origPv.DeepCopy()
	cpPv.Metadata["unit"] = "fahrenheit"
	if origPv.Metadata["unit"] != "celsius" {
		t.Errorf("副本修改 PropertyValue.Metadata 影响了原对象: %q", origPv.Metadata["unit"])
	}
}
