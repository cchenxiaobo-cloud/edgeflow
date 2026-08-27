package opcua

import (
	"errors"
	"testing"
)

// TestDiagnosticInfoRoundTrip 验证 DiagnosticInfo 位域全组合编解码 round-trip。
func TestDiagnosticInfoRoundTrip(t *testing.T) {
	good := StatusCode(0)
	tests := []struct {
		name string
		di   DiagnosticInfo
	}{
		{name: "空", di: DiagnosticInfo{}},
		{name: "symbolicID", di: DiagnosticInfo{SymbolicID: intPtr32(7)}},
		{name: "namespaceURI", di: DiagnosticInfo{NamespaceURI: intPtr32(2)}},
		{name: "locale+localizedText", di: DiagnosticInfo{Locale: intPtr32(1), LocalizedText: intPtr32(5)}},
		{name: "additionalInfo", di: DiagnosticInfo{AdditionalInfo: strPtr("extra")}},
		{name: "innerStatus", di: DiagnosticInfo{InnerStatusCode: &good}},
		{name: "全组合", di: DiagnosticInfo{
			SymbolicID: intPtr32(1), NamespaceURI: intPtr32(2), Locale: intPtr32(3),
			LocalizedText: intPtr32(4), AdditionalInfo: strPtr("x"),
			InnerStatusCode:     &good,
			InnerDiagnosticInfo: &DiagnosticInfo{SymbolicID: intPtr32(99)},
		}},
		{name: "深层递归", di: DiagnosticInfo{
			InnerDiagnosticInfo: &DiagnosticInfo{
				InnerDiagnosticInfo: &DiagnosticInfo{AdditionalInfo: strPtr("deep")},
			},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var e encoder
			if err := tt.di.encodeUA(&e); err != nil {
				t.Fatalf("encode: %v", err)
			}
			var d decoder
			d.b = e.buf
			got, err := decodeDiagnosticInfo(&d)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !diagnosticInfoEqual(tt.di, got) {
				t.Fatalf("round-trip 不一致:\n  want %+v\n  got  %+v", tt.di, got)
			}
		})
	}
}

// TestDiagnosticInfoReservedBitRejected 验证保留位（bit7）被拒绝。
func TestDiagnosticInfoReservedBitRejected(t *testing.T) {
	var d decoder
	d.b = []byte{0x80}
	if _, err := decodeDiagnosticInfo(&d); !errors.Is(err, ErrInvalidEncoding) {
		t.Fatalf("保留位应报 ErrInvalidEncoding，got %v", err)
	}
}

// TestDiagnosticInfoListRoundTrip 验证数组 round-trip（含 null）。
func TestDiagnosticInfoListRoundTrip(t *testing.T) {
	var e encoder
	encodeDiagnosticInfoList(&e, nil) // null
	var d decoder
	d.b = e.buf
	got, err := decodeDiagnosticInfoList(&d)
	if err != nil || got != nil {
		t.Fatalf("null 列表: got %v err %v", got, err)
	}

	e.buf = nil
	list := []DiagnosticInfo{{SymbolicID: intPtr32(1)}, {}}
	encodeDiagnosticInfoList(&e, list)
	var d2 decoder
	d2.b = e.buf
	got2, err := decodeDiagnosticInfoList(&d2)
	if err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(got2) != 2 || !diagnosticInfoEqual(list[0], got2[0]) || !diagnosticInfoEqual(list[1], got2[1]) {
		t.Fatalf("list round-trip 不一致: got %+v", got2)
	}
}

func intPtr32(v int32) *int32 { return &v }
func strPtr(v string) *string { return &v }

func diagnosticInfoEqual(a, b DiagnosticInfo) bool {
	if (a.SymbolicID == nil) != (b.SymbolicID == nil) {
		return false
	}
	if a.SymbolicID != nil && *a.SymbolicID != *b.SymbolicID {
		return false
	}
	if (a.NamespaceURI == nil) != (b.NamespaceURI == nil) {
		return false
	}
	if a.NamespaceURI != nil && *a.NamespaceURI != *b.NamespaceURI {
		return false
	}
	if (a.Locale == nil) != (b.Locale == nil) {
		return false
	}
	if a.Locale != nil && *a.Locale != *b.Locale {
		return false
	}
	if (a.LocalizedText == nil) != (b.LocalizedText == nil) {
		return false
	}
	if a.LocalizedText != nil && *a.LocalizedText != *b.LocalizedText {
		return false
	}
	if (a.AdditionalInfo == nil) != (b.AdditionalInfo == nil) {
		return false
	}
	if a.AdditionalInfo != nil && *a.AdditionalInfo != *b.AdditionalInfo {
		return false
	}
	if (a.InnerStatusCode == nil) != (b.InnerStatusCode == nil) {
		return false
	}
	if a.InnerStatusCode != nil && *a.InnerStatusCode != *b.InnerStatusCode {
		return false
	}
	if (a.InnerDiagnosticInfo == nil) != (b.InnerDiagnosticInfo == nil) {
		return false
	}
	if a.InnerDiagnosticInfo != nil && !diagnosticInfoEqual(*a.InnerDiagnosticInfo, *b.InnerDiagnosticInfo) {
		return false
	}
	return true
}
