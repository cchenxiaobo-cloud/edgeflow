package opcua

import (
	"errors"
	"testing"
)

// buildNestedDiagnostic 构造 depth 层嵌套的 DiagnosticInfo 报文：
// 前 depth-1 层仅置 InnerDiagnosticInfo 位（0x40，各占 1 字节），
// 最内层为空掩码 0x00。
func buildNestedDiagnostic(depth int) []byte {
	buf := make([]byte, depth)
	for i := 0; i < depth-1; i++ {
		buf[i] = 0x40
	}
	buf[depth-1] = 0x00
	return buf
}

// TestDiagnosticDepthLimit_200Rejected：200 层嵌套（> MaxDiagnosticDepth）
// 必须被拒绝（PRT-03）。
func TestDiagnosticDepthLimit_200Rejected(t *testing.T) {
	d := &decoder{b: buildNestedDiagnostic(200)}
	_, err := decodeDiagnosticInfo(d, 0)
	if err == nil {
		t.Fatal("200 层嵌套 DiagnosticInfo 应被拒绝，实际解码成功")
	}
	if !errors.Is(err, ErrTooLong) {
		t.Fatalf("错误应包装 ErrTooLong，实际: %v", err)
	}
}

// TestDiagnosticDepthLimit_100Accepted：恰好 MaxDiagnosticDepth 层
// 嵌套必须解码成功，且能逐层解到最内层。
func TestDiagnosticDepthLimit_100Accepted(t *testing.T) {
	d := &decoder{b: buildNestedDiagnostic(MaxDiagnosticDepth)}
	di, err := decodeDiagnosticInfo(d, 0)
	if err != nil {
		t.Fatalf("%d 层嵌套应解码成功，实际: %v", MaxDiagnosticDepth, err)
	}
	level := 1
	for inner := di.InnerDiagnosticInfo; inner != nil; inner = inner.InnerDiagnosticInfo {
		level++
	}
	if level != MaxDiagnosticDepth {
		t.Fatalf("嵌套层数应=%d，实际=%d", MaxDiagnosticDepth, level)
	}
}

// TestDiagnosticDepthLimit_List：数组元素内嵌恶意深层嵌套同样被拒绝。
func TestDiagnosticDepthLimit_List(t *testing.T) {
	// 数组长度 2：首个元素 200 层嵌套，第二字节起即深层链
	buf := []byte{0x02, 0x00, 0x00, 0x00}
	buf = append(buf, buildNestedDiagnostic(200)...)
	d := &decoder{b: buf}
	_, err := decodeDiagnosticInfoList(d)
	if !errors.Is(err, ErrTooLong) {
		t.Fatalf("数组内 200 层嵌套应拒绝并包装 ErrTooLong，实际: %v", err)
	}
}
