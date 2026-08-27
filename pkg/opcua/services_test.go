package opcua

import (
	"reflect"
	"testing"
	"time"
)

// TestServiceMessageRoundTrip 验证各服务消息的编解码 round-trip。
func TestServiceMessageRoundTrip(t *testing.T) {
	now := DateTimeFromTime(time.Now())
	baseReq := RequestHeader{
		AuthenticationToken: NewNodeID(0, 999),
		Timestamp:           now,
		RequestHandle:       42,
		ReturnDiagnostics:   0,
		AuditEntryId:        "",
		TimeoutHint:         0,
		AdditionalHeader:    ExtensionObject{TypeId: NewNodeID(0, 0), Encoding: ExtensionObjectEncodingNone},
	}
	baseResp := ResponseHeader{
		Timestamp:          now,
		RequestHandle:      42,
		ServiceResult:      StatusCode(0),
		ServiceDiagnostics: DiagnosticInfo{},
		StringTable:        nil,
		AdditionalHeader:   ExtensionObject{TypeId: NewNodeID(0, 0), Encoding: ExtensionObjectEncodingNone},
	}

	// CreateSession
	t.Run("CreateSession", func(t *testing.T) {
		req := CreateSessionRequest{
			RequestHeader: baseReq,
			ClientDescription: ApplicationDescription{
				ApplicationUri: "urn:edgeflow:client", ProductUri: "urn:edgeflow",
				ApplicationName: NewLocalizedText("EdgeFlow"), ApplicationType: 1,
				DiscoveryUrls: []string{"opc.tcp://127.0.0.1:4840"},
			},
			ServerUri: "opc.tcp://127.0.0.1:4840", EndpointUrl: "opc.tcp://127.0.0.1:4840",
			SessionName: "edgeflow-1", ClientNonce: []byte{1, 2, 3},
			RequestedSessionTimeout: 3600000,
		}
		var e encoder
		if err := req.encodeUA(&e); err != nil {
			t.Fatalf("encode: %v", err)
		}
		var d decoder
		d.b = e.buf
		got, err := decodeCreateSessionRequest(&d)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !reflect.DeepEqual(got, req) {
			t.Fatalf("round-trip 不一致:\n want %+v\n got  %+v", req, got)
		}
	})
	t.Run("CreateSessionResponse", func(t *testing.T) {
		resp := CreateSessionResponse{
			ResponseHeader:        baseResp,
			SessionId:             NewNodeID(0, 100),
			AuthenticationToken:   NewNodeID(0, 101),
			RevisedSessionTimeout: 3600000,
			ServerNonce:           []byte{9, 9},
			ServerEndpoints:       []EndpointDescription{{EndpointUrl: "opc.tcp://x", SecurityMode: 1, SecurityLevel: 0}},
			MaxRequestMessageSize: 65536,
		}
		var e encoder
		if err := resp.encodeUA(&e); err != nil {
			t.Fatalf("encode: %v", err)
		}
		var d decoder
		d.b = e.buf
		got, err := decodeCreateSessionResponse(&d)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !reflect.DeepEqual(got, resp) {
			t.Fatalf("round-trip 不一致:\n want %+v\n got  %+v", resp, got)
		}
	})
	// ActivateSession
	t.Run("ActivateSession", func(t *testing.T) {
		req := ActivateSessionRequest{
			RequestHeader:     baseReq,
			LocaleIds:         []string{"en"},
			UserIdentityToken: AnonymousIdentityToken(),
		}
		var e encoder
		if err := req.encodeUA(&e); err != nil {
			t.Fatalf("encode: %v", err)
		}
		var d decoder
		d.b = e.buf
		got, err := decodeActivateSessionRequest(&d)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !reflect.DeepEqual(got, req) {
			t.Fatalf("round-trip 不一致:\n want %+v\n got  %+v", req, got)
		}
		// 匿名令牌 body 应为 UA String("Anonymous")
		var bd decoder
		bd.b = got.UserIdentityToken.Body
		if s, err := bd.str(); err != nil || s != "Anonymous" {
			t.Fatalf("匿名令牌 body 异常: %q err %v", s, err)
		}
	})
	t.Run("ActivateSessionResponse", func(t *testing.T) {
		resp := ActivateSessionResponse{
			ResponseHeader:  baseResp,
			ServerNonce:     nil,
			Results:         []StatusCode{0},
			DiagnosticInfos: []DiagnosticInfo{{SymbolicID: intPtr32(1)}},
		}
		var e encoder
		if err := resp.encodeUA(&e); err != nil {
			t.Fatalf("encode: %v", err)
		}
		var d decoder
		d.b = e.buf
		got, err := decodeActivateSessionResponse(&d)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !reflect.DeepEqual(got, resp) {
			t.Fatalf("round-trip 不一致:\n want %+v\n got  %+v", resp, got)
		}
	})
	// Read
	t.Run("Read", func(t *testing.T) {
		req := ReadRequest{
			RequestHeader:      baseReq,
			TimestampsToReturn: 2,
			NodesToRead: []ReadValueId{
				{NodeId: NewNodeID(2, 1001), AttributeId: AttributeIdValue},
				{NodeId: NewStringNodeID(0, "x"), AttributeId: AttributeIdValue},
			},
		}
		var e encoder
		if err := req.encodeUA(&e); err != nil {
			t.Fatalf("encode: %v", err)
		}
		var d decoder
		d.b = e.buf
		got, err := decodeReadRequest(&d)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !reflect.DeepEqual(got, req) {
			t.Fatalf("round-trip 不一致:\n want %+v\n got  %+v", req, got)
		}
	})
	t.Run("ReadResponse", func(t *testing.T) {
		v, _ := NewVariant(25.5)
		resp := ReadResponse{
			ResponseHeader: baseResp,
			Results: []DataValue{
				{Value: &v},
				{Status: statusPtr(0x80340000)},
			},
		}
		var e encoder
		if err := resp.encodeUA(&e); err != nil {
			t.Fatalf("encode: %v", err)
		}
		var d decoder
		d.b = e.buf
		got, err := decodeReadResponse(&d)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(got.Results) != 2 || got.Results[0].Value == nil || got.Results[1].Status == nil {
			t.Fatalf("结果解析异常: %+v", got.Results)
		}
		gotV := *got.Results[0].Value
		if f, _ := gotV.Value.(float64); f != 25.5 {
			t.Fatalf("读回值异常: %v", gotV.Value)
		}
	})
	// Write
	t.Run("Write", func(t *testing.T) {
		v, _ := NewVariant(200.0)
		req := WriteRequest{
			RequestHeader: baseReq,
			NodesToWrite: []WriteValue{{
				NodeId: NewNodeID(2, 3001), AttributeId: AttributeIdValue,
				Value: DataValue{Value: &v, Status: statusPtr(0)},
			}},
		}
		var e encoder
		if err := req.encodeUA(&e); err != nil {
			t.Fatalf("encode: %v", err)
		}
		var d decoder
		d.b = e.buf
		got, err := decodeWriteRequest(&d)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(got.NodesToWrite) != 1 || got.NodesToWrite[0].Value.Value == nil {
			t.Fatalf("write 解析异常: %+v", got.NodesToWrite)
		}
	})
	t.Run("WriteResponse", func(t *testing.T) {
		resp := WriteResponse{
			ResponseHeader: baseResp,
			Results:        []StatusCode{0},
		}
		var e encoder
		if err := resp.encodeUA(&e); err != nil {
			t.Fatalf("encode: %v", err)
		}
		var d decoder
		d.b = e.buf
		got, err := decodeWriteResponse(&d)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !reflect.DeepEqual(got, resp) {
			t.Fatalf("round-trip 不一致:\n want %+v\n got  %+v", resp, got)
		}
	})
	// CloseSession
	t.Run("CloseSession", func(t *testing.T) {
		req := CloseSessionRequest{RequestHeader: baseReq, DeleteSubscriptions: true}
		var e encoder
		if err := req.encodeUA(&e); err != nil {
			t.Fatalf("encode: %v", err)
		}
		var d decoder
		d.b = e.buf
		got, err := decodeCloseSessionRequest(&d)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.DeleteSubscriptions != true || got.RequestHandle != 42 {
			t.Fatalf("round-trip 不一致: %+v", got)
		}
	})
	t.Run("CloseSessionResponse", func(t *testing.T) {
		resp := CloseSessionResponse{ResponseHeader: baseResp}
		var e encoder
		if err := resp.encodeUA(&e); err != nil {
			t.Fatalf("encode: %v", err)
		}
		var d decoder
		d.b = e.buf
		got, err := decodeCloseSessionResponse(&d)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.RequestHandle != 42 {
			t.Fatalf("round-trip 不一致: %+v", got)
		}
	})
}
