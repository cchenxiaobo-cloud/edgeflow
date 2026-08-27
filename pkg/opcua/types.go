package opcua

import (
	"fmt"
	"reflect"
	"time"
)

// ---------------------------------------------------------------------
// DateTime
// ---------------------------------------------------------------------

// DateTime is a UA DateTime: the number of 100-nanosecond intervals
// since 1601-01-01T00:00:00Z (the Windows FILETIME epoch), encoded as
// an Int64 in UA Binary.
type DateTime int64

// filetimeEpochSeconds is the number of seconds between the FILETIME
// epoch (1601-01-01) and the Unix epoch (1970-01-01).
const filetimeEpochSeconds int64 = 11644473600

// DateTimeFromTime converts a time.Time to a UA DateTime. Sub-100 ns
// precision is truncated.
func DateTimeFromTime(t time.Time) DateTime {
	sec := t.Unix() - filetimeEpochSeconds
	return DateTime(sec*1e7 + int64(t.Nanosecond())/100)
}

// Time converts the DateTime back to a time.Time in UTC.
func (d DateTime) Time() time.Time {
	sec := int64(d) / 1e7
	sub := (int64(d) % 1e7) * 100 // 100 ns ticks -> nanoseconds
	return time.Unix(filetimeEpochSeconds+sec, sub).UTC()
}

// ---------------------------------------------------------------------
// Guid
// ---------------------------------------------------------------------

// Guid is a 16-byte universally unique identifier, encoded in UA
// Binary as Data1 (UInt32) + Data2 (UInt16) + Data3 (UInt16) +
// Data4 (8 bytes), all big-endian (OPC UA Part 6 §5.2.2.12).
type Guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

// String renders the Guid in canonical 8-4-4-4-12 hex form.
func (g Guid) String() string {
	return fmt.Sprintf("%08X-%04X-%04X-%02X%02X-%02X%02X%02X%02X%02X%02X",
		g.Data1, g.Data2, g.Data3,
		g.Data4[0], g.Data4[1], g.Data4[2], g.Data4[3],
		g.Data4[4], g.Data4[5], g.Data4[6], g.Data4[7])
}

// ---------------------------------------------------------------------
// StatusCode
// ---------------------------------------------------------------------

// StatusCode is a 32-bit OPC UA status code (OPC UA Part 4 §7.34).
// Bits 30-31 carry the severity; bit 29 marks structure changes;
// bits 0-28 carry the code.
type StatusCode uint32

// Well-known status codes (subset; see Part 4 §7.34 for the full table).
const (
	StatusGood                  StatusCode = 0x00000000
	StatusUncertain             StatusCode = 0x40000000
	StatusBad                   StatusCode = 0x80000000
	StatusBadUnexpectedError    StatusCode = 0x80010000
	StatusBadInternalError      StatusCode = 0x80020000
	StatusBadOutOfMemory        StatusCode = 0x80030000
	StatusBadCommunicationError StatusCode = 0x80050000
	StatusBadTimeout            StatusCode = 0x80060000
)

// Severity is the top two bits of a StatusCode.
type Severity uint8

// Severity values (OPC UA Part 4 §7.34).
const (
	SeverityGood      Severity = 0
	SeverityUncertain Severity = 1
	SeverityBad       Severity = 2
	SeverityReserved  Severity = 3
)

// Severity returns the severity class of the status code.
func (s StatusCode) Severity() Severity { return Severity((s >> 30) & 0x3) }

// IsGood reports whether the code is in the Good class.
func (s StatusCode) IsGood() bool { return s.Severity() == SeverityGood }

// IsUncertain reports whether the code is in the Uncertain class.
func (s StatusCode) IsUncertain() bool { return s.Severity() == SeverityUncertain }

// IsBad reports whether the code is in the Bad class.
func (s StatusCode) IsBad() bool { return s.Severity() == SeverityBad }

// String returns a human-readable form.
func (s StatusCode) String() string {
	switch s {
	case StatusGood:
		return "Good"
	case StatusUncertain:
		return "Uncertain"
	case StatusBad:
		return "Bad"
	case StatusBadUnexpectedError:
		return "BadUnexpectedError"
	case StatusBadInternalError:
		return "BadInternalError"
	case StatusBadOutOfMemory:
		return "BadOutOfMemory"
	case StatusBadCommunicationError:
		return "BadCommunicationError"
	case StatusBadTimeout:
		return "BadTimeout"
	}
	return fmt.Sprintf("StatusCode(0x%08X)", uint32(s))
}

// ---------------------------------------------------------------------
// NodeId
// ---------------------------------------------------------------------

// NodeIdType identifies which identifier field of a NodeId carries the
// value.
type NodeIdType uint8

// NodeId identifier kinds.
const (
	NodeIDNumeric NodeIdType = iota
	NodeIDString
	NodeIDGuid
	NodeIDByteString
)

// NodeId is an OPC UA node identifier (OPC UA Part 6 §5.2.2.9, Table 5).
// The encoder picks the shortest legal wire encoding:
//
//	ns=0, numeric<=255             -> two-byte form (0x00 + UInt8 id)
//	ns<=65535, numeric<=65535      -> four-byte form (0x01 + UInt16 ns + UInt16 id;
//	                                  the ns field is always carried, even when 0)
//	ns=0, numeric>65535            -> numeric form (0x02 + UInt32 id; ns implicit 0,
//	                                  no ns bytes on the wire)
//	ns>65535, or ns!=0 and         -> extended numeric form
//	numeric>65535                     (0x80|0x02 + UInt32 ns + UInt32 id)
//	string/guid/bytestring         -> 0x03/0x04/0x05 + UInt16 ns + payload,
//	                                  or 0x80|type + UInt32 ns + payload
type NodeId struct {
	Namespace uint32
	Type      NodeIdType
	Numeric   uint32
	Str       string
	Guid      Guid
	Bytes     []byte
}

// NewNodeID returns a numeric NodeId.
func NewNodeID(namespace uint32, id uint32) NodeId {
	return NodeId{Namespace: namespace, Type: NodeIDNumeric, Numeric: id}
}

// NewStringNodeID returns a string NodeId.
func NewStringNodeID(namespace uint32, id string) NodeId {
	return NodeId{Namespace: namespace, Type: NodeIDString, Str: id}
}

// NewGuidNodeID returns a Guid NodeId.
func NewGuidNodeID(namespace uint32, id Guid) NodeId {
	return NodeId{Namespace: namespace, Type: NodeIDGuid, Guid: id}
}

// NewByteStringNodeID returns a ByteString NodeId.
func NewByteStringNodeID(namespace uint32, id []byte) NodeId {
	return NodeId{Namespace: namespace, Type: NodeIDByteString, Bytes: id}
}

// String renders the NodeId in the conventional
// "ns=<namespace>;i=<numeric>" / ";s=" / ";g=" / ";b=" notation.
func (n NodeId) String() string {
	switch n.Type {
	case NodeIDNumeric:
		return fmt.Sprintf("ns=%d;i=%d", n.Namespace, n.Numeric)
	case NodeIDString:
		return fmt.Sprintf("ns=%d;s=%s", n.Namespace, n.Str)
	case NodeIDGuid:
		return fmt.Sprintf("ns=%d;g=%s", n.Namespace, n.Guid.String())
	case NodeIDByteString:
		return fmt.Sprintf("ns=%d;b=%X", n.Namespace, n.Bytes)
	}
	return fmt.Sprintf("ns=%d;type=%d", n.Namespace, n.Type)
}

// NodeId encoding byte values (OPC UA Part 6 §5.2.2.9).
const (
	nodeIDTwoByte      byte = 0x00
	nodeIDFourByte     byte = 0x01
	nodeIDNumeric      byte = 0x02
	nodeIDString       byte = 0x03
	nodeIDGuid         byte = 0x04
	nodeIDByteString   byte = 0x05
	nodeIDExtendedFlag byte = 0x80
)

func (n NodeId) encodeUA(e *encoder) error {
	switch n.Type {
	case NodeIDNumeric:
		if n.Namespace == 0 && n.Numeric <= 0xFF {
			e.u8(nodeIDTwoByte)
			e.u8(byte(n.Numeric))
			return nil
		}
		if n.Namespace <= 0xFFFF && n.Numeric <= 0xFFFF {
			// Four-byte form: 0x01 + UInt16 namespace + UInt16
			// identifier. Both fields are always carried, even
			// when the namespace index is 0 (OPC UA Part 6
			// §5.2.2.9, Table 5).
			e.u8(nodeIDFourByte)
			e.u16(uint16(n.Namespace))
			e.u16(uint16(n.Numeric))
			return nil
		}
		if n.Namespace == 0 {
			// Numeric form: 0x02 + UInt32 identifier. The
			// namespace index is implicitly 0 and carries no
			// bytes on the wire (OPC UA Part 6 §5.2.2.9).
			e.u8(nodeIDNumeric)
			e.u32(n.Numeric)
			return nil
		}
		// Extended numeric form: 0x80|0x02 + UInt32 namespace +
		// UInt32 identifier.
		e.u8(nodeIDExtendedFlag | nodeIDNumeric)
		e.u32(n.Namespace)
		e.u32(n.Numeric)
		return nil
	case NodeIDString:
		if n.Namespace <= 0xFFFF {
			e.u8(nodeIDString)
			e.u16(uint16(n.Namespace))
			e.str(n.Str)
			return nil
		}
		e.u8(nodeIDExtendedFlag | nodeIDString)
		e.u32(n.Namespace)
		e.str(n.Str)
		return nil
	case NodeIDGuid:
		if n.Namespace <= 0xFFFF {
			e.u8(nodeIDGuid)
			e.u16(uint16(n.Namespace))
			e.guid(n.Guid)
			return nil
		}
		e.u8(nodeIDExtendedFlag | nodeIDGuid)
		e.u32(n.Namespace)
		e.guid(n.Guid)
		return nil
	case NodeIDByteString:
		if n.Namespace <= 0xFFFF {
			e.u8(nodeIDByteString)
			e.u16(uint16(n.Namespace))
			e.bytes(n.Bytes)
			return nil
		}
		e.u8(nodeIDExtendedFlag | nodeIDByteString)
		e.u32(n.Namespace)
		e.bytes(n.Bytes)
		return nil
	}
	return fmt.Errorf("%w: NodeId type %d", ErrInvalidEncoding, n.Type)
}

func decodeNodeID(d *decoder) (NodeId, error) {
	enc, err := d.u8()
	if err != nil {
		return NodeId{}, err
	}
	extended := enc&nodeIDExtendedFlag != 0
	switch enc & 0x7F {
	case nodeIDTwoByte:
		// The two-byte and four-byte forms have no extended
		// namespace variant: 0x80|0x00 and 0x80|0x01 are not
		// defined by the spec and must be rejected.
		if extended {
			return NodeId{}, fmt.Errorf("%w: extended two-byte NodeId", ErrInvalidEncoding)
		}
		v, err := d.u8()
		if err != nil {
			return NodeId{}, err
		}
		return NodeId{Type: NodeIDNumeric, Numeric: uint32(v)}, nil
	case nodeIDFourByte:
		// Four-byte form: 0x01 + UInt16 namespace + UInt16
		// identifier; both are carried on the wire, even when the
		// namespace index is 0 (OPC UA Part 6 §5.2.2.9, Table 5).
		// The extended variant 0x80|0x01 is not defined by the
		// spec and must be rejected.
		if extended {
			return NodeId{}, fmt.Errorf("%w: extended four-byte NodeId", ErrInvalidEncoding)
		}
		ns, err := d.u16()
		if err != nil {
			return NodeId{}, err
		}
		v, err := d.u16()
		if err != nil {
			return NodeId{}, err
		}
		return NodeId{Namespace: uint32(ns), Type: NodeIDNumeric, Numeric: uint32(v)}, nil
	case nodeIDNumeric:
		if extended {
			// Extended numeric: 0x80|0x02 + UInt32 namespace +
			// UInt32 identifier.
			ns, err := d.u32()
			if err != nil {
				return NodeId{}, err
			}
			v, err := d.u32()
			if err != nil {
				return NodeId{}, err
			}
			return NodeId{Namespace: ns, Type: NodeIDNumeric, Numeric: v}, nil
		}
		// Numeric form: 0x02 + UInt32 identifier. The namespace
		// index is implicitly 0 and carries no bytes on the wire.
		v, err := d.u32()
		if err != nil {
			return NodeId{}, err
		}
		return NodeId{Type: NodeIDNumeric, Numeric: v}, nil
	case nodeIDString:
		ns, err := d.namespaceIndex(extended)
		if err != nil {
			return NodeId{}, err
		}
		s, err := d.str()
		if err != nil {
			return NodeId{}, err
		}
		return NodeId{Namespace: ns, Type: NodeIDString, Str: s}, nil
	case nodeIDGuid:
		ns, err := d.namespaceIndex(extended)
		if err != nil {
			return NodeId{}, err
		}
		g, err := d.guid()
		if err != nil {
			return NodeId{}, err
		}
		return NodeId{Namespace: ns, Type: NodeIDGuid, Guid: g}, nil
	case nodeIDByteString:
		ns, err := d.namespaceIndex(extended)
		if err != nil {
			return NodeId{}, err
		}
		b, err := d.bytes()
		if err != nil {
			return NodeId{}, err
		}
		return NodeId{Namespace: ns, Type: NodeIDByteString, Bytes: b}, nil
	}
	return NodeId{}, fmt.Errorf("%w: NodeId encoding byte 0x%02X", ErrInvalidEncoding, enc)
}

// namespaceIndex decodes the namespace index that follows the NodeId
// encoding byte: 2 bytes normally, 4 bytes (UInt32) in the extended
// form.
func (d *decoder) namespaceIndex(extended bool) (uint32, error) {
	if extended {
		return d.u32()
	}
	ns, err := d.u16()
	return uint32(ns), err
}

// ---------------------------------------------------------------------
// QualifiedName
// ---------------------------------------------------------------------

// QualifiedName is a name qualified by a namespace index
// (OPC UA Part 6 §5.2.2.7): UInt16 namespace + String name.
type QualifiedName struct {
	Namespace uint16
	Name      string
}

func (q QualifiedName) encodeUA(e *encoder) {
	e.u16(q.Namespace)
	e.str(q.Name)
}

func decodeQualifiedName(d *decoder) (QualifiedName, error) {
	var q QualifiedName
	var err error
	if q.Namespace, err = d.u16(); err != nil {
		return q, err
	}
	if q.Name, err = d.str(); err != nil {
		return q, err
	}
	return q, nil
}

// ---------------------------------------------------------------------
// LocalizedText
// ---------------------------------------------------------------------

// LocalizedText is a locale-tagged human-readable string
// (OPC UA Part 6 §5.2.2.8). The encoding mask bit 0 marks a present
// Locale, bit 1 a present Text; absent fields stay empty.
type LocalizedText struct {
	Locale    string
	Text      string
	HasLocale bool
	HasText   bool
}

// NewLocalizedText returns a LocalizedText with only Text set.
func NewLocalizedText(text string) LocalizedText {
	return LocalizedText{Text: text, HasText: true}
}

// NewLocalizedTextWithLocale returns a LocalizedText with Locale and
// Text set.
func NewLocalizedTextWithLocale(locale, text string) LocalizedText {
	return LocalizedText{Locale: locale, Text: text, HasLocale: true, HasText: true}
}

const (
	localizedTextLocaleMask byte = 0x01
	localizedTextTextMask   byte = 0x02
)

func (t LocalizedText) encodeUA(e *encoder) {
	var mask byte
	if t.HasLocale {
		mask |= localizedTextLocaleMask
	}
	if t.HasText {
		mask |= localizedTextTextMask
	}
	e.u8(mask)
	if t.HasLocale {
		e.str(t.Locale)
	}
	if t.HasText {
		e.str(t.Text)
	}
}

func decodeLocalizedText(d *decoder) (LocalizedText, error) {
	mask, err := d.u8()
	if err != nil {
		return LocalizedText{}, err
	}
	if mask&^byte(localizedTextLocaleMask|localizedTextTextMask) != 0 {
		return LocalizedText{}, fmt.Errorf("%w: LocalizedText mask 0x%02X with reserved bits", ErrInvalidEncoding, mask)
	}
	var t LocalizedText
	t.HasLocale = mask&localizedTextLocaleMask != 0
	t.HasText = mask&localizedTextTextMask != 0
	if t.HasLocale {
		if t.Locale, err = d.str(); err != nil {
			return LocalizedText{}, err
		}
	}
	if t.HasText {
		if t.Text, err = d.str(); err != nil {
			return LocalizedText{}, err
		}
	}
	return t, nil
}

// ---------------------------------------------------------------------
// ExtensionObject
// ---------------------------------------------------------------------

// ExtensionObject wraps a data-type-specific body
// (OPC UA Part 6 §5.2.2.17): an ExpandedNodeId TypeId followed by an
// encoding byte and an optional body.
//
// This milestone encodes the TypeId as a plain NodeId plus the
// ExpandedNodeId "no extra fields" mask byte (0x00); namespace-URI and
// server-index variants are rejected with ErrUnsupportedType.
type ExtensionObject struct {
	TypeId   NodeId
	Encoding byte // 0 = no body, 1 = ByteString body, 2 = XmlElement body
	Body     []byte
}

// ExtensionObject encoding values.
const (
	ExtensionObjectEncodingNone       byte = 0x00
	ExtensionObjectEncodingByteString byte = 0x01
	ExtensionObjectEncodingXmlElement byte = 0x02
)

func (x ExtensionObject) encodeUA(e *encoder) error {
	if err := x.TypeId.encodeUA(e); err != nil {
		return err
	}
	e.u8(0x00) // ExpandedNodeId mask: no namespace URI, no server index
	e.u8(x.Encoding)
	switch x.Encoding {
	case ExtensionObjectEncodingNone:
		return nil
	case ExtensionObjectEncodingByteString:
		e.bytes(x.Body)
		return nil
	case ExtensionObjectEncodingXmlElement:
		e.str(string(x.Body))
		return nil
	}
	return fmt.Errorf("%w: ExtensionObject encoding %d", ErrInvalidEncoding, x.Encoding)
}

func decodeExtensionObject(d *decoder) (ExtensionObject, error) {
	var x ExtensionObject
	var err error
	if x.TypeId, err = decodeNodeID(d); err != nil {
		return x, err
	}
	expMask, err := d.u8()
	if err != nil {
		return x, err
	}
	if expMask != 0 {
		return x, fmt.Errorf("%w: ExpandedNodeId namespace URI / server index (mask 0x%02X)", ErrUnsupportedType, expMask)
	}
	if x.Encoding, err = d.u8(); err != nil {
		return x, err
	}
	switch x.Encoding {
	case ExtensionObjectEncodingNone:
		return x, nil
	case ExtensionObjectEncodingByteString:
		if x.Body, err = d.bytes(); err != nil {
			return x, err
		}
		return x, nil
	case ExtensionObjectEncodingXmlElement:
		var s string
		if s, err = d.str(); err != nil {
			return x, err
		}
		x.Body = []byte(s)
		return x, nil
	}
	return x, fmt.Errorf("%w: ExtensionObject encoding %d", ErrInvalidEncoding, x.Encoding)
}

// ---------------------------------------------------------------------
// Variant
// ---------------------------------------------------------------------

// UA built-in type ids used by the Variant encoding mask
// (OPC UA Part 6 §5.1.2 and §5.2.2.16).
const (
	VariantNull            byte = 0x00
	VariantBoolean         byte = 0x01
	VariantSByte           byte = 0x02
	VariantByte            byte = 0x03
	VariantInt16           byte = 0x04
	VariantUInt16          byte = 0x05
	VariantInt32           byte = 0x06
	VariantUInt32          byte = 0x07
	VariantInt64           byte = 0x08
	VariantUInt64          byte = 0x09
	VariantFloat           byte = 0x0A
	VariantDouble          byte = 0x0B
	VariantString          byte = 0x0C
	VariantDateTime        byte = 0x0D
	VariantGuid            byte = 0x0E
	VariantByteString      byte = 0x0F
	VariantXmlElement      byte = 0x10 // decoded: ErrUnsupportedType
	VariantNodeId          byte = 0x11
	VariantExpandedNodeId  byte = 0x12 // decoded: ErrUnsupportedType
	VariantStatusCode      byte = 0x13
	VariantQualifiedName   byte = 0x14
	VariantLocalizedText   byte = 0x15
	VariantExtensionObject byte = 0x16
	VariantDataValue       byte = 0x17
	VariantVariant         byte = 0x18
	VariantDiagnosticInfo  byte = 0x19 // decoded: ErrUnsupportedType

	// VariantArray is the mask bit marking an array value (bit 7).
	VariantArray byte = 0x80
	// VariantDimensions is the mask bit marking array dimensions
	// (bit 6). Dimensions are decoded and re-emitted on encode.
	VariantDimensions byte = 0x40
)

// Variant is a UA Variant (OPC UA Part 6 §5.2.2.16): a type mask byte
// followed by a scalar or array of a built-in type. Value holds the
// natural Go representation; Mask must be consistent with Value (the
// NewVariant constructor infers it). A nil Value is the Null variant.
//
// Supported built-in types (scalar and slice forms): Null, Boolean,
// SByte, Byte, Int16, UInt16, Int32, UInt32, Int64, UInt64, Float,
// Double, String, DateTime, Guid, ByteString, NodeId, StatusCode,
// QualifiedName, LocalizedText, ExtensionObject, DataValue, Variant.
// XmlElement, ExpandedNodeId and DiagnosticInfo decode with
// ErrUnsupportedType.
type Variant struct {
	Mask byte
	// Value is one of the scalar Go types listed above, a slice of
	// one of them (array variant), or nil (Null variant).
	Value any
	// Dimensions holds the array dimensions carried by mask bit
	// 0x40; nil when absent. Decoded dimensions are preserved and
	// re-emitted on encode.
	Dimensions []int32
}

// NullVariant returns the Null variant.
func NullVariant() Variant { return Variant{Mask: VariantNull, Value: nil} }

// NewVariant infers the encoding mask from the Go type of v and
// returns the corresponding Variant. A nil slice encodes as a null
// array (length -1); an empty non-nil slice as an empty array.
func NewVariant(v any) (Variant, error) {
	m, err := variantMaskOf(v)
	if err != nil {
		return Variant{}, err
	}
	return Variant{Mask: m, Value: v}, nil
}

// TypeID returns the built-in type id (mask bits 0-5).
func (v Variant) TypeID() byte { return v.Mask & 0x3F }

// IsArray reports whether the variant carries an array value.
func (v Variant) IsArray() bool { return v.Mask&VariantArray != 0 }

// IsNull reports whether the variant is the Null variant.
func (v Variant) IsNull() bool { return v.Mask&0x3F == VariantNull }

func variantMaskOf(v any) (byte, error) {
	switch v.(type) {
	case nil:
		return VariantNull, nil
	case bool:
		return VariantBoolean, nil
	case int8:
		return VariantSByte, nil
	case uint8:
		return VariantByte, nil
	case int16:
		return VariantInt16, nil
	case uint16:
		return VariantUInt16, nil
	case int32:
		return VariantInt32, nil
	case uint32:
		return VariantUInt32, nil
	case int64:
		return VariantInt64, nil
	case uint64:
		return VariantUInt64, nil
	case float32:
		return VariantFloat, nil
	case float64:
		return VariantDouble, nil
	case string:
		return VariantString, nil
	case DateTime:
		return VariantDateTime, nil
	case Guid:
		return VariantGuid, nil
	case []byte:
		return VariantByteString, nil
	case NodeId:
		return VariantNodeId, nil
	case StatusCode:
		return VariantStatusCode, nil
	case QualifiedName:
		return VariantQualifiedName, nil
	case LocalizedText:
		return VariantLocalizedText, nil
	case ExtensionObject:
		return VariantExtensionObject, nil
	case DataValue:
		return VariantDataValue, nil
	case Variant:
		return VariantVariant, nil
	case []bool:
		return VariantArray | VariantBoolean, nil
	case []int8:
		return VariantArray | VariantSByte, nil
	case []int16:
		return VariantArray | VariantInt16, nil
	case []uint16:
		return VariantArray | VariantUInt16, nil
	case []int32:
		return VariantArray | VariantInt32, nil
	case []uint32:
		return VariantArray | VariantUInt32, nil
	case []int64:
		return VariantArray | VariantInt64, nil
	case []uint64:
		return VariantArray | VariantUInt64, nil
	case []float32:
		return VariantArray | VariantFloat, nil
	case []float64:
		return VariantArray | VariantDouble, nil
	case []string:
		return VariantArray | VariantString, nil
	case []DateTime:
		return VariantArray | VariantDateTime, nil
	case []Guid:
		return VariantArray | VariantGuid, nil
	case [][]byte:
		return VariantArray | VariantByteString, nil
	case []NodeId:
		return VariantArray | VariantNodeId, nil
	case []StatusCode:
		return VariantArray | VariantStatusCode, nil
	case []QualifiedName:
		return VariantArray | VariantQualifiedName, nil
	case []LocalizedText:
		return VariantArray | VariantLocalizedText, nil
	case []ExtensionObject:
		return VariantArray | VariantExtensionObject, nil
	case []DataValue:
		return VariantArray | VariantDataValue, nil
	case []Variant:
		return VariantArray | VariantVariant, nil
	}
	return 0, fmt.Errorf("%w: Go type %T", ErrUnsupportedType, v)
}

func (v Variant) encodeUA(e *encoder) error {
	if v.Value == nil {
		e.u8(VariantNull)
		return nil
	}
	e.u8(v.Mask)
	if err := encodeVariantValue(e, v.Mask, v.Value); err != nil {
		return err
	}
	// Array dimensions (mask bit 0x40) are emitted after the value,
	// preserving what was decoded (OPC UA Part 6 §5.2.2.16).
	if v.Mask&VariantDimensions != 0 {
		e.i32(int32(len(v.Dimensions)))
		for _, d := range v.Dimensions {
			e.i32(d)
		}
	}
	return nil
}

// variantTypeError reports a mask/value type mismatch. It is returned
// instead of panicking on a type assertion failure.
func variantTypeError(mask byte, v any) error {
	return fmt.Errorf("%w: Variant mask 0x%02X does not match value of type %T", ErrInvalidEncoding, mask, v)
}

func encodeVariantValue(e *encoder, mask byte, v any) error {
	isArr := mask&VariantArray != 0
	if isArr {
		rv := reflect.ValueOf(v)
		if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
			return fmt.Errorf("%w: array mask with non-slice value %T", ErrInvalidEncoding, v)
		}
		if rv.IsNil() {
			e.i32(-1)
			return nil
		}
		e.i32(int32(rv.Len()))
	}
	switch mask & 0x3F {
	case VariantBoolean:
		if isArr {
			xs, ok := v.([]bool)
			if !ok {
				return variantTypeError(mask, v)
			}
			for _, x := range xs {
				e.bool(x)
			}
		} else {
			x, ok := v.(bool)
			if !ok {
				return variantTypeError(mask, v)
			}
			e.bool(x)
		}
	case VariantSByte:
		if isArr {
			xs, ok := v.([]int8)
			if !ok {
				return variantTypeError(mask, v)
			}
			for _, x := range xs {
				e.i8(x)
			}
		} else {
			x, ok := v.(int8)
			if !ok {
				return variantTypeError(mask, v)
			}
			e.i8(x)
		}
	case VariantByte:
		if isArr {
			xs, ok := v.([]uint8)
			if !ok {
				return variantTypeError(mask, v)
			}
			for _, x := range xs {
				e.u8(x)
			}
		} else {
			x, ok := v.(uint8)
			if !ok {
				return variantTypeError(mask, v)
			}
			e.u8(x)
		}
	case VariantInt16:
		if isArr {
			xs, ok := v.([]int16)
			if !ok {
				return variantTypeError(mask, v)
			}
			for _, x := range xs {
				e.i16(x)
			}
		} else {
			x, ok := v.(int16)
			if !ok {
				return variantTypeError(mask, v)
			}
			e.i16(x)
		}
	case VariantUInt16:
		if isArr {
			xs, ok := v.([]uint16)
			if !ok {
				return variantTypeError(mask, v)
			}
			for _, x := range xs {
				e.u16(x)
			}
		} else {
			x, ok := v.(uint16)
			if !ok {
				return variantTypeError(mask, v)
			}
			e.u16(x)
		}
	case VariantInt32:
		if isArr {
			xs, ok := v.([]int32)
			if !ok {
				return variantTypeError(mask, v)
			}
			for _, x := range xs {
				e.i32(x)
			}
		} else {
			x, ok := v.(int32)
			if !ok {
				return variantTypeError(mask, v)
			}
			e.i32(x)
		}
	case VariantUInt32:
		if isArr {
			xs, ok := v.([]uint32)
			if !ok {
				return variantTypeError(mask, v)
			}
			for _, x := range xs {
				e.u32(x)
			}
		} else {
			x, ok := v.(uint32)
			if !ok {
				return variantTypeError(mask, v)
			}
			e.u32(x)
		}
	case VariantInt64:
		if isArr {
			xs, ok := v.([]int64)
			if !ok {
				return variantTypeError(mask, v)
			}
			for _, x := range xs {
				e.i64(x)
			}
		} else {
			x, ok := v.(int64)
			if !ok {
				return variantTypeError(mask, v)
			}
			e.i64(x)
		}
	case VariantUInt64:
		if isArr {
			xs, ok := v.([]uint64)
			if !ok {
				return variantTypeError(mask, v)
			}
			for _, x := range xs {
				e.u64(x)
			}
		} else {
			x, ok := v.(uint64)
			if !ok {
				return variantTypeError(mask, v)
			}
			e.u64(x)
		}
	case VariantFloat:
		if isArr {
			xs, ok := v.([]float32)
			if !ok {
				return variantTypeError(mask, v)
			}
			for _, x := range xs {
				e.f32(x)
			}
		} else {
			x, ok := v.(float32)
			if !ok {
				return variantTypeError(mask, v)
			}
			e.f32(x)
		}
	case VariantDouble:
		if isArr {
			xs, ok := v.([]float64)
			if !ok {
				return variantTypeError(mask, v)
			}
			for _, x := range xs {
				e.f64(x)
			}
		} else {
			x, ok := v.(float64)
			if !ok {
				return variantTypeError(mask, v)
			}
			e.f64(x)
		}
	case VariantString:
		if isArr {
			xs, ok := v.([]string)
			if !ok {
				return variantTypeError(mask, v)
			}
			for _, x := range xs {
				e.str(x)
			}
		} else {
			x, ok := v.(string)
			if !ok {
				return variantTypeError(mask, v)
			}
			e.str(x)
		}
	case VariantDateTime:
		if isArr {
			xs, ok := v.([]DateTime)
			if !ok {
				return variantTypeError(mask, v)
			}
			for _, x := range xs {
				e.i64(int64(x))
			}
		} else {
			x, ok := v.(DateTime)
			if !ok {
				return variantTypeError(mask, v)
			}
			e.i64(int64(x))
		}
	case VariantGuid:
		if isArr {
			xs, ok := v.([]Guid)
			if !ok {
				return variantTypeError(mask, v)
			}
			for _, x := range xs {
				e.guid(x)
			}
		} else {
			x, ok := v.(Guid)
			if !ok {
				return variantTypeError(mask, v)
			}
			e.guid(x)
		}
	case VariantByteString:
		if isArr {
			xs, ok := v.([][]byte)
			if !ok {
				return variantTypeError(mask, v)
			}
			for _, x := range xs {
				e.bytes(x)
			}
		} else {
			x, ok := v.([]byte)
			if !ok {
				return variantTypeError(mask, v)
			}
			e.bytes(x)
		}
	case VariantNodeId:
		if isArr {
			xs, ok := v.([]NodeId)
			if !ok {
				return variantTypeError(mask, v)
			}
			for _, x := range xs {
				if err := x.encodeUA(e); err != nil {
					return err
				}
			}
		} else {
			x, ok := v.(NodeId)
			if !ok {
				return variantTypeError(mask, v)
			}
			if err := x.encodeUA(e); err != nil {
				return err
			}
		}
	case VariantStatusCode:
		if isArr {
			xs, ok := v.([]StatusCode)
			if !ok {
				return variantTypeError(mask, v)
			}
			for _, x := range xs {
				e.u32(uint32(x))
			}
		} else {
			x, ok := v.(StatusCode)
			if !ok {
				return variantTypeError(mask, v)
			}
			e.u32(uint32(x))
		}
	case VariantQualifiedName:
		if isArr {
			xs, ok := v.([]QualifiedName)
			if !ok {
				return variantTypeError(mask, v)
			}
			for _, x := range xs {
				x.encodeUA(e)
			}
		} else {
			x, ok := v.(QualifiedName)
			if !ok {
				return variantTypeError(mask, v)
			}
			x.encodeUA(e)
		}
	case VariantLocalizedText:
		if isArr {
			xs, ok := v.([]LocalizedText)
			if !ok {
				return variantTypeError(mask, v)
			}
			for _, x := range xs {
				x.encodeUA(e)
			}
		} else {
			x, ok := v.(LocalizedText)
			if !ok {
				return variantTypeError(mask, v)
			}
			x.encodeUA(e)
		}
	case VariantExtensionObject:
		if isArr {
			xs, ok := v.([]ExtensionObject)
			if !ok {
				return variantTypeError(mask, v)
			}
			for _, x := range xs {
				if err := x.encodeUA(e); err != nil {
					return err
				}
			}
		} else {
			x, ok := v.(ExtensionObject)
			if !ok {
				return variantTypeError(mask, v)
			}
			if err := x.encodeUA(e); err != nil {
				return err
			}
		}
	case VariantDataValue:
		if isArr {
			xs, ok := v.([]DataValue)
			if !ok {
				return variantTypeError(mask, v)
			}
			for _, x := range xs {
				if err := x.encodeUA(e); err != nil {
					return err
				}
			}
		} else {
			x, ok := v.(DataValue)
			if !ok {
				return variantTypeError(mask, v)
			}
			if err := x.encodeUA(e); err != nil {
				return err
			}
		}
	case VariantVariant:
		if isArr {
			xs, ok := v.([]Variant)
			if !ok {
				return variantTypeError(mask, v)
			}
			for _, x := range xs {
				if err := x.encodeUA(e); err != nil {
					return err
				}
			}
		} else {
			x, ok := v.(Variant)
			if !ok {
				return variantTypeError(mask, v)
			}
			if err := x.encodeUA(e); err != nil {
				return err
			}
		}
	case VariantNull:
		return fmt.Errorf("%w: non-nil value with Null mask", ErrInvalidEncoding)
	default:
		return fmt.Errorf("%w: Variant type id %d", ErrUnsupportedType, mask&0x3F)
	}
	return nil
}

func decodeVariant(d *decoder) (Variant, error) {
	mask, err := d.u8()
	if err != nil {
		return Variant{}, err
	}
	id := mask & 0x3F
	if id == VariantNull {
		return Variant{Mask: mask, Value: nil}, nil
	}
	var val any
	if mask&VariantArray != 0 {
		n, err := d.i32()
		if err != nil {
			return Variant{}, err
		}
		if n < 0 {
			if n != -1 {
				return Variant{}, fmt.Errorf("%w: negative array length %d", ErrInvalidEncoding, n)
			}
			val = nilSliceFor(id)
		} else {
			if n > 1<<24 {
				return Variant{}, fmt.Errorf("%w: %d array elements", ErrTooLong, n)
			}
			if val, err = decodeArrayValue(d, id, int(n)); err != nil {
				return Variant{}, err
			}
		}
	} else {
		if val, err = decodeBuiltin(d, id); err != nil {
			return Variant{}, err
		}
	}
	var dims []int32
	if mask&VariantDimensions != 0 {
		n, err := d.i32()
		if err != nil {
			return Variant{}, err
		}
		if n < 0 || n > 1024 {
			return Variant{}, fmt.Errorf("%w: %d array dimensions", ErrTooLong, n)
		}
		dims = make([]int32, n)
		for i := range dims {
			if dims[i], err = d.i32(); err != nil {
				return Variant{}, err
			}
		}
	}
	return Variant{Mask: mask, Value: val, Dimensions: dims}, nil
}

// decodeBuiltin decodes a single value of the built-in type id.
func decodeBuiltin(d *decoder, id byte) (any, error) {
	switch id {
	case VariantBoolean:
		return d.bool()
	case VariantSByte:
		return d.i8()
	case VariantByte:
		return d.u8()
	case VariantInt16:
		return d.i16()
	case VariantUInt16:
		return d.u16()
	case VariantInt32:
		return d.i32()
	case VariantUInt32:
		return d.u32()
	case VariantInt64:
		return d.i64()
	case VariantUInt64:
		return d.u64()
	case VariantFloat:
		return d.f32()
	case VariantDouble:
		return d.f64()
	case VariantString:
		return d.str()
	case VariantDateTime:
		v, err := d.i64()
		return DateTime(v), err
	case VariantGuid:
		return d.guid()
	case VariantByteString:
		return d.bytes()
	case VariantNodeId:
		return decodeNodeID(d)
	case VariantStatusCode:
		v, err := d.u32()
		return StatusCode(v), err
	case VariantQualifiedName:
		return decodeQualifiedName(d)
	case VariantLocalizedText:
		return decodeLocalizedText(d)
	case VariantExtensionObject:
		return decodeExtensionObject(d)
	case VariantDataValue:
		return decodeDataValue(d)
	case VariantVariant:
		return decodeVariant(d)
	case VariantXmlElement, VariantExpandedNodeId, VariantDiagnosticInfo:
		return nil, fmt.Errorf("%w: Variant type id %d", ErrUnsupportedType, id)
	}
	return nil, fmt.Errorf("%w: Variant type id %d", ErrInvalidEncoding, id)
}

// nilSliceFor returns the typed nil slice representing a null array of
// the built-in type id.
func nilSliceFor(id byte) any {
	switch id {
	case VariantBoolean:
		return []bool(nil)
	case VariantSByte:
		return []int8(nil)
	case VariantByte:
		return []uint8(nil)
	case VariantInt16:
		return []int16(nil)
	case VariantUInt16:
		return []uint16(nil)
	case VariantInt32:
		return []int32(nil)
	case VariantUInt32:
		return []uint32(nil)
	case VariantInt64:
		return []int64(nil)
	case VariantUInt64:
		return []uint64(nil)
	case VariantFloat:
		return []float32(nil)
	case VariantDouble:
		return []float64(nil)
	case VariantString:
		return []string(nil)
	case VariantDateTime:
		return []DateTime(nil)
	case VariantGuid:
		return []Guid(nil)
	case VariantByteString:
		return [][]byte(nil)
	case VariantNodeId:
		return []NodeId(nil)
	case VariantStatusCode:
		return []StatusCode(nil)
	case VariantQualifiedName:
		return []QualifiedName(nil)
	case VariantLocalizedText:
		return []LocalizedText(nil)
	case VariantExtensionObject:
		return []ExtensionObject(nil)
	case VariantDataValue:
		return []DataValue(nil)
	case VariantVariant:
		return []Variant(nil)
	}
	return nil
}

// decodeArrayValue decodes n elements of the built-in type id into a
// typed slice.
func decodeArrayValue(d *decoder, id byte, n int) (any, error) {
	switch id {
	case VariantBoolean:
		return decodeArrayOf[bool](d, id, n)
	case VariantSByte:
		return decodeArrayOf[int8](d, id, n)
	case VariantByte:
		return decodeArrayOf[uint8](d, id, n)
	case VariantInt16:
		return decodeArrayOf[int16](d, id, n)
	case VariantUInt16:
		return decodeArrayOf[uint16](d, id, n)
	case VariantInt32:
		return decodeArrayOf[int32](d, id, n)
	case VariantUInt32:
		return decodeArrayOf[uint32](d, id, n)
	case VariantInt64:
		return decodeArrayOf[int64](d, id, n)
	case VariantUInt64:
		return decodeArrayOf[uint64](d, id, n)
	case VariantFloat:
		return decodeArrayOf[float32](d, id, n)
	case VariantDouble:
		return decodeArrayOf[float64](d, id, n)
	case VariantString:
		return decodeArrayOf[string](d, id, n)
	case VariantDateTime:
		return decodeArrayOf[DateTime](d, id, n)
	case VariantGuid:
		return decodeArrayOf[Guid](d, id, n)
	case VariantByteString:
		return decodeArrayOf[[]byte](d, id, n)
	case VariantNodeId:
		return decodeArrayOf[NodeId](d, id, n)
	case VariantStatusCode:
		return decodeArrayOf[StatusCode](d, id, n)
	case VariantQualifiedName:
		return decodeArrayOf[QualifiedName](d, id, n)
	case VariantLocalizedText:
		return decodeArrayOf[LocalizedText](d, id, n)
	case VariantExtensionObject:
		return decodeArrayOf[ExtensionObject](d, id, n)
	case VariantDataValue:
		return decodeArrayOf[DataValue](d, id, n)
	case VariantVariant:
		return decodeArrayOf[Variant](d, id, n)
	}
	return nil, fmt.Errorf("%w: Variant type id %d", ErrInvalidEncoding, id)
}

func decodeArrayOf[T any](d *decoder, id byte, n int) ([]T, error) {
	out := make([]T, n)
	for i := range out {
		v, err := decodeBuiltin(d, id)
		if err != nil {
			return nil, err
		}
		out[i] = v.(T)
	}
	return out, nil
}

// ---------------------------------------------------------------------
// DataValue
// ---------------------------------------------------------------------

// DataValue is a value with optional status and timestamps
// (OPC UA Part 6 §5.2.2.18). Present fields are tracked by the
// encoding mask; absent fields are represented by nil pointers.
// Picosecond fields are only legal together with their timestamp.
type DataValue struct {
	Value             *Variant
	Status            *StatusCode
	SourceTimestamp   *DateTime
	ServerTimestamp   *DateTime
	SourcePicoseconds *uint16
	ServerPicoseconds *uint16
}

// DataValue encoding mask bits.
const (
	dataValueValueMask      byte = 0x01
	dataValueStatusMask     byte = 0x02
	dataValueSourceTimeMask byte = 0x04
	dataValueServerTimeMask byte = 0x08
	dataValueSourcePicoMask byte = 0x10
	dataValueServerPicoMask byte = 0x20
)

func (v DataValue) encodeUA(e *encoder) error {
	if v.SourcePicoseconds != nil && v.SourceTimestamp == nil {
		return fmt.Errorf("%w: SourcePicoseconds without SourceTimestamp", ErrInvalidEncoding)
	}
	if v.ServerPicoseconds != nil && v.ServerTimestamp == nil {
		return fmt.Errorf("%w: ServerPicoseconds without ServerTimestamp", ErrInvalidEncoding)
	}
	var mask byte
	if v.Value != nil {
		mask |= dataValueValueMask
	}
	if v.Status != nil {
		mask |= dataValueStatusMask
	}
	if v.SourceTimestamp != nil {
		mask |= dataValueSourceTimeMask
	}
	if v.ServerTimestamp != nil {
		mask |= dataValueServerTimeMask
	}
	if v.SourcePicoseconds != nil {
		mask |= dataValueSourcePicoMask
	}
	if v.ServerPicoseconds != nil {
		mask |= dataValueServerPicoMask
	}
	e.u8(mask)
	if v.Value != nil {
		if err := v.Value.encodeUA(e); err != nil {
			return err
		}
	}
	if v.Status != nil {
		e.u32(uint32(*v.Status))
	}
	if v.SourceTimestamp != nil {
		e.i64(int64(*v.SourceTimestamp))
	}
	if v.ServerTimestamp != nil {
		e.i64(int64(*v.ServerTimestamp))
	}
	if v.SourcePicoseconds != nil {
		e.u16(*v.SourcePicoseconds)
	}
	if v.ServerPicoseconds != nil {
		e.u16(*v.ServerPicoseconds)
	}
	return nil
}

func decodeDataValue(d *decoder) (DataValue, error) {
	mask, err := d.u8()
	if err != nil {
		return DataValue{}, err
	}
	if mask&^byte(dataValueValueMask|dataValueStatusMask|dataValueSourceTimeMask|dataValueServerTimeMask|dataValueSourcePicoMask|dataValueServerPicoMask) != 0 {
		return DataValue{}, fmt.Errorf("%w: DataValue mask 0x%02X with reserved bits", ErrInvalidEncoding, mask)
	}
	var v DataValue
	if mask&dataValueValueMask != 0 {
		val, err := decodeVariant(d)
		if err != nil {
			return DataValue{}, err
		}
		v.Value = &val
	}
	if mask&dataValueStatusMask != 0 {
		s, err := d.u32()
		if err != nil {
			return DataValue{}, err
		}
		st := StatusCode(s)
		v.Status = &st
	}
	if mask&dataValueSourceTimeMask != 0 {
		ts, err := d.i64()
		if err != nil {
			return DataValue{}, err
		}
		t := DateTime(ts)
		v.SourceTimestamp = &t
	}
	if mask&dataValueServerTimeMask != 0 {
		ts, err := d.i64()
		if err != nil {
			return DataValue{}, err
		}
		t := DateTime(ts)
		v.ServerTimestamp = &t
	}
	if mask&dataValueSourcePicoMask != 0 {
		if v.SourceTimestamp == nil {
			return DataValue{}, fmt.Errorf("%w: SourcePicoseconds without SourceTimestamp", ErrInvalidEncoding)
		}
		p, err := d.u16()
		if err != nil {
			return DataValue{}, err
		}
		v.SourcePicoseconds = &p
	}
	if mask&dataValueServerPicoMask != 0 {
		if v.ServerTimestamp == nil {
			return DataValue{}, fmt.Errorf("%w: ServerPicoseconds without ServerTimestamp", ErrInvalidEncoding)
		}
		p, err := d.u16()
		if err != nil {
			return DataValue{}, err
		}
		v.ServerPicoseconds = &p
	}
	return v, nil
}

// ---------------------------------------------------------------------
// DiagnosticInfo 的完整实现见 diagnostic.go（v0.14.0 补全位域语义；
// Variant 内嵌 0x19 仍不支持，见 decodeBuiltin）。
// ---------------------------------------------------------------------
