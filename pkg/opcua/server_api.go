package opcua

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

// EncodeSequenceHeader 编码序列头。
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
	return decodeCreateSessionRequest(&d)
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
	return decodeActivateSessionRequest(&d)
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
	return decodeReadRequest(&d)
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
	return decodeWriteRequest(&d)
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
	return decodeCloseSessionRequest(&d)
}

// EncodeCloseSessionResponse 编码 CloseSession 响应体。
func EncodeCloseSessionResponse(r CloseSessionResponse) ([]byte, error) {
	var e encoder
	if err := r.encodeUA(&e); err != nil {
		return nil, err
	}
	return e.buf, nil
}
