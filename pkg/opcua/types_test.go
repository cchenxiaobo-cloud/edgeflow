package opcua

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

// encodeBytes encodes v through its encodeUA method.
func encodeBytes(t *testing.T, enc func(e *encoder) error) []byte {
	t.Helper()
	var e encoder
	if err := enc(&e); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return e.buf
}

// decodeInto decodes a value of the given kind from b.
func decodeNodeIDBytes(t *testing.T, b []byte) (NodeId, error) {
	t.Helper()
	var d decoder
	d.b = b
	return decodeNodeID(&d)
}

func TestNodeIDRoundTrip(t *testing.T) {
	g := Guid{0x00112233, 0x4455, 0x6677, [8]byte{0x88, 0x99, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}}
	cases := []struct {
		name string
		id   NodeId
	}{
		{"two-byte-zero", NewNodeID(0, 0)},
		{"two-byte-max", NewNodeID(0, 255)},
		{"four-byte-min", NewNodeID(0, 256)},
		{"four-byte-max", NewNodeID(0, 65535)},
		{"numeric-ns0-65536", NewNodeID(0, 65536)},
		{"numeric-ns0-max", NewNodeID(0, 0xFFFFFFFF)},
		{"numeric-ns", NewNodeID(7, 123456)},
		{"numeric-ns-65535", NewNodeID(65535, 42)},
		{"numeric-extended", NewNodeID(70000, 99)},
		{"numeric-extended-max-ns", NewNodeID(0xFFFFFFFF, 0xFFFFFFFF)},
		{"string", NewStringNodeID(3, "Temperature")},
		{"string-empty", NewStringNodeID(1, "")},
		{"string-utf8", NewStringNodeID(2, "温度传感器-α")},
		{"string-extended", NewStringNodeID(100000, "x")},
		{"guid", NewGuidNodeID(2, g)},
		{"guid-extended", NewGuidNodeID(65536+5, g)},
		{"bytestring", NewByteStringNodeID(1, []byte{0xDE, 0xAD, 0xBE, 0xEF})},
		{"bytestring-empty", NewByteStringNodeID(0, []byte{})},
		{"bytestring-extended", NewByteStringNodeID(200000, []byte{9})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := encodeBytes(t, tc.id.encodeUA)
			got, err := decodeNodeIDBytes(t, b)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !reflect.DeepEqual(got, tc.id) {
				t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, tc.id)
			}
		})
	}
}

func TestNodeIDWireEncodings(t *testing.T) {
	cases := []struct {
		name string
		id   NodeId
		want []byte
	}{
		{"two-byte", NewNodeID(0, 7), []byte{0x00, 0x07}},
		// Four-byte form: 0x01 + UInt16 namespace + UInt16
		// identifier; the ns field is always carried (Part 6
		// §5.2.2.9, Table 5).
		{"four-byte", NewNodeID(0, 256), []byte{0x01, 0x00, 0x00, 0x01, 0x00}},
		{"four-byte-max", NewNodeID(0, 65535), []byte{0x01, 0x00, 0x00, 0xFF, 0xFF}},
		{"four-byte-ns1", NewNodeID(1, 0), []byte{0x01, 0x00, 0x01, 0x00, 0x00}},
		// ns=0 with id > 65535 uses the Numeric form (0x02 + UInt32
		// id only; no ns bytes on the wire).
		{"numeric-ns0-large", NewNodeID(0, 65536), []byte{0x02, 0x00, 0x01, 0x00, 0x00}},
		// ns!=0 with id > 65535 uses the extended numeric form
		// (0x82 + UInt32 ns + UInt32 id).
		{"numeric", NewNodeID(7, 123456), []byte{0x82, 0x00, 0x00, 0x00, 0x07, 0x00, 0x01, 0xE2, 0x40}},
		{"numeric-extended", NewNodeID(70000, 42), []byte{0x82, 0x00, 0x01, 0x11, 0x70, 0x00, 0x00, 0x00, 0x2A}},
		{"string", NewStringNodeID(1, "abc"), []byte{0x03, 0x00, 0x01, 0x00, 0x00, 0x00, 0x03, 'a', 'b', 'c'}},
		{"string-extended", NewStringNodeID(0x10000, "x"), []byte{0x83, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 'x'}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := encodeBytes(t, tc.id.encodeUA)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("wire mismatch:\n got %X\nwant %X", got, tc.want)
			}
		})
	}
}

func TestNodeIDDecodeErrors(t *testing.T) {
	if _, err := decodeNodeIDBytes(t, []byte{}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short buffer: got %v", err)
	}
	if _, err := decodeNodeIDBytes(t, []byte{0x0B}); !errors.Is(err, ErrInvalidEncoding) {
		t.Fatalf("reserved encoding byte: got %v", err)
	}
	// Extended two-byte form (0x80) is not defined by the spec
	// (0x80|0x00 and 0x80|0x01 must be rejected).
	if _, err := decodeNodeIDBytes(t, []byte{0x80, 0x07}); !errors.Is(err, ErrInvalidEncoding) {
		t.Fatalf("extended two-byte: got %v", err)
	}
	if _, err := decodeNodeIDBytes(t, []byte{0x81, 0x00, 0x01}); !errors.Is(err, ErrInvalidEncoding) {
		t.Fatalf("extended four-byte: got %v", err)
	}
	// Four-byte form: UInt16 namespace + UInt16 identifier, both
	// carried on the wire.
	got, err := decodeNodeIDBytes(t, []byte{0x01, 0x00, 0x00, 0x01, 0x00})
	if err != nil {
		t.Fatalf("four-byte decode: %v", err)
	}
	want := NodeId{Type: NodeIDNumeric, Numeric: 256}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("four-byte: got %+v want %+v", got, want)
	}
	// Four-byte form with a non-zero namespace.
	got, err = decodeNodeIDBytes(t, []byte{0x01, 0x00, 0x01, 0x00, 0x00})
	if err != nil {
		t.Fatalf("four-byte ns=1 decode: %v", err)
	}
	want = NodeId{Namespace: 1, Type: NodeIDNumeric, Numeric: 0}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("four-byte ns=1: got %+v want %+v", got, want)
	}
	// Numeric form (0x02) carries only a UInt32 identifier; the
	// namespace is implicitly 0 and the first two bytes must NOT be
	// read as a namespace index.
	got, err = decodeNodeIDBytes(t, []byte{0x02, 0x00, 0x01, 0x00, 0x00})
	if err != nil {
		t.Fatalf("numeric ns=0 decode: %v", err)
	}
	want = NodeId{Type: NodeIDNumeric, Numeric: 65536}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("numeric ns=0: got %+v want %+v", got, want)
	}
	// Extended numeric form: UInt32 namespace + UInt32 identifier.
	got, err = decodeNodeIDBytes(t, []byte{0x82, 0x00, 0x00, 0x00, 0x07, 0x00, 0x01, 0xE2, 0x40})
	if err != nil {
		t.Fatalf("extended numeric decode: %v", err)
	}
	want = NodeId{Namespace: 7, Type: NodeIDNumeric, Numeric: 123456}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("extended numeric: got %+v want %+v", got, want)
	}
}

func TestNodeIDString(t *testing.T) {
	if got, want := NewNodeID(2, 85).String(), "ns=2;i=85"; got != want {
		t.Fatalf("numeric: got %q want %q", got, want)
	}
	if got, want := NewStringNodeID(3, "x").String(), "ns=3;s=x"; got != want {
		t.Fatalf("string: got %q want %q", got, want)
	}
}

func TestGuidRoundTripAndString(t *testing.T) {
	g := Guid{0x00112233, 0x4455, 0x6677, [8]byte{0x88, 0x99, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}}
	if got, want := g.String(), "00112233-4455-6677-8899-AABBCCDDEEFF"; got != want {
		t.Fatalf("String: got %q want %q", got, want)
	}
	b := encodeBytes(t, func(e *encoder) error { e.guid(g); return nil })
	if len(b) != 16 {
		t.Fatalf("guid encoded to %d bytes, want 16", len(b))
	}
	var d decoder
	d.b = b
	got, err := d.guid()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != g {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, g)
	}
}

func TestStatusCodeSeverity(t *testing.T) {
	cases := []struct {
		code StatusCode
		sev  Severity
		good bool
		bad  bool
	}{
		{StatusGood, SeverityGood, true, false},
		{StatusUncertain, SeverityUncertain, false, false},
		{StatusBad, SeverityBad, false, true},
		{StatusBadTimeout, SeverityBad, false, true},
		{StatusCode(0xC0000000), SeverityReserved, false, false},
	}
	for _, tc := range cases {
		if got := tc.code.Severity(); got != tc.sev {
			t.Errorf("%08X severity: got %d want %d", uint32(tc.code), got, tc.sev)
		}
		if got := tc.code.IsGood(); got != tc.good {
			t.Errorf("%08X IsGood: got %v", uint32(tc.code), got)
		}
		if got := tc.code.IsBad(); got != tc.bad {
			t.Errorf("%08X IsBad: got %v", uint32(tc.code), got)
		}
	}
	if got, want := StatusBadTimeout.String(), "BadTimeout"; got != want {
		t.Fatalf("String: got %q want %q", got, want)
	}
	if got := StatusCode(0x12345678).String(); !strings.HasPrefix(got, "StatusCode(0x") {
		t.Fatalf("unknown code String: %q", got)
	}
}

func TestQualifiedNameRoundTrip(t *testing.T) {
	cases := []QualifiedName{
		{Namespace: 0, Name: ""},
		{Namespace: 2, Name: "Temperature"},
		{Namespace: 65535, Name: "中文名字"},
	}
	for _, q := range cases {
		b := encodeBytes(t, func(e *encoder) error { q.encodeUA(e); return nil })
		var d decoder
		d.b = b
		got, err := decodeQualifiedName(&d)
		if err != nil {
			t.Fatalf("%+v: decode: %v", q, err)
		}
		if !reflect.DeepEqual(got, q) {
			t.Fatalf("round-trip mismatch: got %+v want %+v", got, q)
		}
	}
}

func TestLocalizedTextRoundTrip(t *testing.T) {
	cases := []LocalizedText{
		{},
		{HasLocale: true, Locale: "zh-CN"},
		{Text: "你好", HasText: true},
		{Locale: "en", Text: "Hello", HasLocale: true, HasText: true},
		{HasLocale: true, HasText: true}, // both present but empty
	}
	for _, lt := range cases {
		b := encodeBytes(t, func(e *encoder) error { lt.encodeUA(e); return nil })
		var d decoder
		d.b = b
		got, err := decodeLocalizedText(&d)
		if err != nil {
			t.Fatalf("%+v: decode: %v", lt, err)
		}
		if !reflect.DeepEqual(got, lt) {
			t.Fatalf("round-trip mismatch: got %+v want %+v", got, lt)
		}
	}
}

func TestExtensionObjectRoundTrip(t *testing.T) {
	typeID := NewNodeID(0, 582) // e.g. DataTypeEncoding for a custom struct
	cases := []ExtensionObject{
		{TypeId: typeID, Encoding: ExtensionObjectEncodingNone},
		{TypeId: typeID, Encoding: ExtensionObjectEncodingByteString, Body: []byte{1, 2, 3, 4}},
		{TypeId: typeID, Encoding: ExtensionObjectEncodingByteString, Body: []byte{}},
		{TypeId: typeID, Encoding: ExtensionObjectEncodingByteString, Body: nil}, // null body
		{TypeId: NewStringNodeID(3, "enc"), Encoding: ExtensionObjectEncodingXmlElement, Body: []byte("<a/>")},
	}
	for _, x := range cases {
		b := encodeBytes(t, x.encodeUA)
		var d decoder
		d.b = b
		got, err := decodeExtensionObject(&d)
		if err != nil {
			t.Fatalf("%+v: decode: %v", x, err)
		}
		if !reflect.DeepEqual(got, x) {
			t.Fatalf("round-trip mismatch: got %+v want %+v", got, x)
		}
	}
	// ExpandedNodeId with namespace URI must be rejected.
	bad := encodeBytes(t, func(e *encoder) error {
		if err := typeID.encodeUA(e); err != nil {
			return err
		}
		e.u8(0x01) // namespace URI present
		e.u8(ExtensionObjectEncodingNone)
		return nil
	})
	var d decoder
	d.b = bad
	if _, err := decodeExtensionObject(&d); !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("expanded mask: got %v, want ErrUnsupportedType", err)
	}
}

func TestDateTimeConversion(t *testing.T) {
	cases := []time.Time{
		time.Date(2026, 8, 19, 6, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 19, 6, 0, 0, 123450000, time.UTC),
		time.Date(1601, 1, 1, 0, 0, 0, 0, time.UTC), // epoch
		time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC), // Unix epoch
		time.Date(2100, 12, 31, 23, 59, 59, 999999900, time.UTC),
	}
	for _, tt := range cases {
		dt := DateTimeFromTime(tt)
		got := dt.Time()
		if !got.Equal(tt) {
			t.Fatalf("DateTime %d: got %v want %v", int64(dt), got, tt)
		}
		// Wire round-trip.
		b := encodeBytes(t, func(e *encoder) error { e.i64(int64(dt)); return nil })
		var d decoder
		d.b = b
		v, err := d.i64()
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if v != int64(dt) {
			t.Fatalf("wire mismatch: got %d want %d", v, int64(dt))
		}
	}
}

func TestStringEncodingRules(t *testing.T) {
	// Empty string encodes with length 0.
	b := encodeBytes(t, func(e *encoder) error { e.str(""); return nil })
	if !reflect.DeepEqual(b, []byte{0, 0, 0, 0}) {
		t.Fatalf("empty string: got %X", b)
	}
	var d decoder
	d.b = b
	s, err := d.str()
	if err != nil || s != "" {
		t.Fatalf("empty string decode: %q %v", s, err)
	}

	// Null string (length -1) decodes to "".
	d2 := decoder{b: []byte{0xFF, 0xFF, 0xFF, 0xFF}}
	s, err = d2.str()
	if err != nil || s != "" {
		t.Fatalf("null string decode: %q %v", s, err)
	}
}

func TestStringTruncation(t *testing.T) {
	prefix := strings.Repeat("A", MaxStringLength)
	s := prefix + "TAIL"
	b := encodeBytes(t, func(e *encoder) error { e.str(s); return nil })
	// Append a marker byte after the string to verify stream alignment.
	b = append(b, 0x42)
	var d decoder
	d.b = b
	got, err := d.str()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != MaxStringLength || got != prefix {
		t.Fatalf("truncation: got len=%d want %d", len(got), MaxStringLength)
	}
	marker, err := d.u8()
	if err != nil {
		t.Fatalf("marker: %v", err)
	}
	if marker != 0x42 {
		t.Fatalf("stream misaligned after truncation: marker=%X", marker)
	}

	// ByteString truncates the same way.
	b2 := encodeBytes(t, func(e *encoder) error { e.bytes([]byte(s)); return nil })
	d2 := decoder{b: b2}
	gotB, err := d2.bytes()
	if err != nil {
		t.Fatalf("bytestring decode: %v", err)
	}
	if len(gotB) != MaxStringLength {
		t.Fatalf("bytestring truncation: got len=%d want %d", len(gotB), MaxStringLength)
	}
}

func TestByteStringNullVsEmpty(t *testing.T) {
	var d decoder
	d.b = encodeBytes(t, func(e *encoder) error { e.bytes(nil); return nil })
	b, err := d.bytes()
	if err != nil || b != nil {
		t.Fatalf("null bytestring: %v %v", b, err)
	}
	d2 := decoder{b: encodeBytes(t, func(e *encoder) error { e.bytes([]byte{}); return nil })}
	b, err = d2.bytes()
	if err != nil || b == nil || len(b) != 0 {
		t.Fatalf("empty bytestring: %v %v", b, err)
	}
}

func TestDataValueRoundTrip(t *testing.T) {
	now := DateTimeFromTime(time.Date(2026, 8, 19, 6, 0, 0, 123450000, time.UTC))
	bad := StatusBadTimeout
	pico := uint16(1234)
	val, err := NewVariant(float64(36.5))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		dv   DataValue
	}{
		{"empty", DataValue{}},
		{"value-only", DataValue{Value: &val}},
		{"status-only", DataValue{Status: &bad}},
		{"timestamps", DataValue{SourceTimestamp: &now, ServerTimestamp: &now}},
		{"full", DataValue{Value: &val, Status: &bad, SourceTimestamp: &now, ServerTimestamp: &now}},
		{"picoseconds", DataValue{SourceTimestamp: &now, ServerTimestamp: &now, SourcePicoseconds: &pico, ServerPicoseconds: &pico}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := encodeBytes(t, tc.dv.encodeUA)
			var d decoder
			d.b = b
			got, err := decodeDataValue(&d)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !reflect.DeepEqual(got, tc.dv) {
				t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, tc.dv)
			}
		})
	}
}

func TestDataValuePicoRequiresTimestamp(t *testing.T) {
	pico := uint16(1)
	dv := DataValue{SourcePicoseconds: &pico}
	if err := dv.encodeUA(&encoder{}); !errors.Is(err, ErrInvalidEncoding) {
		t.Fatalf("encode without timestamp: got %v", err)
	}
	// Wire form with picoseconds bit but no timestamp.
	var e encoder
	e.u8(dataValueSourcePicoMask)
	e.u16(1)
	var d decoder
	d.b = e.buf
	if _, err := decodeDataValue(&d); !errors.Is(err, ErrInvalidEncoding) {
		t.Fatalf("decode without timestamp: got %v", err)
	}
}

func variantRoundTrip(t *testing.T, name string, v Variant) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		b := encodeBytes(t, v.encodeUA)
		var d decoder
		d.b = b
		got, err := decodeVariant(&d)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Mask != v.Mask {
			t.Fatalf("mask mismatch: got 0x%02X want 0x%02X", got.Mask, v.Mask)
		}
		if !reflect.DeepEqual(got.Value, v.Value) {
			t.Fatalf("value mismatch:\n got %#v\nwant %#v", got.Value, v.Value)
		}
		if !reflect.DeepEqual(got.Dimensions, v.Dimensions) {
			t.Fatalf("dimensions mismatch: got %v want %v", got.Dimensions, v.Dimensions)
		}
	})
}

func TestVariantScalars(t *testing.T) {
	g := Guid{0x00112233, 0x4455, 0x6677, [8]byte{0x88, 0x99, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}}
	now := DateTimeFromTime(time.Date(2026, 8, 19, 6, 0, 0, 0, time.UTC))
	status := StatusBadTimeout
	qn := QualifiedName{Namespace: 2, Name: "Temperature"}
	lt := NewLocalizedTextWithLocale("zh-CN", "温度")
	ext := ExtensionObject{TypeId: NewNodeID(0, 582), Encoding: ExtensionObjectEncodingByteString, Body: []byte{1, 2, 3}}
	dvVal, _ := NewVariant(int32(42))
	dv := DataValue{Value: &dvVal, Status: &status}
	inner, _ := NewVariant(uint16(7))
	scalars := []struct {
		name string
		v    any
	}{
		{"bool", true},
		{"sbyte", int8(-5)},
		{"byte", uint8(200)},
		{"int16", int16(-1234)},
		{"uint16", uint16(54321)},
		{"int32", int32(-123456)},
		{"uint32", uint32(4000000000)},
		{"int64", int64(-1234567890123)},
		{"uint64", uint64(18446744073709551615)},
		{"float", float32(3.14)},
		{"double", float64(-2.718281828)},
		{"string", "hello 世界"},
		{"string-empty", ""},
		{"datetime", now},
		{"guid", g},
		{"bytestring", []byte{0x01, 0x02}},
		{"bytestring-null", []byte(nil)},
		{"nodeid", NewNodeID(2, 85)},
		{"nodeid-string", NewStringNodeID(3, "abc")},
		{"statuscode", status},
		{"qualifiedname", qn},
		{"localizedtext", lt},
		{"extensionobject", ext},
		{"datavalue", dv},
		{"variant", inner},
	}
	for _, tc := range scalars {
		v, err := NewVariant(tc.v)
		if err != nil {
			t.Fatalf("%s: NewVariant: %v", tc.name, err)
		}
		variantRoundTrip(t, tc.name, v)
	}
	// Null variant.
	variantRoundTrip(t, "null", NullVariant())
}

func TestVariantArrays(t *testing.T) {
	now := DateTimeFromTime(time.Date(2026, 8, 19, 6, 0, 0, 0, time.UTC))
	g := Guid{0x00112233, 0x4455, 0x6677, [8]byte{0x88, 0x99, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}}
	arrays := []struct {
		name string
		v    any
	}{
		{"bool", []bool{true, false, true}},
		{"sbyte", []int8{-1, 0, 1}},
		{"byte", []uint8{0, 255, 128}},
		{"int16", []int16{-32768, 32767}},
		{"uint16", []uint16{0, 65535}},
		{"int32", []int32{-2147483648, 2147483647}},
		{"uint32", []uint32{0, 4294967295}},
		{"int64", []int64{-9223372036854775808, 9223372036854775807}},
		{"uint64", []uint64{0, 18446744073709551615}},
		{"float", []float32{1.5, -2.5}},
		{"double", []float64{1.1, 2.2, 3.3}},
		{"string", []string{"a", "", "中文"}},
		{"datetime", []DateTime{now, now + 1}},
		{"guid", []Guid{g, {}}},
		{"bytestring", [][]byte{{1, 2}, nil, {}}},
		{"nodeid", []NodeId{NewNodeID(0, 1), NewStringNodeID(2, "x")}},
		{"statuscode", []StatusCode{StatusGood, StatusBad}},
		{"qualifiedname", []QualifiedName{{Namespace: 1, Name: "a"}, {Namespace: 2, Name: "b"}}},
		{"localizedtext", []LocalizedText{NewLocalizedText("a"), {}}},
		{"extensionobject", []ExtensionObject{{TypeId: NewNodeID(0, 1), Encoding: ExtensionObjectEncodingNone}}},
		{"datavalue", []DataValue{{}, {Status: &[]StatusCode{StatusGood}[0]}}},
		{"variant", []Variant{mustVariant(t, int32(1)), mustVariant(t, "two")}},
	}
	for _, tc := range arrays {
		v, err := NewVariant(tc.v)
		if err != nil {
			t.Fatalf("%s: NewVariant: %v", tc.name, err)
		}
		variantRoundTrip(t, tc.name, v)
	}
	// Empty and null arrays.
	variantRoundTrip(t, "array-empty", mustVariant(t, []int32{}))
	variantRoundTrip(t, "array-null", mustVariant(t, []int32(nil)))
}

func mustVariant(t *testing.T, v any) Variant {
	t.Helper()
	vv, err := NewVariant(v)
	if err != nil {
		t.Fatalf("NewVariant(%#v): %v", v, err)
	}
	return vv
}

func TestVariantDecodeDimensions(t *testing.T) {
	// Hand-craft: mask 0x46 (Int32 array + dimensions). Per the spec
	// the array length and elements come first, the dimensions last:
	// length=6, 6 int32 elements, then dims=2 [2,3].
	var e encoder
	e.u8(VariantInt32 | VariantDimensions | VariantArray)
	e.i32(6)
	for i := int32(0); i < 6; i++ {
		e.i32(i)
	}
	e.i32(2)
	e.i32(2)
	e.i32(3)
	var d decoder
	d.b = e.buf
	v, err := decodeVariant(&d)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(v.Dimensions, []int32{2, 3}) {
		t.Fatalf("dimensions: got %v", v.Dimensions)
	}
	want := []int32{0, 1, 2, 3, 4, 5}
	if !reflect.DeepEqual(v.Value, want) {
		t.Fatalf("value: got %#v want %#v", v.Value, want)
	}
}

func TestVariantErrors(t *testing.T) {
	// Unsupported Go types.
	for _, v := range []any{map[string]int{}, complex(1, 2), struct{}{}} {
		if _, err := NewVariant(v); !errors.Is(err, ErrUnsupportedType) {
			t.Fatalf("NewVariant(%T): got %v, want ErrUnsupportedType", v, err)
		}
	}
	// Unsupported built-in type ids on decode (XmlElement, DiagnosticInfo).
	for _, id := range []byte{VariantXmlElement, VariantExpandedNodeId, VariantDiagnosticInfo} {
		var d decoder
		d.b = []byte{id}
		if _, err := decodeVariant(&d); !errors.Is(err, ErrUnsupportedType) {
			t.Fatalf("decode id 0x%02X: got %v, want ErrUnsupportedType", id, err)
		}
	}
	// Invalid array length (absurd element count) must be rejected.
	var e encoder
	e.u8(VariantArray | VariantInt32)
	e.i32(1 << 30)
	var d decoder
	d.b = e.buf
	if _, err := decodeVariant(&d); !errors.Is(err, ErrTooLong) {
		t.Fatalf("huge array: got %v, want ErrTooLong", err)
	}
	// Truncated array payload.
	e2 := encoder{}
	e2.u8(VariantArray | VariantInt32)
	e2.i32(4)
	e2.i32(1)
	var d2 decoder
	d2.b = e2.buf
	if _, err := decodeVariant(&d2); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated array: got %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestVariantEncodeMaskValueMismatch(t *testing.T) {
	// Hand-constructed Variants whose mask does not match the value
	// type must return an error, never panic (interface conversion).
	cases := []Variant{
		{Mask: VariantBoolean, Value: []int32{1, 2}},
		{Mask: VariantArray | VariantBoolean, Value: []int32{1, 2}},
		{Mask: VariantArray | VariantBoolean, Value: int32(1)},
		{Mask: VariantArray | VariantBoolean, Value: true},
		{Mask: VariantInt32, Value: "nope"},
		{Mask: VariantArray | VariantString, Value: []int64{1}},
		{Mask: VariantNull, Value: int32(1)},
		{Mask: VariantByteString, Value: "0x01"},
		{Mask: VariantArray | VariantNodeId, Value: []string{"x"}},
		{Mask: VariantDateTime, Value: int64(42)},
		{Mask: VariantDataValue, Value: []DataValue{}},
	}
	for _, v := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("mask 0x%02X value %T: panic %v", v.Mask, v.Value, r)
				}
			}()
			if err := v.encodeUA(&encoder{}); !errors.Is(err, ErrInvalidEncoding) {
				t.Errorf("mask 0x%02X value %T: got %v, want ErrInvalidEncoding", v.Mask, v.Value, err)
			}
		}()
	}
}

func TestVariantDimensionsRoundTrip(t *testing.T) {
	// Decode → re-encode must reproduce the exact wire bytes:
	// decoded dimensions are preserved and re-emitted (mask 0x40).
	var e encoder
	e.u8(VariantInt32 | VariantDimensions | VariantArray)
	e.i32(6)
	for i := int32(0); i < 6; i++ {
		e.i32(i)
	}
	e.i32(2)
	e.i32(2)
	e.i32(3)
	wire := e.buf
	var d decoder
	d.b = wire
	v, err := decodeVariant(&d)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := encodeBytes(t, v.encodeUA)
	if !reflect.DeepEqual(got, wire) {
		t.Fatalf("re-encode mismatch:\n got %X\nwant %X", got, wire)
	}

	// Encode side: dimensions are emitted after the value.
	dims := Variant{
		Mask:       VariantInt32 | VariantArray | VariantDimensions,
		Value:      []int32{1, 2, 3, 4},
		Dimensions: []int32{2, 2},
	}
	want := encodeBytes(t, func(e *encoder) error {
		e.u8(VariantInt32 | VariantArray | VariantDimensions)
		e.i32(4)
		e.i32(1)
		e.i32(2)
		e.i32(3)
		e.i32(4)
		e.i32(2)
		e.i32(2)
		e.i32(2)
		return nil
	})
	if got := encodeBytes(t, dims.encodeUA); !reflect.DeepEqual(got, want) {
		t.Fatalf("dims encode mismatch:\n got %X\nwant %X", got, want)
	}

	// And a null array with dimensions (length -1, dims present).
	nullDims := Variant{
		Mask:       VariantInt32 | VariantArray | VariantDimensions,
		Value:      []int32(nil),
		Dimensions: []int32{0},
	}
	wantNull := encodeBytes(t, func(e *encoder) error {
		e.u8(VariantInt32 | VariantArray | VariantDimensions)
		e.i32(-1)
		e.i32(1)
		e.i32(0)
		return nil
	})
	if got := encodeBytes(t, nullDims.encodeUA); !reflect.DeepEqual(got, wantNull) {
		t.Fatalf("null-dims encode mismatch:\n got %X\nwant %X", got, wantNull)
	}
}

func TestNegativeLengthPrefixesRejected(t *testing.T) {
	// -1 is null; any other negative length is invalid per Part 6.
	for _, b := range [][]byte{{0xFF, 0xFF, 0xFF, 0xFE}, {0xFF, 0xFF, 0xFF, 0x80}} {
		var d decoder
		d.b = b
		if _, err := d.str(); !errors.Is(err, ErrInvalidEncoding) {
			t.Errorf("String length %d: got %v, want ErrInvalidEncoding", int32(int8(b[3])), err)
		}
	}
	var d2 decoder
	d2.b = []byte{0xFF, 0xFF, 0xFF, 0xFE}
	if _, err := d2.bytes(); !errors.Is(err, ErrInvalidEncoding) {
		t.Fatalf("ByteString length -2: got %v, want ErrInvalidEncoding", err)
	}
	// Variant array length -2 must be rejected as well.
	var e encoder
	e.u8(VariantArray | VariantInt32)
	e.i32(-2)
	var d3 decoder
	d3.b = e.buf
	if _, err := decodeVariant(&d3); !errors.Is(err, ErrInvalidEncoding) {
		t.Fatalf("array length -2: got %v, want ErrInvalidEncoding", err)
	}
}

func TestReservedBitsRejected(t *testing.T) {
	// LocalizedText mask with reserved bits set (bits 2-7).
	var d decoder
	d.b = []byte{0x04}
	if _, err := decodeLocalizedText(&d); !errors.Is(err, ErrInvalidEncoding) {
		t.Fatalf("LocalizedText mask 0x04: got %v, want ErrInvalidEncoding", err)
	}
	// DataValue mask with reserved bits set (bits 6-7).
	d2 := decoder{b: []byte{0x40}}
	if _, err := decodeDataValue(&d2); !errors.Is(err, ErrInvalidEncoding) {
		t.Fatalf("DataValue mask 0x40: got %v, want ErrInvalidEncoding", err)
	}
	// NodeId reserved encoding byte 0x86 (extended ByteString is
	// legal; 0x86 is reserved).
	if _, err := decodeNodeIDBytes(t, []byte{0x86, 0, 0, 0, 0}); !errors.Is(err, ErrInvalidEncoding) {
		t.Fatalf("NodeId 0x86: got %v, want ErrInvalidEncoding", err)
	}
}

func TestLocalizedTextString(t *testing.T) {
	lt := NewLocalizedTextWithLocale("zh-CN", "温度")
	if lt.Text != "温度" || !lt.HasText || !lt.HasLocale {
		t.Fatalf("constructor: %+v", lt)
	}
}
