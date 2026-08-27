package opcua

// Browse 服务（OPC UA Part 4 §5.8.2，v0.15.0）。
// TypeId=527/530（Encoding_DefaultBinary，官方 NodeIds.csv v1.04）。
// 最小裁剪：ExpandedNodeId 不支持 namespaceUri/serverIndex 形式（mask 恒 0）；
// 单页返回（continuationPoint 为空）；BrowseNext 本轮不发起。

import (
	"fmt"
	"time"
)

const (
	TypeIdBrowseRequest  uint32 = 527
	TypeIdBrowseResponse uint32 = 530

	ReferenceTypeIdOrganizes uint32 = 35 // i=35 Organizes（概念记录）
	NodeClassVariable        uint32 = 2
	NodeClassObject          uint32 = 1
)

// BrowseDescription 内嵌结构（无独立线上 TypeId；DataType 514/Binary 516 仅记录）。
type BrowseDescription struct {
	NodeId          NodeId
	BrowseDirection uint32 // 0=Forward 1=Inverse 2=Both
	ReferenceTypeId NodeId
	IncludeSubtypes bool
	NodeClassMask   uint32
	ResultMask      uint32 // bit0 ReferenceType bit1 IsForward bit2 NodeClass bit3 BrowseName
}

func (b BrowseDescription) encodeUA(e *encoder) error {
	if err := b.NodeId.encodeUA(e); err != nil {
		return err
	}
	e.u32(b.BrowseDirection)
	if err := b.ReferenceTypeId.encodeUA(e); err != nil {
		return err
	}
	e.bool(b.IncludeSubtypes)
	e.u32(b.NodeClassMask)
	e.u32(b.ResultMask)
	return nil
}

func decodeBrowseDescription(d *decoder) (BrowseDescription, error) {
	var b BrowseDescription
	var err error
	if b.NodeId, err = decodeNodeID(d); err != nil {
		return b, err
	}
	if b.BrowseDirection, err = d.u32(); err != nil {
		return b, err
	}
	if b.ReferenceTypeId, err = decodeNodeID(d); err != nil {
		return b, err
	}
	if b.IncludeSubtypes, err = d.bool(); err != nil {
		return b, err
	}
	if b.NodeClassMask, err = d.u32(); err != nil {
		return b, err
	}
	if b.ResultMask, err = d.u32(); err != nil {
		return b, err
	}
	return b, nil
}

type ViewDescription struct {
	ViewId      NodeId
	Timestamp   DateTime
	ViewVersion uint32
}

func (v ViewDescription) encodeUA(e *encoder) error {
	if err := v.ViewId.encodeUA(e); err != nil {
		return err
	}
	e.i64(int64(v.Timestamp))
	e.u32(v.ViewVersion)
	return nil
}

func decodeViewDescription(d *decoder) (ViewDescription, error) {
	var v ViewDescription
	var err error
	if v.ViewId, err = decodeNodeID(d); err != nil {
		return v, err
	}
	var ts int64
	if ts, err = d.i64(); err != nil {
		return v, err
	}
	v.Timestamp = DateTime(ts)
	if v.ViewVersion, err = d.u32(); err != nil {
		return v, err
	}
	return v, nil
}

type BrowseRequest struct {
	RequestHeader
	View                          ViewDescription
	RequestedMaxReferencesPerNode uint32
	NodesToBrowse                 []BrowseDescription
}

func (r BrowseRequest) encodeUA(e *encoder) error {
	if err := r.RequestHeader.encodeUA(e); err != nil {
		return err
	}
	if err := r.View.encodeUA(e); err != nil {
		return err
	}
	e.u32(r.RequestedMaxReferencesPerNode)
	e.i32(int32(len(r.NodesToBrowse)))
	for _, n := range r.NodesToBrowse {
		if err := n.encodeUA(e); err != nil {
			return err
		}
	}
	return nil
}

func decodeBrowseRequest(d *decoder) (BrowseRequest, error) {
	var r BrowseRequest
	var err error
	if r.RequestHeader, err = decodeRequestHeader(d); err != nil {
		return r, err
	}
	if r.View, err = decodeViewDescription(d); err != nil {
		return r, err
	}
	if r.RequestedMaxReferencesPerNode, err = d.u32(); err != nil {
		return r, err
	}
	n, err := d.i32()
	if err != nil {
		return r, err
	}
	if n >= 0 && n <= MaxArrayLength {
		for i := int32(0); i < n; i++ {
			desc, derr := decodeBrowseDescription(d)
			if derr != nil {
				return r, derr
			}
			r.NodesToBrowse = append(r.NodesToBrowse, desc)
		}
	}
	return r, nil
}

// ExpandedNodeId 最小形式：nodeId + 可选 namespaceUri/serverIndex 位恒不置。
type ExpandedNodeId struct {
	NodeId NodeId
}

func (x ExpandedNodeId) encodeUA(e *encoder) error {
	return x.NodeId.encodeUA(e) // NodeId 自带首字节编码位，与“无额外字段”的 ExpandedNodeId 简化形式一致
}

func decodeExpandedNodeId(d *decoder) (ExpandedNodeId, error) {
	var x ExpandedNodeId
	mask, err := d.u8()
	if err != nil {
		return x, err
	}
	// 编码字节低 7 位与 NodeId 一致；bit7=namespaceUri、bit6=serverIndex（本轮拒绝）
	if mask&0xC0 != 0 {
		return x, fmt.Errorf("%w: ExpandedNodeId namespace URI / server index (mask 0x%02X)", ErrUnsupportedType, mask)
	}
	d.off-- // 回退已消费的编码字节，交由 decodeNodeID 正常分派
	x.NodeId, err = decodeNodeID(d)
	return x, err
}

type ReferenceDescription struct {
	ReferenceTypeId NodeId
	IsForward       bool
	NodeId          ExpandedNodeId
	BrowseName      QualifiedName
	DisplayName     LocalizedText
	NodeClass       uint32
}

func (r ReferenceDescription) encodeUA(e *encoder) error {
	if err := r.ReferenceTypeId.encodeUA(e); err != nil {
		return err
	}
	e.bool(r.IsForward)
	if err := r.NodeId.encodeUA(e); err != nil {
		return err
	}
	r.BrowseName.encodeUA(e)
	r.DisplayName.encodeUA(e)
	e.u32(r.NodeClass)
	return nil
}

func decodeReferenceDescription(d *decoder) (ReferenceDescription, error) {
	var r ReferenceDescription
	var err error
	if r.ReferenceTypeId, err = decodeNodeID(d); err != nil {
		return r, err
	}
	if r.IsForward, err = d.bool(); err != nil {
		return r, err
	}
	if r.NodeId, err = decodeExpandedNodeId(d); err != nil {
		return r, err
	}
	if r.BrowseName, err = decodeQualifiedName(d); err != nil {
		return r, err
	}
	if r.DisplayName, err = decodeLocalizedText(d); err != nil {
		return r, err
	}
	r.NodeClass, err = d.u32()
	return r, err
}

type BrowseResult struct {
	Status            StatusCode
	ContinuationPoint []byte
	References        []ReferenceDescription
}

func (r BrowseResult) encodeUA(e *encoder) error {
	e.u32(uint32(r.Status))
	e.bytes(r.ContinuationPoint)
	e.i32(int32(len(r.References)))
	for _, ref := range r.References {
		if err := ref.encodeUA(e); err != nil {
			return err
		}
	}
	return nil
}

func decodeBrowseResult(d *decoder) (BrowseResult, error) {
	var r BrowseResult
	st, err := d.u32()
	if err != nil {
		return r, err
	}
	r.Status = StatusCode(st)
	if r.ContinuationPoint, err = d.bytes(); err != nil {
		return r, err
	}
	n, err := d.i32()
	if err != nil {
		return r, err
	}
	if n >= 0 && n <= MaxArrayLength {
		for i := int32(0); i < n; i++ {
			ref, derr := decodeReferenceDescription(d)
			if derr != nil {
				return r, derr
			}
			r.References = append(r.References, ref)
		}
	}
	return r, nil
}

type BrowseResponse struct {
	ResponseHeader
	Results         []BrowseResult
	DiagnosticInfos []DiagnosticInfo
}

func (r BrowseResponse) encodeUA(e *encoder) error {
	if err := r.ResponseHeader.encodeUA(e); err != nil {
		return err
	}
	e.i32(int32(len(r.Results)))
	for _, res := range r.Results {
		if err := res.encodeUA(e); err != nil {
			return err
		}
	}
	encodeDiagnosticInfoList(e, r.DiagnosticInfos)
	return nil
}

func decodeBrowseResponse(d *decoder) (BrowseResponse, error) {
	var r BrowseResponse
	var err error
	if r.ResponseHeader, err = decodeResponseHeader(d); err != nil {
		return r, err
	}
	n, err := d.i32()
	if err != nil {
		return r, err
	}
	if n >= 0 && n <= MaxArrayLength {
		for i := int32(0); i < n; i++ {
			res, derr := decodeBrowseResult(d)
			if derr != nil {
				return r, derr
			}
			r.Results = append(r.Results, res)
		}
	}
	if r.DiagnosticInfos, err = decodeDiagnosticInfoList(d); err != nil {
		return r, err
	}
	return r, nil
}

// ---------------------------------------------------------------------
// 高层 API：Client.Browse 返回 (browseName, nodeId, nodeClass) 列表。
// ---------------------------------------------------------------------

type BrowsedNode struct {
	Name      string
	NodeId    NodeId
	NodeClass uint32
}

// Browse 浏览指定节点的正向 Hierarchical/Organizing 引用（简化：全引用遍历，
// 由调用方按 NodeClass 过滤）。resultMask=0x3f（全部字段）。
func (c *Client) Browse(node NodeId) ([]BrowsedNode, error) {
	req := BrowseRequest{
		RequestHeader: RequestHeader{
			AuthenticationToken: c.authTok,
			Timestamp:           DateTimeFromTime(time.Now()),
			RequestHandle:       c.sc.nextReqID(),
		},
		View:                          ViewDescription{ViewId: NewNodeID(0, 0)},
		RequestedMaxReferencesPerNode: 1000,
		NodesToBrowse: []BrowseDescription{{
			NodeId:          node,
			BrowseDirection: 0,                // Forward
			ReferenceTypeId: NewNodeID(0, 33), // HierarchicalReferences（含子类型语义忽略——直接全引）
			IncludeSubtypes: true,
			NodeClassMask:   0, // 全部
			ResultMask:      0x3f,
		}},
	}
	var e encoder
	if err := req.encodeUA(&e); err != nil {
		return nil, err
	}
	body, err := c.roundTrip(e.buf)
	if err != nil {
		return nil, fmt.Errorf("opcua: send Browse: %w", err)
	}
	var d decoder
	d.b = body
	resp, err := decodeBrowseResponse(&d)
	if err != nil {
		return nil, fmt.Errorf("opcua: decode Browse response: %w", err)
	}
	if !resp.ServiceResult.IsGood() {
		return nil, fmt.Errorf("opcua: Browse 服务失败: %s", resp.ServiceResult)
	}
	if len(resp.Results) == 0 {
		return nil, fmt.Errorf("opcua: Browse 响应缺少 Results")
	}
	res := resp.Results[0]
	if !res.Status.IsGood() {
		return nil, fmt.Errorf("opcua: Browse 结果失败: %s", res.Status)
	}
	out := make([]BrowsedNode, 0, len(res.References))
	for _, ref := range res.References {
		out = append(out, BrowsedNode{
			Name:      ref.BrowseName.Name,
			NodeId:    ref.NodeId.NodeId,
			NodeClass: ref.NodeClass,
		})
	}
	return out, nil
}

// BrowseRaw 是调试辅助：发送请求并返回原始响应服务体（未解码）。
// 仅供 hack 探针与互操作排障使用，不在稳定 API 承诺范围。
func (c *Client) BrowseRaw(req BrowseRequest) ([]BrowsedNode, []byte, error) {
	var e encoder
	if err := req.encodeUA(&e); err != nil {
		return nil, nil, err
	}
	body, err := c.roundTrip(e.buf)
	if err != nil {
		return nil, nil, err
	}
	raw := append([]byte{}, body...)
	var d decoder
	d.b = body
	resp, derr := decodeBrowseResponse(&d)
	if derr != nil {
		return nil, raw, derr
	}
	out := make([]BrowsedNode, 0)
	if len(resp.Results) > 0 {
		for _, ref := range resp.Results[0].References {
			out = append(out, BrowsedNode{Name: ref.BrowseName.Name, NodeId: ref.NodeId.NodeId, NodeClass: ref.NodeClass})
		}
	}
	return out, raw, nil
}
