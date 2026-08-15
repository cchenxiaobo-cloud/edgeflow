// 云边通道消息压缩（WBS 4.4，ROADMAP 4.4「消息压缩」，audit-m02 #16 计划内延后项）。
//
// 帧格式（在既有 JSON 信封之外增加一层透明压缩，不改信封结构）：
//
//	明文: {JSON 信封}\n                      ← 旧格式，恒以 '{' 开头
//	压缩: "EFGZ" + flags(1B) + gzip(明文)    ← magic 4B + 标志 1B + gzip 负载
//
// flags 低 1 位为 gzip（0x01），其余位保留（后续可扩展 snappy 等）。
// 明文信封恒以 '{' 开头，与 magic "EFGZ" 不可能碰撞：接收端按前缀即可
// 无损区分两种格式，不需要额外握手。
//
// 兼容策略（v1.0 兼容性优先，协商式而非仅接收端降级）：
//   - Register / RegisterAck 恒为明文——它们是协商信道，且负载小（见
//     MinCompressBytes 跳过规则），明文不损失收益；
//   - 边缘在 Register 中声明 compression="gzip"（新增可选字段，旧云端
//     忽略未知 JSON 字段）；云端仅在 RegisterAck 中回带同字段时才启用
//     上行压缩——旧云端不回字段，新边缘自动保持明文（新边 ↔ 旧云兼容）；
//   - 云端仅对已声明 gzip 能力的连接启用下发压缩——旧边缘不声明，
//     云端自动保持明文下发（旧边 ↔ 新云兼容）。
//
// 小消息跳过：明文 < MinCompressBytes，或 gzip 结果不小于明文（不可压缩
// 数据/小消息膨胀）时，EncodeCompressed 直接返回明文——保证压缩后
// 字节数"永不更大"，且跳过后的明文对旧解码器完全透明。
//
// ReadLimit 交互：WebSocket 层 1 MiB 读限制作用于线缆字节（压缩字节）；
// 本层对解压后的明文输出同样设 MaxDecompressedBytes（1 MiB）上限，
// 防止"压缩炸弹"（小字节解压放大）——净效果是消息体量上限与 v1.0
// 明文通道完全一致（明文 ≤ 1 MiB），仅线缆字节可缩小。
//
// 并发安全：本层无共享状态（每次调用独立创建 gzip reader/writer 并
// 显式 Close），任意并发调用安全。
package protocol

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
)

// 压缩帧常量。
const (
	// compressMagic 是压缩帧头 magic（4 字节，ASCII "EFGZ"）。
	// 明文 JSON 信封恒以 '{' 开头，二者不可能碰撞。
	compressMagic = "EFGZ"
	// flagGzip 表示负载为 gzip 压缩的明文信封。
	flagGzip byte = 0x01
	// MinCompressBytes 是压缩生效的明文大小下限：小于该值的消息
	// （如心跳、注册应答）直接走明文，避免 gzip 头开销导致膨胀。
	MinCompressBytes = 256
	// MaxDecompressedBytes 是解压后明文的上限（1 MiB，与云边通道
	// 单消息大小上限对称）：防止压缩炸弹解压放大。
	MaxDecompressedBytes = 1 << 20
)

// EncodeCompressed 编码消息为可发送字节：小消息/不可压缩数据自动跳过
// 压缩（返回与 Encode 相同的明文），其余返回 "EFGZ"+flags+gzip 帧。
// 返回值保证不大于明文长度（跳过规则见文件头注释）。
func EncodeCompressed(m *Message) ([]byte, error) {
	plain, err := Encode(m)
	if err != nil {
		return nil, err
	}
	if len(plain) < MinCompressBytes {
		return plain, nil
	}
	var buf bytes.Buffer
	buf.WriteString(compressMagic)
	buf.WriteByte(flagGzip)
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(plain); err != nil {
		return nil, fmt.Errorf("gzip 压缩消息失败: %w", err)
	}
	// Close 必须显式执行：冲刷尾部并写入 gzip 校验和（CRC32），
	// 之后 buf 才是完整帧。失败（理论极少）按错误处理，不返回半帧。
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("gzip 收尾失败: %w", err)
	}
	if !shouldCompress(len(plain), buf.Len()) {
		// 不可压缩数据（如已压缩的 base64 负载）：压缩反而更大，跳过
		return plain, nil
	}
	return buf.Bytes(), nil
}

// shouldCompress 决策：压缩产物是否值得（严格小于明文才压缩）。
// 独立成函数便于直接测试边界（等大/膨胀必须跳过）。
func shouldCompress(plainLen, frameLen int) bool {
	return frameLen < plainLen
}

// DecodeCompressed 解码云边通道收到的字节：带压缩 magic 走解压路径，
// 否则按明文走原 Decode 路径（旧版本互操作：旧端发送的明文消息
// 无需任何配置即可被本层识别）。
func DecodeCompressed(data []byte) (*Message, error) {
	if !IsCompressed(data) {
		return Decode(data)
	}
	plain, err := decompress(data)
	if err != nil {
		return nil, err
	}
	return Decode(plain)
}

// IsCompressed 判断字节是否为压缩帧（magic 前缀匹配）。
// 明文信封恒以 '{' 开头，magic 前缀即充分判据。
func IsCompressed(data []byte) bool {
	return len(data) >= len(compressMagic) && string(data[:len(compressMagic)]) == compressMagic
}

// Decompress 解压压缩帧并返回明文字节（不做消息合法性校验），
// 供错误处理路径提取元信息（如 rejectInvalid 从非法压缩帧中
// 提取 source 回 Ack）。非压缩帧返回错误。
// 解压输出受 MaxDecompressedBytes 上限保护。
func Decompress(data []byte) ([]byte, error) {
	if !IsCompressed(data) {
		return nil, errors.New("不是压缩帧（缺少 magic）")
	}
	return decompress(data)
}

// decompress 解压帧负载：校验帧头长度与 flags，gzip 解压并施加
// MaxDecompressedBytes 明文上限（防压缩炸弹）。
func decompress(data []byte) ([]byte, error) {
	if len(data) < len(compressMagic)+1 {
		return nil, errors.New("压缩帧头不完整（magic 后缺 flags）")
	}
	flags := data[len(compressMagic)]
	if flags != flagGzip {
		return nil, fmt.Errorf("不支持的压缩标志 0x%02x（当前仅支持 gzip）", flags)
	}
	zr, err := gzip.NewReader(bytes.NewReader(data[len(compressMagic)+1:]))
	if err != nil {
		return nil, fmt.Errorf("gzip 头解析失败: %w", err)
	}
	defer func() { _ = zr.Close() }()
	// 多读 1 字节用于检测超限：恰好 MaxDecompressedBytes 合法，
	// 超过则 ReadAll 会读到第 Max+1 字节，据此报错。
	out, err := io.ReadAll(io.LimitReader(zr, MaxDecompressedBytes+1))
	if err != nil {
		return nil, fmt.Errorf("gzip 解压失败: %w", err)
	}
	if len(out) > MaxDecompressedBytes {
		return nil, fmt.Errorf("解压后消息 %d 字节超过上限 %d 字节（压缩炸弹或超限消息）",
			len(out), MaxDecompressedBytes)
	}
	return out, nil
}
