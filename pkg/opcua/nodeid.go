package opcua

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// ParseNodeID 把字符串形式的节点标识解析为 NodeId，供
// EDGEFLOW_OPCUA_NODES 配置（如 "temperature=ns=2;i=1001"）使用。
//
// 支持的形式（与 NodeId.String() 输出互逆，types.go L184-207）：
//
//	ns=2;i=1001        数字节点（十进制）
//	ns=0;s=temperature 字符串节点
//	ns=1;g=<GUID>      GUID 节点（8-4-4-4-12 或连续 32 hex）
//	ns=1;b=<HEX>       ByteString 节点（十六进制字节串）
//	1001               纯数字 → ns=0;i=1001（宽容形式）
//
// 非法输入（负 ns、非数字 id、空 s=、非法 GUID/HEX 等）返回错误。
func ParseNodeID(s string) (NodeId, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return NodeId{}, fmt.Errorf("opcua: 空节点标识")
	}
	// 宽容形式：纯数字 → ns=0;i=<n>
	if !strings.Contains(s, ";") && !strings.Contains(s, "=") {
		n, err := strconv.ParseUint(s, 10, 32)
		if err != nil {
			return NodeId{}, fmt.Errorf("opcua: 非法节点标识 %q（纯数字形式需为 32 位无符号整数）", s)
		}
		return NewNodeID(0, uint32(n)), nil
	}
	parts := strings.Split(s, ";")
	if len(parts) != 2 {
		return NodeId{}, fmt.Errorf("opcua: 非法节点标识 %q（期望 ns=<n>;<type>=<id> 两段）", s)
	}
	nsPart, idPart := parts[0], parts[1]
	if !strings.HasPrefix(nsPart, "ns=") {
		return NodeId{}, fmt.Errorf("opcua: 非法节点标识 %q（首段需为 ns=<namespace>）", s)
	}
	ns, err := strconv.ParseUint(strings.TrimPrefix(nsPart, "ns="), 10, 32)
	if err != nil {
		return NodeId{}, fmt.Errorf("opcua: 非法命名空间索引 %q", nsPart)
	}
	kind, id, ok := strings.Cut(idPart, "=")
	if !ok {
		return NodeId{}, fmt.Errorf("opcua: 非法节点标识 %q（第二段需为 <type>=<id>）", s)
	}
	switch kind {
	case "i":
		num, err := strconv.ParseUint(id, 10, 32)
		if err != nil {
			return NodeId{}, fmt.Errorf("opcua: 非法数字节点标识 %q（需为 32 位无符号整数）", id)
		}
		return NewNodeID(uint32(ns), uint32(num)), nil
	case "s":
		if id == "" {
			return NodeId{}, fmt.Errorf("opcua: 非法字符串节点标识 %q（s= 后不能为空）", s)
		}
		return NewStringNodeID(uint32(ns), id), nil
	case "g":
		g, err := parseGUID(id)
		if err != nil {
			return NodeId{}, fmt.Errorf("opcua: 非法 GUID 节点标识 %q: %w", s, err)
		}
		return NewGuidNodeID(uint32(ns), g), nil
	case "b":
		raw := strings.ReplaceAll(id, " ", "")
		if raw == "" {
			return NodeId{}, fmt.Errorf("opcua: 非法 ByteString 节点标识 %q（b= 后不能为空）", s)
		}
		if len(raw)%2 != 0 {
			return NodeId{}, fmt.Errorf("opcua: 非法 ByteString 节点标识 %q（十六进制长度需为偶数）", s)
		}
		b, err := hex.DecodeString(raw)
		if err != nil {
			return NodeId{}, fmt.Errorf("opcua: 非法 ByteString 节点标识 %q: %w", s, err)
		}
		return NewByteStringNodeID(uint32(ns), b), nil
	default:
		return NodeId{}, fmt.Errorf("opcua: 不支持的节点标识类型 %q（支持 i/s/g/b）", kind)
	}
}

// parseGUID 解析 8-4-4-4-12 或连续 32 位十六进制形式的 GUID。
func parseGUID(s string) (Guid, error) {
	var g Guid
	compact := strings.ReplaceAll(s, "-", "")
	if len(compact) != 32 {
		return g, fmt.Errorf("GUID %q 长度非法（期望 32 位十六进制）", s)
	}
	b, err := hex.DecodeString(compact)
	if err != nil {
		return g, fmt.Errorf("GUID %q 非十六进制: %w", s, err)
	}
	g.Data1 = uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	g.Data2 = uint16(b[4])<<8 | uint16(b[5])
	g.Data3 = uint16(b[6])<<8 | uint16(b[7])
	copy(g.Data4[:], b[8:16])
	return g, nil
}
