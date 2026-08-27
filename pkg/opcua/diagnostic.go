package opcua

import (
	"fmt"
)

// DiagnosticInfo 是 OPC UA DiagnosticInfo 类型（OPC UA Part 6
// §5.2.2.16，Part 4 §7.8）的完整实现。
//
// v0.14.0 从空骨架补全位域语义：服务消息的 ResponseHeader /
// ActivateSessionResponse / ReadResponse / WriteResponse 等都必须
// 解码 DiagnosticInfo（服务端通常返回空或简单条目），空骨架会导致
// 服务层解码失败。编码实现同步提供（round-trip 测试 + 服务端
// 错误回包可选）。
//
// 注意：Variant 内嵌 DiagnosticInfo（Variant 类型掩码 0x19）仍保持
// ErrUnsupportedType 不解码（v1 边界，见 decodeBuiltin）——本类型
// 只在服务消息头部/尾部作为独立字段解码。
type DiagnosticInfo struct {
	// SymbolicID 对应编码掩码 bit0（有符号 32 位整数，指向服务端
	// 符号表条目）。
	SymbolicID *int32
	// NamespaceURI 对应编码掩码 bit1（指向命名空间 URI 表条目）。
	NamespaceURI *int32
	// Locale 对应编码掩码 bit2（指向 locale 表条目）。
	Locale *int32
	// LocalizedText 对应编码掩码 bit3（指向本地化文本表条目）。
	LocalizedText *int32
	// AdditionalInfo 对应编码掩码 bit4（自由文本）。
	AdditionalInfo *string
	// InnerStatusCode 对应编码掩码 bit5（内层状态码）。
	InnerStatusCode *StatusCode
	// InnerDiagnosticInfo 对应编码掩码 bit6（递归内层诊断信息）。
	InnerDiagnosticInfo *DiagnosticInfo
}

// DiagnosticInfo 编码掩码位（OPC UA Part 6 §5.2.2.16）。
const (
	diagSymbolicIDMask      byte = 0x01
	diagNamespaceURIMask    byte = 0x02
	diagLocaleMask          byte = 0x04
	diagLocalizedTextMask   byte = 0x08
	diagAdditionalInfoMask  byte = 0x10
	diagInnerStatusMask     byte = 0x20
	diagInnerDiagnosticMask byte = 0x40
)

// diagValidMask 是所有合法的编码掩码位。
const diagValidMask byte = diagSymbolicIDMask | diagNamespaceURIMask | diagLocaleMask |
	diagLocalizedTextMask | diagAdditionalInfoMask | diagInnerStatusMask | diagInnerDiagnosticMask

// MaxArrayLength 是服务层数组字段的最大元素数（防御性上限，
// 与 Variant 数组的 1<<24 上限同数量级安全边界）。
const MaxArrayLength = 1 << 24

// encodeUA 把 DiagnosticInfo 编码为 UA Binary 字节流。
func (d DiagnosticInfo) encodeUA(e *encoder) error {
	var mask byte
	if d.SymbolicID != nil {
		mask |= diagSymbolicIDMask
	}
	if d.NamespaceURI != nil {
		mask |= diagNamespaceURIMask
	}
	if d.Locale != nil {
		mask |= diagLocaleMask
	}
	if d.LocalizedText != nil {
		mask |= diagLocalizedTextMask
	}
	if d.AdditionalInfo != nil {
		mask |= diagAdditionalInfoMask
	}
	if d.InnerStatusCode != nil {
		mask |= diagInnerStatusMask
	}
	if d.InnerDiagnosticInfo != nil {
		mask |= diagInnerDiagnosticMask
	}
	e.u8(mask)
	if d.SymbolicID != nil {
		e.i32(*d.SymbolicID)
	}
	if d.NamespaceURI != nil {
		e.i32(*d.NamespaceURI)
	}
	if d.Locale != nil {
		e.i32(*d.Locale)
	}
	if d.LocalizedText != nil {
		e.i32(*d.LocalizedText)
	}
	if d.AdditionalInfo != nil {
		e.str(*d.AdditionalInfo)
	}
	if d.InnerStatusCode != nil {
		e.u32(uint32(*d.InnerStatusCode))
	}
	if d.InnerDiagnosticInfo != nil {
		if err := d.InnerDiagnosticInfo.encodeUA(e); err != nil {
			return err
		}
	}
	return nil
}

// decodeDiagnosticInfo 从字节流解码一个 DiagnosticInfo。保留位
// （bit7）被拒绝；每个掩码位按规范逐个解码，bit6 递归。
func decodeDiagnosticInfo(d *decoder) (DiagnosticInfo, error) {
	mask, err := d.u8()
	if err != nil {
		return DiagnosticInfo{}, err
	}
	if mask&^diagValidMask != 0 {
		return DiagnosticInfo{}, fmt.Errorf("%w: DiagnosticInfo mask 0x%02X with reserved bits", ErrInvalidEncoding, mask)
	}
	var di DiagnosticInfo
	if mask&diagSymbolicIDMask != 0 {
		v, err := d.i32()
		if err != nil {
			return DiagnosticInfo{}, err
		}
		di.SymbolicID = &v
	}
	if mask&diagNamespaceURIMask != 0 {
		v, err := d.i32()
		if err != nil {
			return DiagnosticInfo{}, err
		}
		di.NamespaceURI = &v
	}
	if mask&diagLocaleMask != 0 {
		v, err := d.i32()
		if err != nil {
			return DiagnosticInfo{}, err
		}
		di.Locale = &v
	}
	if mask&diagLocalizedTextMask != 0 {
		v, err := d.i32()
		if err != nil {
			return DiagnosticInfo{}, err
		}
		di.LocalizedText = &v
	}
	if mask&diagAdditionalInfoMask != 0 {
		v, err := d.str()
		if err != nil {
			return DiagnosticInfo{}, err
		}
		di.AdditionalInfo = &v
	}
	if mask&diagInnerStatusMask != 0 {
		v, err := d.u32()
		if err != nil {
			return DiagnosticInfo{}, err
		}
		st := StatusCode(v)
		di.InnerStatusCode = &st
	}
	if mask&diagInnerDiagnosticMask != 0 {
		inner, err := decodeDiagnosticInfo(d)
		if err != nil {
			return DiagnosticInfo{}, err
		}
		di.InnerDiagnosticInfo = &inner
	}
	return di, nil
}

// decodeDiagnosticInfoList 解码 DiagnosticInfo 数组（长度 -1 = null
// → nil；每个元素按需解码）。
func decodeDiagnosticInfoList(d *decoder) ([]DiagnosticInfo, error) {
	n, err := d.i32()
	if err != nil {
		return nil, err
	}
	if n == -1 {
		return nil, nil
	}
	if n < 0 {
		return nil, fmt.Errorf("%w: negative DiagnosticInfo array length %d", ErrInvalidEncoding, n)
	}
	if n > MaxArrayLength {
		return nil, fmt.Errorf("%w: DiagnosticInfo array length %d exceeds limit %d", ErrTooLong, n, MaxArrayLength)
	}
	out := make([]DiagnosticInfo, 0, n)
	for i := int32(0); i < n; i++ {
		di, err := decodeDiagnosticInfo(d)
		if err != nil {
			return nil, err
		}
		out = append(out, di)
	}
	return out, nil
}

// encodeDiagnosticInfoList 编码 DiagnosticInfo 数组（nil → null）。
func encodeDiagnosticInfoList(e *encoder, list []DiagnosticInfo) {
	if list == nil {
		e.i32(-1)
		return
	}
	e.i32(int32(len(list)))
	for i := range list {
		_ = list[i].encodeUA(e) // 编码不依赖外部状态，忽略错误以保持签名简单
	}
}
