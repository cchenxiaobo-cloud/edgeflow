package opcua

import (
	"errors"
	"runtime"
	"testing"
	"time"
)

// ---- PRT-01：Variant 数组分配前校验 ----
// 注：本仓库 UA Binary 编解码为 **大端序**（见 binary.go BigEndian 约定），
// 手工构造报文一律经仓库 encoder，避免字节序手误。

// TestDecodeVariantMaliciousArrayLength：20 字节报文声明 16M 元素
// Boolean 数组（0x81 = Array|Boolean），分配前必须拒绝。
func TestDecodeVariantMaliciousArrayLength(t *testing.T) {
	e := encoder{}
	e.u8(VariantArray | VariantBoolean)
	e.i32(0x00FFFFFF)                                                        // 声明 16M 元素
	e.buf = append(e.buf, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15) // 仅 15 字节残余
	d := &decoder{b: e.buf}
	_, err := decodeVariant(d)
	if !errors.Is(err, ErrTooLong) {
		t.Fatalf("恶意大数组声明应拒绝并包装 ErrTooLong，实际: %v", err)
	}
}

// TestDecodeVariantMaliciousGuidArray：非 1 字节元素类型（Guid=16B）
// 同样按最小字节数校验。
func TestDecodeVariantMaliciousGuidArray(t *testing.T) {
	e := encoder{}
	e.u8(VariantArray | VariantGuid)
	e.i32(100000) // 需 ≥1.6MB，缓冲远不足
	e.buf = append(e.buf, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10)
	d := &decoder{b: e.buf}
	_, err := decodeVariant(d)
	if !errors.Is(err, ErrTooLong) {
		t.Fatalf("恶意 Guid 大数组声明应拒绝，实际: %v", err)
	}
}

// TestDecodeVariantSmallArrayStillWorks：合法小数组不受影响（防误拒，
// 且保持既有截断语义：小规模声明走解码循环内 ErrUnexpectedEOF）。
func TestDecodeVariantSmallArrayStillWorks(t *testing.T) {
	e := encoder{}
	e.u8(VariantArray | VariantBoolean)
	e.i32(3)
	e.buf = append(e.buf, 0x01, 0x00, 0x01)
	d := &decoder{b: e.buf}
	v, err := decodeVariant(d)
	if err != nil {
		t.Fatalf("合法 Boolean[3] 应解码成功，实际: %v", err)
	}
	arr, ok := v.Value.([]bool)
	if !ok || len(arr) != 3 || !arr[0] || arr[1] || !arr[2] {
		t.Fatalf("解码结果异常: %+v", v.Value)
	}
}

// ---- PRT-14：decodeStringList 分配前校验 ----

// TestDecodeStringListMaliciousLength：声明 16M 个 String（每元素至少
// 4 字节定界符）但残余缓冲仅 20 字节，分配前必须拒绝。
func TestDecodeStringListMaliciousLength(t *testing.T) {
	e := encoder{}
	e.i32(0x00FFFFFF) // 声明 16M 个 String
	e.buf = append(e.buf, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20)
	d := &decoder{b: e.buf}
	_, err := decodeStringList(d)
	if !errors.Is(err, ErrTooLong) {
		t.Fatalf("恶意 StringList 声明长度应拒绝并包装 ErrTooLong，实际: %v", err)
	}
}

// TestDecodeStringListSmallStillWorks：合法小列表不受影响（防误拒）。
func TestDecodeStringListSmallStillWorks(t *testing.T) {
	e := encoder{}
	e.i32(2)
	e.i32(2)
	e.buf = append(e.buf, 'a', 'b')
	e.i32(1)
	e.buf = append(e.buf, 'c')
	d := &decoder{b: e.buf}
	out, err := decodeStringList(d)
	if err != nil {
		t.Fatalf("合法 String[2] 应解码成功，实际: %v", err)
	}
	if len(out) != 2 || out[0] != "ab" || out[1] != "c" {
		t.Fatalf("解码结果异常: %q", out)
	}
}

// ---- PRT-04：泵异常退出必须关闭 pubCh，订阅方 range 可退出 ----

// TestPumpExitClosesPubCh：泵因连接级故障退出后，pubCh 被关闭
// （range 退出、goroutine 回收），不遗留泄漏。
func TestPumpExitClosesPubCh(t *testing.T) {
	addr := startMockOPCUServer(t, false)
	c, err := Open("opc.tcp://"+addr, 3*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = c.Close() }()

	base := runtime.NumGoroutine()
	// 白盒构造：模拟 Subscribe 之后的泵运行态；提前捕获通道引用——
	// pumpLoop 退出时会把 c.pubCh 字段置 nil（不复用已关通道），
	// 订阅方必须 range 捕获的引用而非事后读字段（nil 通道永不退出）。
	c.mu.Lock()
	pc := make(chan PublishResult, 16)
	c.pubCh = pc
	c.pumping = true
	c.mu.Unlock()
	go c.pumpLoop()

	// 模拟泵异常退出：连接级故障（关闭底层连接）
	_ = c.sc.conn.netConn.Close()

	// 订阅方 range 必须能退出（range 捕获的 pc，非字段）
	done := make(chan struct{})
	go func() {
		for range pc {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("泵异常退出后 pubCh 未关闭，订阅方 range 永久阻塞（PRT-04）")
	}

	// goroutine 数应回落到基线附近（泵 + range 均退出）
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= base+2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("goroutine 泄漏：基线 %d，当前 %d", base, runtime.NumGoroutine())
}
