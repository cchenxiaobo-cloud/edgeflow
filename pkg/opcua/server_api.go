package opcua

import (
	"fmt"
)

// server_api.go —— 服务端互操作面（v0.14.0）。
//
// 把客户端侧私有的 UA Binary 编解码以导出包装暴露给服务端实现
// （自研模拟器 pkg/opcuasim / 未来真实服务器适配）：服务端需要
// 解码客户端请求（OPN / 各服务请求）并编码响应。全部为薄包装，
// 不改动任何既有符号。

// EncodeAsymmetricSecurityHeader 编码非对称安全头（OPN/CLO 帧头）。
func EncodeAsymmetricSecurityHeader(h AsymmetricSecurityHeader) ([]byte, error) {
	var e encoder
	if err := h.encodeUA(&e); err != nil {
		return nil, err
	}
	return e.buf, nil
}

// DecodeAsymmetricSecurityHeader 解码非对称安全头，返回剩余字节。
func DecodeAsymmetricSecurityHeader(b []byte) (AsymmetricSecurityHeader, []byte, error) {
	var d decoder
	d.b = b
	h, err := decodeAsymmetricSecurityHeader(&d)
	if err != nil {
		return h, nil, err
	}
	return h, d.b[d.off:], nil
}

// EncodeSymmetricSecurityHeader 编码对称安全头（MSG 帧头）。
func EncodeSymmetricSecurityHeader(h SymmetricSecurityHeader) ([]byte, error) {
	var e encoder
	if err := h.encodeUA(&e); err != nil {
		return nil, err
	}
	return e.buf, nil
}

// DecodeSymmetricSecurityHeader 解码对称安全头，返回剩余字节。
func DecodeSymmetricSecurityHeader(b []byte) (SymmetricSecurityHeader, []byte, error) {
	var d decoder
	d.b = b
	h, err := decodeSymmetricSecurityHeader(&d)
	if err != nil {
		return h, nil, err
	}
	return h, d.b[d.off:], nil
}

// EncodeSequenceHeader 编码序列头（server_api 导出层）。
func EncodeSequenceHeader(h SequenceHeader) ([]byte, error) {
	var e encoder
	if err := h.encodeUA(&e); err != nil {
		return nil, err
	}
	return e.buf, nil
}

// DecodeSequenceHeader 解码序列头，返回剩余字节。
func DecodeSequenceHeader(b []byte) (SequenceHeader, []byte, error) {
	var d decoder
	d.b = b
	h, err := decodeSequenceHeader(&d)
	if err != nil {
		return h, nil, err
	}
	return h, d.b[d.off:], nil
}

// DecodeOpenSecureChannelRequest 解码 OPN 请求体（去除安全头与序列头后的剩余字节）。
func DecodeOpenSecureChannelRequest(b []byte) (OpenSecureChannelRequest, error) {
	var d decoder
	d.b = b
	return decodeOpenSecureChannelRequest(&d)
}

// EncodeOpenSecureChannelResponse 编码 OPN 响应体。
func EncodeOpenSecureChannelResponse(r OpenSecureChannelResponse) ([]byte, error) {
	var e encoder
	if err := r.encodeUA(&e); err != nil {
		return nil, err
	}
	return e.buf, nil
}

// DecodeRequestHeader 解码服务请求头。
func DecodeRequestHeader(b []byte) (RequestHeader, []byte, error) {
	var d decoder
	d.b = b
	h, err := decodeRequestHeader(&d)
	if err != nil {
		return h, nil, err
	}
	return h, d.b[d.off:], nil
}

// EncodeSignatureData 编码 SignatureData（Part 4 §7.31）。
func EncodeSignatureData(s SignatureData) ([]byte, error) {
	var e encoder
	if err := s.encodeUA(&e); err != nil {
		return nil, err
	}
	return e.buf, nil
}

// DecodeSignatureData 解码 SignatureData，返回剩余字节（v0.28.1 OPN 体加密）。
func DecodeSignatureData(b []byte) (SignatureData, []byte, error) {
	var d decoder
	d.b = b
	s, err := decodeSignatureData(&d)
	if err != nil {
		return s, nil, err
	}
	return s, d.b[d.off:], nil
}

// EncodeResponseHeader 编码服务响应头。
func EncodeResponseHeader(h ResponseHeader) ([]byte, error) {
	var e encoder
	if err := h.encodeUA(&e); err != nil {
		return nil, err
	}
	return e.buf, nil
}

// DecodeCreateSessionRequest 解码 CreateSession 请求体。
func DecodeCreateSessionRequest(b []byte) (CreateSessionRequest, error) {
	var d decoder
	d.b = b
	r, err := decodeCreateSessionRequest(&d)
	if err != nil {
		return r, err
	}
	// 试解防误吞：要求字节全消费，否则视为形状不符（交回分派链下一分支）
	if d.off != len(d.b) {
		return r, fmt.Errorf("%w: DecodeCreateSessionRequest trailing bytes (%d/%d)", ErrInvalidEncoding, d.off, len(d.b))
	}
	return r, nil
}

// EncodeCreateSessionResponse 编码 CreateSession 响应体。
func EncodeCreateSessionResponse(r CreateSessionResponse) ([]byte, error) {
	var e encoder
	if err := r.encodeUA(&e); err != nil {
		return nil, err
	}
	return e.buf, nil
}

// DecodeActivateSessionRequest 解码 ActivateSession 请求体。
func DecodeActivateSessionRequest(b []byte) (ActivateSessionRequest, error) {
	var d decoder
	d.b = b
	r, err := decodeActivateSessionRequest(&d)
	if err != nil {
		return r, err
	}
	// 试解防误吞：要求字节全消费，否则视为形状不符（交回分派链下一分支）
	if d.off != len(d.b) {
		return r, fmt.Errorf("%w: DecodeActivateSessionRequest trailing bytes (%d/%d)", ErrInvalidEncoding, d.off, len(d.b))
	}
	return r, nil
}

// EncodeActivateSessionResponse 编码 ActivateSession 响应体。
func EncodeActivateSessionResponse(r ActivateSessionResponse) ([]byte, error) {
	var e encoder
	if err := r.encodeUA(&e); err != nil {
		return nil, err
	}
	return e.buf, nil
}

// DecodeReadRequest 解码 Read 请求体。
func DecodeReadRequest(b []byte) (ReadRequest, error) {
	var d decoder
	d.b = b
	r, err := decodeReadRequest(&d)
	if err != nil {
		return r, err
	}
	// 试解防误吞：要求字节全消费，否则视为形状不符（交回分派链下一分支）
	if d.off != len(d.b) {
		return r, fmt.Errorf("%w: DecodeReadRequest trailing bytes (%d/%d)", ErrInvalidEncoding, d.off, len(d.b))
	}
	return r, nil
}

// EncodeReadResponse 编码 Read 响应体。
func EncodeReadResponse(r ReadResponse) ([]byte, error) {
	var e encoder
	if err := r.encodeUA(&e); err != nil {
		return nil, err
	}
	return e.buf, nil
}

// DecodeWriteRequest 解码 Write 请求体。
func DecodeWriteRequest(b []byte) (WriteRequest, error) {
	var d decoder
	d.b = b
	r, err := decodeWriteRequest(&d)
	if err != nil {
		return r, err
	}
	// 试解防误吞：要求字节全消费，否则视为形状不符（交回分派链下一分支）
	if d.off != len(d.b) {
		return r, fmt.Errorf("%w: DecodeWriteRequest trailing bytes (%d/%d)", ErrInvalidEncoding, d.off, len(d.b))
	}
	return r, nil
}

// EncodeWriteResponse 编码 Write 响应体。
func EncodeWriteResponse(r WriteResponse) ([]byte, error) {
	var e encoder
	if err := r.encodeUA(&e); err != nil {
		return nil, err
	}
	return e.buf, nil
}

// DecodeCloseSessionRequest 解码 CloseSession 请求体。
func DecodeCloseSessionRequest(b []byte) (CloseSessionRequest, error) {
	var d decoder
	d.b = b
	r, err := decodeCloseSessionRequest(&d)
	if err != nil {
		return r, err
	}
	// 试解防误吞：要求字节全消费，否则视为形状不符（交回分派链下一分支）
	if d.off != len(d.b) {
		return r, fmt.Errorf("%w: DecodeCloseSessionRequest trailing bytes (%d/%d)", ErrInvalidEncoding, d.off, len(d.b))
	}
	return r, nil
}

// EncodeCloseSessionResponse 编码 CloseSession 响应体。
func EncodeCloseSessionResponse(r CloseSessionResponse) ([]byte, error) {
	var e encoder
	if err := r.encodeUA(&e); err != nil {
		return nil, err
	}
	return e.buf, nil
}

// ---------------------------------------------------------------------
// 订阅族导出面（v0.15.0）：供 pkg/opcuasim 等服务端适配使用。
// ---------------------------------------------------------------------

// DecodeCreateSubscriptionRequest 解码 CreateSubscription 请求体。
func DecodeCreateSubscriptionRequest(b []byte) (CreateSubscriptionRequest, error) {
	var d decoder
	d.b = b
	r, err := decodeCreateSubscriptionRequest(&d)
	if err != nil {
		return r, err
	}
	// 试解防误吞：要求字节全消费，否则视为形状不符（交回分派链下一分支）
	if d.off != len(d.b) {
		return r, fmt.Errorf("%w: DecodeCreateSubscriptionRequest trailing bytes (%d/%d)", ErrInvalidEncoding, d.off, len(d.b))
	}
	return r, nil
}

// EncodeCreateSubscriptionResponse 编码 CreateSubscription 响应体。
func EncodeCreateSubscriptionResponse(r CreateSubscriptionResponse) ([]byte, error) {
	var e encoder
	if err := r.encodeUA(&e); err != nil {
		return nil, err
	}
	return e.buf, nil
}

// DecodePublishRequest 解码 Publish 请求体。
func DecodePublishRequest(b []byte) (PublishRequest, error) {
	var d decoder
	d.b = b
	return decodePublishRequest(&d)
}

// EncodePublishResponse 编码 Publish 响应体。
func EncodePublishResponse(r PublishResponse) ([]byte, error) {
	var e encoder
	if err := r.encodeUA(&e); err != nil {
		return nil, err
	}
	return e.buf, nil
}

// DecodeCreateMonitoredItemsRequest 解码 CreateMonitoredItems 请求体。
func DecodeCreateMonitoredItemsRequest(b []byte) (CreateMonitoredItemsRequest, error) {
	var d decoder
	d.b = b
	r, err := decodeCreateMonitoredItemsRequest(&d)
	if err != nil {
		return r, err
	}
	// 试解防误吞：要求字节全消费，否则视为形状不符（交回分派链下一分支）
	if d.off != len(d.b) {
		return r, fmt.Errorf("%w: DecodeCreateMonitoredItemsRequest trailing bytes (%d/%d)", ErrInvalidEncoding, d.off, len(d.b))
	}
	return r, nil
}

// EncodeCreateMonitoredItemsResponse 编码 CreateMonitoredItems 响应体。
func EncodeCreateMonitoredItemsResponse(r CreateMonitoredItemsResponse) ([]byte, error) {
	var e encoder
	if err := r.encodeUA(&e); err != nil {
		return nil, err
	}
	return e.buf, nil
}

// DecodeDeleteSubscriptionsRequest 解码 DeleteSubscriptions 请求体。
func DecodeDeleteSubscriptionsRequest(b []byte) (DeleteSubscriptionsRequest, error) {
	var d decoder
	d.b = b
	r, err := decodeDeleteSubscriptionsRequest(&d)
	if err != nil {
		return r, err
	}
	// 试解防误吞：要求字节全消费，否则视为形状不符（交回分派链下一分支）
	if d.off != len(d.b) {
		return r, fmt.Errorf("%w: DecodeDeleteSubscriptionsRequest trailing bytes (%d/%d)", ErrInvalidEncoding, d.off, len(d.b))
	}
	return r, nil
}

// EncodeDeleteSubscriptionsResponse 编码 DeleteSubscriptions 响应体。
func EncodeDeleteSubscriptionsResponse(r DeleteSubscriptionsResponse) ([]byte, error) {
	var e encoder
	if err := r.encodeUA(&e); err != nil {
		return nil, err
	}
	return e.buf, nil
}

// DecodeBrowseRequest 解码 Browse 请求体。
func DecodeBrowseRequest(b []byte) (BrowseRequest, error) {
	var d decoder
	d.b = b
	r, err := decodeBrowseRequest(&d)
	if err != nil {
		return r, err
	}
	// 试解防误吞：要求字节全消费，否则视为形状不符（交回分派链下一分支）
	if d.off != len(d.b) {
		return r, fmt.Errorf("%w: DecodeBrowseRequest trailing bytes (%d/%d)", ErrInvalidEncoding, d.off, len(d.b))
	}
	return r, nil
}

// EncodeBrowseResponse 编码 Browse 响应体。
func EncodeBrowseResponse(r BrowseResponse) ([]byte, error) {
	var e encoder
	if err := r.encodeUA(&e); err != nil {
		return nil, err
	}
	return e.buf, nil
}

// EncodeBrowseRequest 编码 Browse 请求体（互操作调试用）。
func EncodeBrowseRequest(r BrowseRequest) ([]byte, error) {
	var e encoder
	if err := r.encodeUA(&e); err != nil {
		return nil, err
	}
	return e.buf, nil
}

// EncodePublishRequest 编码 Publish 请求体（互操作调试用）。
func EncodePublishRequest(r PublishRequest) ([]byte, error) {
	var e encoder
	if err := r.encodeUA(&e); err != nil {
		return nil, err
	}
	return e.buf, nil
}

// EncodeCreateMonitoredItemsRequest 编码 CreateMonitoredItems 请求体（互操作调试用）。
func EncodeCreateMonitoredItemsRequest(r CreateMonitoredItemsRequest) ([]byte, error) {
	var e encoder
	if err := r.encodeUA(&e); err != nil {
		return nil, err
	}
	return e.buf, nil
}

// EncodeCreateSubscriptionRequest 编码 CreateSubscription 请求体（互操作调试用）。
func EncodeCreateSubscriptionRequest(r CreateSubscriptionRequest) ([]byte, error) {
	var e encoder
	if err := r.encodeUA(&e); err != nil {
		return nil, err
	}
	return e.buf, nil
}
