package opcua

// 订阅服务族（OPC UA Part 4 §5.13，v0.15.0）。
// TypeId 均为 Encoding_DefaultBinary 数值（来源：OPC Foundation
// UA-Nodeset v1.04 Schema/NodeIds.csv；见 docs/API-SPEC §13）。
//
// 异步模型（泵模式）：首次订阅后由 Client 启动唯一读 goroutine，
// PublishResponse 经分发器推送至 pubCh；严格配对请求仍走 syncWaiters。

import (
	"fmt"
)

// 服务 TypeId（Binary 编码数值）。注意与 DataType 数值区分：
// 例如 CreateSubscriptionRequest 的 DataType 是 785，落线 TypeId 是 787。
const (
	TypeIdCreateSubscriptionRequest    uint32 = 787
	TypeIdCreateSubscriptionResponse   uint32 = 790
	TypeIdModifySubscriptionRequest    uint32 = 793
	TypeIdModifySubscriptionResponse   uint32 = 796
	TypeIdDeleteSubscriptionsRequest   uint32 = 847
	TypeIdDeleteSubscriptionsResponse  uint32 = 850
	TypeIdCreateMonitoredItemsRequest  uint32 = 751
	TypeIdCreateMonitoredItemsResponse uint32 = 754
	TypeIdDeleteMonitoredItemsRequest  uint32 = 781
	TypeIdDeleteMonitoredItemsResponse uint32 = 784
	TypeIdPublishRequest               uint32 = 826
	TypeIdPublishResponse              uint32 = 829
	TypeIdRepublishRequest             uint32 = 832 // 本轮客户端不主动发
	TypeIdRepublishResponse            uint32 = 835

	// NotificationMessage.ExtensionObject 内容 TypeId。
	TypeIdDataChangeNotification   uint32 = 811
	TypeIdStatusChangeNotification uint32 = 820
	TypeIdEventNotificationList    uint32 = 916 // 本轮不解码内容（跳过）
	TypeIdDataChangeFilter         uint32 = 724
)

// MonitoringMode（Part 4 §5.13.2）。
const (
	MonitoringModeDisabled  uint32 = 0
	MonitoringModeSampling  uint32 = 1
	MonitoringModeReporting uint32 = 2
)

// TimestampsToReturn 枚举（Part 4 §5.12.1）/§7.30）。
const (
	TimestampsToReturnSource  int32 = 0
	TimestampsToReturnServer  int32 = 1
	TimestampsToReturnBoth    int32 = 2
	TimestampsToReturnNeither int32 = 3
)

// DataChangeTrigger / DeadbandType（Part 4 §7.21.2/§7.19）。
const (
	DataChangeTriggerStatus        uint32 = 0
	DataChangeTriggerStatusValue   uint32 = 1
	DataChangeTriggerStatusValueTs uint32 = 2
	DeadbandNone                   uint32 = 0
)

// MonitoredItemCreateResult 汇总 CreateMonitoredItems 单项结果。
type MonitoredItemCreateResult struct {
	MonitoredItemId uint32
	Status          StatusCode
}

// ---------------------------------------------------------------------
// CreateSubscription（Part 4 §5.13.2）
// ---------------------------------------------------------------------

type CreateSubscriptionRequest struct {
	RequestHeader
	RequestedPublishingInterval float64
	RequestedLifetimeCount      uint32
	RequestedMaxKeepAliveCount  uint32
	MaxNotificationsPerPublish  uint32
	PublishingEnabled           bool
	Priority                    byte
}

func (r CreateSubscriptionRequest) encodeUA(e *encoder) error {
	if err := r.RequestHeader.encodeUA(e); err != nil {
		return err
	}
	e.f64(r.RequestedPublishingInterval)
	e.u32(r.RequestedLifetimeCount)
	e.u32(r.RequestedMaxKeepAliveCount)
	e.u32(r.MaxNotificationsPerPublish)
	e.bool(r.PublishingEnabled)
	e.u8(r.Priority)
	return nil
}

func decodeCreateSubscriptionRequest(d *decoder) (CreateSubscriptionRequest, error) {
	var r CreateSubscriptionRequest
	var err error
	if r.RequestHeader, err = decodeRequestHeader(d); err != nil {
		return r, err
	}
	if r.RequestedPublishingInterval, err = d.f64(); err != nil {
		return r, err
	}
	if r.RequestedLifetimeCount, err = d.u32(); err != nil {
		return r, err
	}
	if r.RequestedMaxKeepAliveCount, err = d.u32(); err != nil {
		return r, err
	}
	if r.MaxNotificationsPerPublish, err = d.u32(); err != nil {
		return r, err
	}
	if r.PublishingEnabled, err = d.bool(); err != nil {
		return r, err
	}
	if r.Priority, err = d.u8(); err != nil {
		return r, err
	}
	return r, nil
}

type CreateSubscriptionResponse struct {
	ResponseHeader
	SubscriptionId            uint32
	RevisedPublishingInterval float64
	RevisedLifetimeCount      uint32
	RevisedMaxKeepAliveCount  uint32
}

func (r CreateSubscriptionResponse) encodeUA(e *encoder) error {
	if err := r.ResponseHeader.encodeUA(e); err != nil {
		return err
	}
	e.u32(r.SubscriptionId)
	e.f64(r.RevisedPublishingInterval)
	e.u32(r.RevisedLifetimeCount)
	e.u32(r.RevisedMaxKeepAliveCount)
	return nil
}

func decodeCreateSubscriptionResponse(d *decoder) (CreateSubscriptionResponse, error) {
	var r CreateSubscriptionResponse
	var err error
	if r.ResponseHeader, err = decodeResponseHeader(d); err != nil {
		return r, err
	}
	if r.SubscriptionId, err = d.u32(); err != nil {
		return r, err
	}
	if r.RevisedPublishingInterval, err = d.f64(); err != nil {
		return r, err
	}
	if r.RevisedLifetimeCount, err = d.u32(); err != nil {
		return r, err
	}
	if r.RevisedMaxKeepAliveCount, err = d.u32(); err != nil {
		return r, err
	}
	return r, nil
}

// ---------------------------------------------------------------------
// Publish（Part 4 §5.13.5）
// ---------------------------------------------------------------------

// SubscriptionAcknowledgement 内嵌结构（无独立线上 TypeId）。
type SubscriptionAcknowledgement struct {
	SubscriptionId uint32
	SequenceNumber uint32
}

func (a SubscriptionAcknowledgement) encodeUA(e *encoder) error {
	e.u32(a.SubscriptionId)
	e.u32(a.SequenceNumber)
	return nil
}

func decodeSubscriptionAcknowledgement(d *decoder) (SubscriptionAcknowledgement, error) {
	var a SubscriptionAcknowledgement
	var err error
	if a.SubscriptionId, err = d.u32(); err != nil {
		return a, err
	}
	if a.SequenceNumber, err = d.u32(); err != nil {
		return a, err
	}
	return a, nil
}

type PublishRequest struct {
	RequestHeader
	SubscriptionAcks []SubscriptionAcknowledgement
}

func (r PublishRequest) encodeUA(e *encoder) error {
	if err := r.RequestHeader.encodeUA(e); err != nil {
		return err
	}
	e.i32(int32(len(r.SubscriptionAcks)))
	for _, a := range r.SubscriptionAcks {
		if err := a.encodeUA(e); err != nil {
			return err
		}
	}
	return nil
}

func decodePublishRequest(d *decoder) (PublishRequest, error) {
	var r PublishRequest
	var err error
	if r.RequestHeader, err = decodeRequestHeader(d); err != nil {
		return r, err
	}
	n, err := d.i32()
	if err != nil {
		return r, err
	}
	if n >= 0 {
		if n > MaxArrayLength {
			return r, fmt.Errorf("%w: acks length %d exceeds limit %d", ErrTooLong, n, MaxArrayLength)
		}
		for i := int32(0); i < n; i++ {
			a, err := decodeSubscriptionAcknowledgement(d)
			if err != nil {
				return r, err
			}
			r.SubscriptionAcks = append(r.SubscriptionAcks, a)
		}
	}
	// 试解防误吞：Publish 是除头外最短的服务请求，解码后必须无剩余字节。
	// 带扩展体的帧（Browse 等）交回调用方分派后续分支。
	if d.off != len(d.b) {
		return r, fmt.Errorf("%w: Publish trailing bytes (%d/%d)", ErrInvalidEncoding, d.off, len(d.b))
	}
	return r, nil
}

// MonitoredItemNotification 内嵌结构：clientHandle + 采样值。
type MonitoredItemNotification struct {
	ClientHandle uint32
	Value        DataValue
}

func (m MonitoredItemNotification) encodeUA(e *encoder) error {
	e.u32(m.ClientHandle)
	return m.Value.encodeUA(e)
}

func decodeMonitoredItemNotification(d *decoder) (MonitoredItemNotification, error) {
	var m MonitoredItemNotification
	var err error
	if m.ClientHandle, err = d.u32(); err != nil {
		return m, err
	}
	if m.Value, err = decodeDataValue(d); err != nil {
		return m, err
	}
	return m, nil
}

// NotificationMessage 携带一组通知（数据变更 / 状态变更 / 事件列表之一）。
type NotificationMessage struct {
	SequenceNumber   uint32
	PublishTime      DateTime
	NotificationData []NotificationData
}

// NotificationData 是已解码的通知联合体（本轮支持 DataChange 与 StatusChange；
// EventNotificationList 仅占位计数、内容不解码）。
type NotificationData struct {
	Kind              NotificationKind
	DataChange        []MonitoredItemNotification
	StatusChange      StatusCode
	EventElementCount int32 // Kind==NotificationEvent 时的元素个数（事件体跳过）
}

type NotificationKind byte

const (
	NotificationUnknown NotificationKind = iota
	NotificationDataChange
	NotificationStatusChange
	NotificationEvent
)

func (m NotificationMessage) encodeUA(e *encoder) error {
	e.u32(m.SequenceNumber)
	e.i64(int64(m.PublishTime))
	e.i32(int32(len(m.NotificationData)))
	for _, nd := range m.NotificationData {
		switch nd.Kind {
		case NotificationDataChange:
			// ExtObj(TypeId=811): {items[], diagnosticInfos[]}
			var inner encoder
			inner.i32(int32(len(nd.DataChange)))
			for _, it := range nd.DataChange {
				if err := it.encodeUA(&inner); err != nil {
					return err
				}
			}
			inner.i32(-1) // diagnosticInfos: null
			ext := ExtensionObject{
				TypeId:   NewNodeID(0, TypeIdDataChangeNotification),
				Encoding: ExtensionObjectEncodingByteString,
				Body:     inner.buf,
			}
			if err := ext.encodeUA(e); err != nil {
				return err
			}
		case NotificationStatusChange:
			var inner encoder
			inner.u32(uint32(nd.StatusChange))
			inner.i32(-1) // diagnosticInfos
			ext := ExtensionObject{
				TypeId:   NewNodeID(0, TypeIdStatusChangeNotification),
				Encoding: ExtensionObjectEncodingByteString,
				Body:     inner.buf,
			}
			if err := ext.encodeUA(e); err != nil {
				return err
			}
		default:
			return fmt.Errorf("opcua: 未支持的 NotificationData kind %d", nd.Kind)
		}
	}
	return nil
}

// decodeNotificationMessage 解码 PublishResponse.notificationMessage 字段
// （不含前面的序列号前的 header——调用方先解 ResponseHeader 后传入此处剩余体）。
func decodeNotificationBody(d *decoder) (NotificationMessage, error) {
	var m NotificationMessage
	var err error
	if m.SequenceNumber, err = d.u32(); err != nil {
		return m, err
	}
	var ts int64
	if ts, err = d.i64(); err != nil {
		return m, err
	}
	m.PublishTime = DateTime(ts)
	n, err := d.i32()
	if err != nil {
		return m, err
	}
	// PRT-13：负长度未拒绝会跳过解码循环致游标错位，垃圾数据被当
	// 通知投递应用；对齐 decodeVariant/decodeStringList 语义：仅 -1
	// （null 数组）放行为空通知，其余负值一律拒绝。
	if n < 0 && n != -1 {
		return m, fmt.Errorf("%w: negative notificationData length %d", ErrInvalidEncoding, n)
	}
	if n > MaxArrayLength {
		return m, fmt.Errorf("%w: notificationData length %d exceeds limit", ErrTooLong, n)
	}
	for i := int32(0); i < n; i++ {
		ext, err := decodeExtensionObject(d)
		if err != nil {
			return m, err
		}
		nd, err := decodeNotificationExtObj(ext)
		if err != nil {
			return m, err
		}
		m.NotificationData = append(m.NotificationData, nd)
	}
	return m, nil
}

func decodeNotificationExtObj(ext ExtensionObject) (NotificationData, error) {
	var nd NotificationData
	switch ext.TypeId.String() {
	case NewNodeID(0, TypeIdDataChangeNotification).String():
		nd.Kind = NotificationDataChange
		if ext.Encoding == ExtensionObjectEncodingNone {
			return nd, nil
		}
		var dd decoder
		dd.b = ext.Body
		n, err := dd.i32()
		if err != nil {
			return nd, err
		}
		// PRT-13：monitoredItems 负长度拒绝（对齐 notificationData 顶层语义）。
		if n < 0 && n != -1 {
			return nd, fmt.Errorf("%w: negative monitoredItems length %d", ErrInvalidEncoding, n)
		}
		if n > MaxArrayLength {
			return nd, fmt.Errorf("%w: monitoredItems length %d exceeds limit", ErrTooLong, n)
		}
		for i := int32(0); i < n; i++ {
			it, err := decodeMonitoredItemNotification(&dd)
			if err != nil {
				return nd, err
			}
			nd.DataChange = append(nd.DataChange, it)
		}
		// 尾部 diagnosticInfos 列表（可为 null/空）忽略内容但消费长度游标：
		if dn, err := dd.i32(); err != nil {
			return nd, err
		} else if dn > 0 && dn <= MaxArrayLength {
			// DiagnosticInfo 完整解码能力已有：逐个读取以校验结构
			for i := int32(0); i < dn; i++ {
				if _, err := decodeDiagnosticInfo(&dd, 0); err != nil {
					return nd, err
				}
			}
		}
		return nd, nil
	case NewNodeID(0, TypeIdStatusChangeNotification).String():
		nd.Kind = NotificationStatusChange
		if ext.Encoding == ExtensionObjectEncodingNone {
			return nd, nil
		}
		var sd decoder
		sd.b = ext.Body
		st, err := sd.u32()
		if err != nil {
			return nd, err
		}
		nd.StatusChange = StatusCode(st)
		if dn, err := sd.i32(); err != nil {
			return nd, err
		} else if dn > 0 && dn <= MaxArrayLength {
			for i := int32(0); i < dn; i++ {
				if _, err := decodeDiagnosticInfo(&sd, 0); err != nil {
					return nd, err
				}
			}
		} else if dn < -1 {
			// PRT-13：StatusChange diagnosticInfos 负长度拒绝（-1=null 放行）。
			return nd, fmt.Errorf("%w: negative diagnosticInfos length %d", ErrInvalidEncoding, dn)
		}
		return nd, nil
	case NewNodeID(0, TypeIdEventNotificationList).String():
		nd.Kind = NotificationEvent
		if ext.Encoding == ExtensionObjectEncodingNone {
			return nd, nil
		}
		var ed decoder
		ed.b = ext.Body
		cnt, err := ed.i32()
		if err != nil {
			return nd, err
		}
		nd.EventElementCount = cnt
		// 事件体（排除项 Alarm&Condition）：不解码内容。长度未知，直接返回，
		// 由调用方容忍尾部未消费字节。
		return nd, nil
	default:
		return nd, fmt.Errorf("opcua: 未支持的 NotificationMessage 类型 %s", ext.TypeId.String())
	}
}

type PublishResponse struct {
	ResponseHeader
	SubscriptionId           uint32
	AvailableSequenceNumbers []uint32
	MoreNotifications        bool
	NotificationMessage      NotificationMessage
	Results                  []StatusCode
	DiagnosticInfos          []DiagnosticInfo
}

func (r PublishResponse) encodeUA(e *encoder) error {
	if err := r.ResponseHeader.encodeUA(e); err != nil {
		return err
	}
	e.u32(r.SubscriptionId)
	e.i32(int32(len(r.AvailableSequenceNumbers)))
	for _, s := range r.AvailableSequenceNumbers {
		e.u32(s)
	}
	e.bool(r.MoreNotifications)
	if err := r.NotificationMessage.encodeUA(e); err != nil {
		return err
	}
	e.i32(int32(len(r.Results)))
	for _, s := range r.Results {
		e.u32(uint32(s))
	}
	encodeDiagnosticInfoList(e, r.DiagnosticInfos)
	return nil
}

func decodePublishResponse(d *decoder) (PublishResponse, error) {
	var r PublishResponse
	var err error
	if r.ResponseHeader, err = decodeResponseHeader(d); err != nil {
		return r, err
	}
	if r.SubscriptionId, err = d.u32(); err != nil {
		return r, err
	}
	an, err := d.i32()
	if err != nil {
		return r, err
	}
	if an >= 0 {
		if an > MaxArrayLength {
			return r, fmt.Errorf("%w: seq list length %d exceeds limit", ErrTooLong, an)
		}
		for i := int32(0); i < an; i++ {
			v, err := d.u32()
			if err != nil {
				return r, err
			}
			r.AvailableSequenceNumbers = append(r.AvailableSequenceNumbers, v)
		}
	}
	if r.MoreNotifications, err = d.bool(); err != nil {
		return r, err
	}
	if r.NotificationMessage, err = decodeNotificationBody(d); err != nil {
		return r, err
	}
	rn, err := d.i32()
	if err != nil {
		return r, err
	}
	if rn >= 0 {
		if rn > MaxArrayLength {
			return r, fmt.Errorf("%w: results length %d exceeds limit", ErrTooLong, rn)
		}
		for i := int32(0); i < rn; i++ {
			v, err := d.u32()
			if err != nil {
				return r, err
			}
			r.Results = append(r.Results, StatusCode(v))
		}
	}
	if r.DiagnosticInfos, err = decodeDiagnosticInfoList(d); err != nil {
		return r, err
	}
	return r, nil
}

// ---------------------------------------------------------------------
// DeleteSubscriptions（Part 4 §5.13.7）
// ---------------------------------------------------------------------

type DeleteSubscriptionsRequest struct {
	RequestHeader
	SubscriptionIds []uint32
}

func (r DeleteSubscriptionsRequest) encodeUA(e *encoder) error {
	if err := r.RequestHeader.encodeUA(e); err != nil {
		return err
	}
	e.i32(int32(len(r.SubscriptionIds)))
	for _, id := range r.SubscriptionIds {
		e.u32(id)
	}
	return nil
}

func decodeDeleteSubscriptionsRequest(d *decoder) (DeleteSubscriptionsRequest, error) {
	var r DeleteSubscriptionsRequest
	var err error
	if r.RequestHeader, err = decodeRequestHeader(d); err != nil {
		return r, err
	}
	n, err := d.i32()
	if err != nil {
		return r, err
	}
	if n >= 0 && n <= MaxArrayLength {
		for i := int32(0); i < n; i++ {
			v, err := d.u32()
			if err != nil {
				return r, err
			}
			r.SubscriptionIds = append(r.SubscriptionIds, v)
		}
	}
	return r, nil
}

type DeleteSubscriptionsResponse struct {
	ResponseHeader
	Results         []StatusCode
	DiagnosticInfos []DiagnosticInfo
}

func (r DeleteSubscriptionsResponse) encodeUA(e *encoder) error {
	if err := r.ResponseHeader.encodeUA(e); err != nil {
		return err
	}
	e.i32(int32(len(r.Results)))
	for _, s := range r.Results {
		e.u32(uint32(s))
	}
	encodeDiagnosticInfoList(e, r.DiagnosticInfos)
	return nil
}

func decodeDeleteSubscriptionsResponse(d *decoder) (DeleteSubscriptionsResponse, error) {
	var r DeleteSubscriptionsResponse
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
			v, err := d.u32()
			if err != nil {
				return r, err
			}
			r.Results = append(r.Results, StatusCode(v))
		}
	}
	if r.DiagnosticInfos, err = decodeDiagnosticInfoList(d); err != nil {
		return r, err
	}
	return r, nil
}

// ---------------------------------------------------------------------
// CreateMonitoredItems（Part 4 §5.13.2。仅 Value 属性 + DataChange 过滤语义）
// ---------------------------------------------------------------------

// MonitoringFilterDescription 本轮固定使用 DataChangeFilter 死区 None，
// 即“每次值变化即通知”；filter 以 ExtObj(724) 落线。
type ItemToCreate struct {
	NodeId             NodeId
	ClientHandle       uint32
	SamplingIntervalMs float64
	QueueSize          uint32
}

type CreateMonitoredItemsRequest struct {
	RequestHeader
	SubscriptionId     uint32
	TimestampsToReturn int32
	ItemsToCreate      []ItemToCreate
}

// dataChangeFilterExtObj 构造 DataChangeFilter(724) ExtObj：
// {trigger=StatusValueTimestamp, deadband=None} —— 值变化即通知。
func dataChangeFilterExtObj() ExtensionObject {
	var fe encoder
	fe.u32(DataChangeTriggerStatusValueTs)
	fe.u32(DeadbandNone)
	return ExtensionObject{
		TypeId:   NewNodeID(0, TypeIdDataChangeFilter),
		Encoding: ExtensionObjectEncodingByteString,
		Body:     fe.buf,
	}
}

func (r CreateMonitoredItemsRequest) encodeUA(e *encoder) error {
	if err := r.RequestHeader.encodeUA(e); err != nil {
		return err
	}
	e.u32(r.SubscriptionId)
	e.i32(r.TimestampsToReturn)
	e.i32(int32(len(r.ItemsToCreate)))
	for _, it := range r.ItemsToCreate {
		// MonitoredItemCreateRequest 字段序（Part 4 §5.13.2.2）：
		// itemToMonitor{nodeId, attributeId, indexRange}
		// monitoringMode(u32)
		// monitoringParameters{clientHandle, samplingInterval, filter, queueSize, discardOldest}
		if err := it.NodeId.encodeUA(e); err != nil {
			return err
		}
		e.u32(AttributeIdValue)
		e.i32(-1) // indexRange: null字符串
		e.u32(MonitoringModeReporting)
		e.u32(it.ClientHandle)
		e.f64(it.SamplingIntervalMs)
		if err := dataChangeFilterExtObj().encodeUA(e); err != nil {
			return err
		}
		e.u32(it.QueueSize)
		e.bool(true) // discardOldest
	}
	return nil
}

// ---------------------------------------------------------------------
// MonitoredItem 结果与 DeleteMonitoredItems（本轮仅最小集）
// ---------------------------------------------------------------------

// MonitoredItemCreateResultDetail 对应服务端单项结果。
type MonitoredItemCreateResultDetail struct {
	MonitoredItemId uint32
	StatusCode      StatusCode
}

func decodeMonitoredItemCreateResult(d *decoder) (MonitoredItemCreateResultDetail, error) {
	var r MonitoredItemCreateResultDetail
	var err error
	st, serr := d.u32()
	if serr != nil {
		return r, serr
	}
	r.StatusCode = StatusCode(st)
	if r.MonitoredItemId, err = d.u32(); err != nil {
		return r, err
	}
	// revisedSamplingInterval(f64) + revisedQueueSize(u32) + filter(ExtObj)
	if _, err = d.f64(); err != nil {
		return r, err
	}
	if _, err = d.u32(); err != nil {
		return r, err
	}
	if _, err = decodeExtensionObject(d); err != nil {
		return r, err
	}
	return r, nil
}

type CreateMonitoredItemsResponse struct {
	ResponseHeader
	Results         []MonitoredItemCreateResultDetail
	DiagnosticInfos []DiagnosticInfo
}

func (r CreateMonitoredItemsResponse) encodeUA(e *encoder) error {
	if err := r.ResponseHeader.encodeUA(e); err != nil {
		return err
	}
	e.i32(int32(len(r.Results)))
	// v0.23.0 修复（主线，B 路新测试暴露）：逐项编码结果结构体实际值——
	// 原实现无视输入硬编码 0（statusCode/itemId 恒 Good/0），服务端侧
	// 永远无法表达 Bad 结果；revised 参数与 filter 仍按占位零值编码。
	for _, res := range r.Results {
		e.u32(uint32(res.StatusCode))
		e.u32(res.MonitoredItemId)
		e.f64(0)
		e.u32(0)
		err := (ExtensionObject{TypeId: NewNodeID(0, 0), Encoding: ExtensionObjectEncodingNone}).encodeUA(e)
		if err != nil {
			return err
		}
	}
	encodeDiagnosticInfoList(e, r.DiagnosticInfos)
	return nil
}

func decodeCreateMonitoredItemsResponse(d *decoder) (CreateMonitoredItemsResponse, error) {
	var r CreateMonitoredItemsResponse
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
			item, derr := decodeMonitoredItemCreateResult(d)
			if derr != nil {
				return r, derr
			}
			r.Results = append(r.Results, item)
		}
	}
	if r.DiagnosticInfos, err = decodeDiagnosticInfoList(d); err != nil {
		return r, err
	}
	return r, nil
}

// DeleteMonitoredItemsRequest 本轮未用（重建走 DeleteSubscription 整删），
// 结构仅保留给 mock 测试对称使用。
type DeleteMonitoredItemsRequest struct {
	RequestHeader
	SubscriptionId   uint32
	MonitoredItemIds []uint32
}

func decodeDeleteMonitoredItemsRequest(d *decoder) (DeleteMonitoredItemsRequest, error) { //nolint:unused
	var r DeleteMonitoredItemsRequest
	var err error
	if r.RequestHeader, err = decodeRequestHeader(d); err != nil {
		return r, err
	}
	if r.SubscriptionId, err = d.u32(); err != nil {
		return r, err
	}
	n, err := d.i32()
	if err != nil {
		return r, err
	}
	if n >= 0 && n <= MaxArrayLength {
		for i := int32(0); i < n; i++ {
			v, err := d.u32()
			if err != nil {
				return r, err
			}
			r.MonitoredItemIds = append(r.MonitoredItemIds, v)
		}
	}
	return r, nil
}

type DeleteMonitoredItemsResponse struct {
	ResponseHeader
	Results         []StatusCode
	DiagnosticInfos []DiagnosticInfo
}

func (r DeleteMonitoredItemsResponse) encodeUA(e *encoder) error { //nolint:unused
	if err := r.ResponseHeader.encodeUA(e); err != nil {
		return err
	}
	e.i32(int32(len(r.Results)))
	for _, s := range r.Results {
		e.u32(uint32(s))
	}
	encodeDiagnosticInfoList(e, r.DiagnosticInfos)
	return nil
}

func decodeDeleteMonitoredItemsResponse(d *decoder) (DeleteMonitoredItemsResponse, error) { //nolint:unused
	var r DeleteMonitoredItemsResponse
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
			v, err := d.u32()
			if err != nil {
				return r, err
			}
			r.Results = append(r.Results, StatusCode(v))
		}
	}
	if r.DiagnosticInfos, err = decodeDiagnosticInfoList(d); err != nil {
		return r, err
	}
	return r, nil
}

func decodeCreateMonitoredItemsRequest(d *decoder) (CreateMonitoredItemsRequest, error) {
	var r CreateMonitoredItemsRequest
	var err error
	if r.RequestHeader, err = decodeRequestHeader(d); err != nil {
		return r, err
	}
	if r.SubscriptionId, err = d.u32(); err != nil {
		return r, err
	}
	if r.TimestampsToReturn, err = d.i32(); err != nil {
		return r, err
	}
	n, err := d.i32()
	if err != nil {
		return r, err
	}
	if n >= 0 && n <= MaxArrayLength {
		for i := int32(0); i < n; i++ {
			// MonitoredItemCreateRequest：itemToMonitor + monitoringMode + monitoringParameters
			var it ItemToCreate
			if it.NodeId, err = decodeNodeID(d); err != nil {
				return r, err
			}
			if _, err = d.u32(); err != nil { // attributeId（本轮恒 Value=13）
				return r, err
			}
			if _, err = d.i32(); err != nil { // indexRange null/字符串
				return r, err
			}
			if _, err = d.u32(); err != nil { // monitoringMode
				return r, err
			}
			if it.ClientHandle, err = d.u32(); err != nil { // parameters.clientHandle
				return r, err
			}
			if it.SamplingIntervalMs, err = d.f64(); err != nil {
				return r, err
			}
			if _, err = decodeExtensionObject(d); err != nil { // filter
				return r, err
			}
			if it.QueueSize, err = d.u32(); err != nil {
				return r, err
			}
			if _, err = d.bool(); err != nil { // discardOldest
				return r, err
			}
			r.ItemsToCreate = append(r.ItemsToCreate, it)
		}
	}
	return r, nil
}
