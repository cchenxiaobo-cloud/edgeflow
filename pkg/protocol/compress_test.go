package protocol

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

// testPayload 构造指定大小的可压缩负载（重复文本，模拟 PodSync/ConfigSync
// 这类含重复结构的 JSON 负载）。
func testPayload(size int) string {
	base := strings.Repeat(`{"key":"value","data":"some repetitive text for compression","n":12345},`, 8)
	var sb strings.Builder
	for sb.Len() < size {
		sb.WriteString(base)
	}
	return sb.String()[:size]
}

// newTestMessage 构造一条可直接编码的消息（负载为给定字符串）。
func newTestMessage(t *testing.T, payload string) *Message {
	t.Helper()
	m, err := NewMessage(TypePodStatus, "edge-node-1", "cloud", map[string]string{"data": payload})
	if err != nil {
		t.Fatalf("构造消息失败: %v", err)
	}
	return m
}

func TestEncodeCompressedRoundtrip(t *testing.T) {
	sizes := []int{100, 255, 256, 257, 1024, 64 * 1024}
	for _, size := range sizes {
		size := size
		t.Run("size_"+strconv.Itoa(size), func(t *testing.T) {
			msg := newTestMessage(t, testPayload(size))
			frame, err := EncodeCompressed(msg)
			if err != nil {
				t.Fatalf("EncodeCompressed(%d) 失败: %v", size, err)
			}
			got, err := DecodeCompressed(frame)
			if err != nil {
				t.Fatalf("DecodeCompressed(%d) 失败: %v", size, err)
			}
			if got.ID != msg.ID || got.Type != msg.Type || got.Source != msg.Source ||
				got.Target != msg.Target || got.Version != msg.Version {
				t.Errorf("往返后信封字段不一致: got %+v, want %+v", got, msg)
			}
			if !bytes.Equal(got.Payload, msg.Payload) {
				t.Errorf("往返后 Payload 不一致")
			}
			// 大消息必须是压缩帧且真正变小
			if size >= MinCompressBytes {
				if !IsCompressed(frame) {
					t.Errorf("size=%d 应产生压缩帧，实际明文", size)
				}
				if len(frame) >= len(testPayload(size))+64 {
					t.Errorf("size=%d 压缩帧 %d 字节未比明文负载 %d 字节小", size, len(frame), size)
				}
			}
		})
	}
}

func TestEncodeCompressedSmallMessageSkipped(t *testing.T) {
	// 小消息（< MinCompressBytes）：EncodeCompressed 返回与 Encode 相同的明文
	msg := newTestMessage(t, "tiny")
	plain, err := Encode(msg)
	if err != nil {
		t.Fatalf("Encode 失败: %v", err)
	}
	if len(plain) >= MinCompressBytes {
		t.Fatalf("测试前提不成立：消息 %d 字节应小于 %d", len(plain), MinCompressBytes)
	}
	frame, err := EncodeCompressed(msg)
	if err != nil {
		t.Fatalf("EncodeCompressed 失败: %v", err)
	}
	if IsCompressed(frame) {
		t.Fatalf("小消息不应被压缩，实际产出压缩帧")
	}
	if !bytes.Equal(frame, plain) {
		t.Fatalf("小消息应原样返回明文: got %q, want %q", frame, plain)
	}
	// 明文帧可被旧解码器 Decode 正常解析（旧版本互操作）
	if _, err := Decode(frame); err != nil {
		t.Fatalf("旧解码器解析明文帧失败: %v", err)
	}
}

func TestEncodeCompressedPreCompressedPayload(t *testing.T) {
	// 已压缩内容（gzip 流再 base64，实际业务中最不可压缩的负载形态）：
	// 即使 gzip 可回收部分 base64 冗余，产物也不得大于明文，且往返必须正确。
	rnd := rand.New(rand.NewSource(42))
	raw := make([]byte, 4096)
	if _, err := rnd.Read(raw); err != nil {
		t.Fatalf("生成随机数据失败: %v", err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(raw); err != nil {
		t.Fatalf("gzip 写入失败: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip 收尾失败: %v", err)
	}
	msg := newTestMessage(t, base64.StdEncoding.EncodeToString(gz.Bytes()))
	plain, err := Encode(msg)
	if err != nil {
		t.Fatalf("Encode 失败: %v", err)
	}
	frame, err := EncodeCompressed(msg)
	if err != nil {
		t.Fatalf("EncodeCompressed 失败: %v", err)
	}
	if len(frame) > len(plain) {
		t.Fatalf("压缩产物 %d 字节大于明文 %d 字节", len(frame), len(plain))
	}
	if _, err := DecodeCompressed(frame); err != nil {
		t.Fatalf("往返失败: %v", err)
	}
}

// TestShouldCompressSkipDecision 直接验证「压缩产物不小于明文则跳过」的
// 决策边界。说明：合法 JSON（≥MinCompressBytes）内容均可被 gzip 缩小
// （base64 的 4/3 冗余也能被回收），该分支在生产中几乎不会触发，是
// 防御性安全阀；此处用决策原语覆盖其边界条件。
func TestShouldCompressSkipDecision(t *testing.T) {
	cases := []struct {
		plainLen, frameLen int
		want               bool
	}{
		{500, 499, true},  // 压缩后更小 → 压缩
		{500, 500, false}, // 等大 → 跳过
		{500, 501, false}, // 膨胀 → 跳过
		{300, 200, true},
		{MinCompressBytes, MinCompressBytes, false},
	}
	for _, c := range cases {
		if got := shouldCompress(c.plainLen, c.frameLen); got != c.want {
			t.Errorf("shouldCompress(%d,%d)=%v, want %v", c.plainLen, c.frameLen, got, c.want)
		}
	}
}

func TestEncodeCompressedNeverLarger(t *testing.T) {
	// 性质测试：任意消息 EncodeCompressed 的产物永不大于明文
	rnd := rand.New(rand.NewSource(7))
	for i := 0; i < 50; i++ {
		size := 200 + rnd.Intn(8000)
		var payload string
		if i%2 == 0 {
			payload = testPayload(size) // 可压缩
		} else {
			raw := make([]byte, size)
			_, _ = rnd.Read(raw)
			payload = base64.StdEncoding.EncodeToString(raw) // 不可压缩
		}
		msg := newTestMessage(t, payload)
		plain, err := Encode(msg)
		if err != nil {
			t.Fatalf("Encode 失败: %v", err)
		}
		frame, err := EncodeCompressed(msg)
		if err != nil {
			t.Fatalf("EncodeCompressed 失败: %v", err)
		}
		if len(frame) > len(plain) {
			t.Fatalf("压缩产物 %d 字节大于明文 %d 字节（size=%d）", len(frame), len(plain), size)
		}
	}
}

func TestDecodeCompressedPlaintextFallback(t *testing.T) {
	// 旧版本互操作：明文消息（旧端发送）无需配置即可被新解码器识别
	msg := newTestMessage(t, testPayload(4096))
	plain, err := Encode(msg)
	if err != nil {
		t.Fatalf("Encode 失败: %v", err)
	}
	got, err := DecodeCompressed(plain)
	if err != nil {
		t.Fatalf("DecodeCompressed 解析明文失败: %v", err)
	}
	if got.ID != msg.ID || !bytes.Equal(got.Payload, msg.Payload) {
		t.Fatalf("明文路径解析结果不一致")
	}
}

func TestDecodeCompressedOldDecoderRejects(t *testing.T) {
	// 旧解码器（只认明文的 Decode）遇到压缩帧应报错而非静默产出错误消息——
	// 协商式兼容策略保证旧端不会真的收到压缩帧（见 EncodeCompressed 接入点），
	// 此测试固化"误收时显式失败"的行为，便于排查。
	msg := newTestMessage(t, testPayload(4096))
	frame, err := EncodeCompressed(msg)
	if err != nil {
		t.Fatalf("EncodeCompressed 失败: %v", err)
	}
	if !IsCompressed(frame) {
		t.Fatalf("测试前提不成立：大消息应产出压缩帧")
	}
	if _, err := Decode(frame); err == nil {
		t.Fatalf("旧解码器应拒绝压缩帧")
	}
}

func TestDecodeCompressedCorrupted(t *testing.T) {
	msg := newTestMessage(t, testPayload(4096))
	frame, err := EncodeCompressed(msg)
	if err != nil {
		t.Fatalf("EncodeCompressed 失败: %v", err)
	}

	t.Run("截断帧", func(t *testing.T) {
		truncated := frame[:len(frame)-8] // 砍掉尾部（gzip 校验和/尾部）
		if _, err := DecodeCompressed(truncated); err == nil {
			t.Fatalf("截断帧应报错")
		}
	})
	t.Run("破坏 gzip 负载", func(t *testing.T) {
		corrupted := append([]byte(nil), frame...)
		corrupted[len(corrupted)-1] ^= 0xFF // 翻转负载末字节（CRC 校验失败）
		if _, err := DecodeCompressed(corrupted); err == nil {
			t.Fatalf("损坏负载应报错")
		}
	})
	t.Run("不支持的压缩标志", func(t *testing.T) {
		bad := append([]byte(nil), frame...)
		bad[len(compressMagic)] = 0x02 // flags 位图不支持的值
		if _, err := DecodeCompressed(bad); err == nil {
			t.Fatalf("未知 flags 应报错")
		}
	})
	t.Run("帧头不完整", func(t *testing.T) {
		bad := append([]byte(nil), frame[:len(compressMagic)]...) // 只有 magic
		if _, err := DecodeCompressed(bad); err == nil {
			t.Fatalf("缺 flags 的帧应报错")
		}
	})
	t.Run("假 magic 非 gzip", func(t *testing.T) {
		bad := append([]byte(compressMagic), 0x01, 'a', 'b', 'c') // magic+flags 但负载非 gzip
		if _, err := DecodeCompressed(bad); err == nil {
			t.Fatalf("非 gzip 负载应报错")
		}
	})
}

func TestDecodeCompressedBomb(t *testing.T) {
	// 压缩炸弹：1 MiB 上限作用于明文输出（ReadLimit 交互，见文件头注释）
	// ——小字节 gzip 帧解压出 2 MiB 明文必须被拒绝，且不 panic。
	var buf bytes.Buffer
	buf.WriteString(compressMagic)
	buf.WriteByte(flagGzip)
	zw := gzip.NewWriter(&buf)
	// 4 MiB 全零明文，gzip 后仅 ~4KB
	if _, err := zw.Write(make([]byte, 4*MaxDecompressedBytes)); err != nil {
		t.Fatalf("gzip 写入失败: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip 收尾失败: %v", err)
	}
	if buf.Len() >= MaxDecompressedBytes {
		t.Fatalf("测试前提不成立：炸弹帧应远小于明文")
	}
	if _, err := DecodeCompressed(buf.Bytes()); err == nil {
		t.Fatalf("压缩炸弹应被明文上限拒绝")
	} else if !strings.Contains(err.Error(), "上限") {
		t.Errorf("炸弹报错应指明上限: %v", err)
	}

	// 恰好等于上限的明文应放行
	var exact bytes.Buffer
	exact.WriteString(compressMagic)
	exact.WriteByte(flagGzip)
	zw2 := gzip.NewWriter(&exact)
	if _, err := zw2.Write(bytes.Repeat([]byte("a"), MaxDecompressedBytes-64)); err != nil {
		t.Fatalf("gzip 写入失败: %v", err)
	}
	if err := zw2.Close(); err != nil {
		t.Fatalf("gzip 收尾失败: %v", err)
	}
	if _, err := Decompress(exact.Bytes()); err != nil {
		t.Fatalf("恰好上限内的帧应解压成功: %v", err)
	}
}

func TestDecompress(t *testing.T) {
	// Decompress：解压不校验消息合法性（错误处理路径提取元信息用）
	msg := newTestMessage(t, testPayload(4096))
	plain, err := Encode(msg)
	if err != nil {
		t.Fatalf("Encode 失败: %v", err)
	}
	frame, err := EncodeCompressed(msg)
	if err != nil {
		t.Fatalf("EncodeCompressed 失败: %v", err)
	}
	got, err := Decompress(frame)
	if err != nil {
		t.Fatalf("Decompress 失败: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("Decompress 结果与明文不一致")
	}
	// 明文输入应报错（Decompress 只接受压缩帧）
	if _, err := Decompress(plain); err == nil {
		t.Fatalf("Decompress 应拒绝明文输入")
	}
	// 非法消息（缺字段）解压本身应成功——校验是 Decode 的职责
	badMsg := &Message{ID: "x", Type: TypeHeartbeat} // 缺 Source/Target/Version
	badPlain, err := Encode(badMsg)
	if err != nil {
		t.Fatalf("Encode 失败: %v", err)
	}
	if len(badPlain) < MinCompressBytes {
		badPlain = append(badPlain, bytes.Repeat([]byte(" "), MinCompressBytes)...) // 撑大以便压缩
	}
	if _, err := Decompress(mustCompress(t, badPlain)); err != nil {
		t.Fatalf("非法消息解压应成功（校验不属 Decompress 职责）: %v", err)
	}
}

// mustCompress 直接构造一个 gzip 压缩帧（测试辅助，绕过跳过规则）。
func mustCompress(t *testing.T, plain []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString(compressMagic)
	buf.WriteByte(flagGzip)
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(plain); err != nil {
		t.Fatalf("gzip 写入失败: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip 收尾失败: %v", err)
	}
	return buf.Bytes()
}

func TestIsCompressed(t *testing.T) {
	msg := newTestMessage(t, testPayload(4096))
	frame, err := EncodeCompressed(msg)
	if err != nil {
		t.Fatalf("EncodeCompressed 失败: %v", err)
	}
	if !IsCompressed(frame) {
		t.Fatalf("压缩帧应识别为压缩")
	}
	plain, err := Encode(msg)
	if err != nil {
		t.Fatalf("Encode 失败: %v", err)
	}
	if IsCompressed(plain) {
		t.Fatalf("明文不应识别为压缩")
	}
	if IsCompressed([]byte("EFG")) {
		t.Fatalf("不足 4 字节的前缀不应识别为压缩")
	}
	if IsCompressed([]byte("EFGX")) {
		t.Fatalf("非 magic 前缀不应识别为压缩")
	}
}
