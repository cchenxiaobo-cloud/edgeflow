// v0410_rtu_test.go：Modbus RTU-over-TCP 联调（spec 0014 US-1）。
//
// 链路：ModbusMapper（rtutcp://）→ pkg/modbusrtu 客户端（RTU 帧 + CRC16）
// → modbussim RTU 模式（CRC/从站校验 → 共享寄存器模型）→ RTU 帧应答。
// 覆盖 Collect（读保持寄存器 0x0000-0x0001）与 HandleCommand（写目标温度
// 寄存器 + 回读、写线圈 + 回读）——与 TCP 路径同语义、不同帧封装。
package modbus

import (
	"context"
	"errors"
	"testing"
	"time"

	"edgeflow/edge/pkg/mapper"
	"edgeflow/pkg/modbusrtu"
	"edgeflow/pkg/modbussim"
)

// TestModbusMapperRTUOverTCP 覆盖 RTU 联调全链：采集 + 指令写回读。
func TestModbusMapperRTUOverTCP(t *testing.T) {
	sim := modbussim.New("127.0.0.1:0", modbussim.WithSeed(42), modbussim.WithStep(30*time.Millisecond))
	if err := sim.Start(); err != nil {
		t.Fatalf("启动模拟器失败: %v", err)
	}
	t.Cleanup(func() { _ = sim.Stop() })
	rtuAddr, err := sim.StartRTU("127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动 RTU 监听失败: %v", err)
	}

	m := New("rtutcp://"+rtuAddr, WithTimeout(2*time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("启动 Mapper 失败: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop() })

	// 1. 采集（FC 0x03 读保持寄存器）：温度/湿度为 sim 随机初值（20-30 / 40-70）。
	props, err := m.Collect()
	if err != nil {
		t.Fatalf("RTU 采集失败: %v", err)
	}
	temp, hum := props["temperature"], props["humidity"]
	if temp < 20 || temp > 30 || hum < 40 || hum > 70 {
		t.Fatalf("RTU 采集值越界: %v", props)
	}

	// 2. 指令（FC 0x06 写寄存器 + 内部回读验证）：写目标温度 30°C。
	// DeviceReport.Properties 只含 Collect 属性（temperature/humidity）——
	// 写一致性由 handleTargetTemp 内部回读校验（不一致即返回错误），
	// 与 TCP 路径同契约。
	rep, err := m.HandleCommand(mapper.DeviceCommand{
		DeviceName: "mb-sensor-01", Namespace: "default", Property: "targetTemp", Value: 30,
	})
	if err != nil {
		t.Fatalf("RTU 写目标温度失败: %v", err)
	}
	if _, ok := rep.Properties["temperature"]; !ok {
		t.Fatalf("快照应含 temperature: %v", rep.Properties)
	}

	// 3. 指令（FC 0x05 写线圈 + FC 0x01 回读）：开/关 coil0 各一次。
	rep, err = m.HandleCommand(mapper.DeviceCommand{
		DeviceName: "mb-sensor-01", Namespace: "default", Property: "coil0", Value: 1,
	})
	if err != nil {
		t.Fatalf("RTU 写线圈失败: %v", err)
	}
	rep, err = m.HandleCommand(mapper.DeviceCommand{
		DeviceName: "mb-sensor-01", Namespace: "default", Property: "coil0", Value: 0,
	})
	if err != nil {
		t.Fatalf("RTU 关线圈失败: %v", err)
	}

	// 4. 二次采集（连接复用 + 波动后的值域校验）。
	props, err = m.Collect()
	if err != nil {
		t.Fatalf("二次 RTU 采集失败: %v", err)
	}
	if props["temperature"] < 20 || props["temperature"] > 30 {
		t.Fatalf("二次采集温度越界: %v", props)
	}

	// 5. 设备名/命名空间声明（与 TCP 路径同语义）。
	if m.DeviceNames()[0] != "mb-sensor-01" || m.DeviceNamespace() != "default" {
		t.Fatalf("设备声明不符: %v / %s", m.DeviceNames(), m.DeviceNamespace())
	}
}

// TestModbusRTUExceptionFromSim 覆盖 mapper↔模拟器全链异常应答（复核 P2-10）：
// 越界从站（0）请求 → 模拟器回异常码 0x0B（网关目标设备无响应）。
func TestModbusRTUExceptionFromSim(t *testing.T) {
	sim := modbussim.New("127.0.0.1:0", modbussim.WithSeed(42))
	if err := sim.Start(); err != nil {
		t.Fatalf("启动模拟器失败: %v", err)
	}
	t.Cleanup(func() { _ = sim.Stop() })
	rtuAddr, err := sim.StartRTU("127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动 RTU 监听失败: %v", err)
	}
	c, err := modbusrtu.DialTCP(rtuAddr, 0, time.Second) // slaveID=0：越界段
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	defer func() { _ = c.Close() }()
	_, err = c.ReadHoldingRegisters(0x0000, 1)
	var mbErr *modbusrtu.ModbusError
	if !errors.As(err, &mbErr) || mbErr.Code != 0x0B {
		t.Fatalf("应得异常码 0x0B: %v", err)
	}
}
