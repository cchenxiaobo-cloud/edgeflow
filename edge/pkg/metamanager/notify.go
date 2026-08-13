// Pod 变更订阅（增量变更通知机制，M2 前置：Edged 增量消费 Pod 元数据）。
//
// 动机：Edged 启动时用 ListPods 全量对账，运行期需要感知增量变更
// （新 Pod 下发 / Pod 删除），避免轮询 SQLite。本文件给 Store 增加
// 发布/订阅能力：
//   - Subscribe 注册一个订阅者，返回订阅 ID 与只读事件通道；
//   - SavePod/DeletePod 成功落盘后向所有订阅者广播 Event
//     （pod.upsert / pod.delete 两类，见事件语义）；
//   - Unsubscribe 注销订阅（事件通道会被关闭，消费方可据此退出循环）。
//
// 事件语义：
//   - pod.upsert：Pod 已落盘。add 与 update 不区分（SavePod 本身是
//     幂等覆盖写，消费方按 namespace+name 覆盖即可，无需关心是新建
//     还是更新——区分只会增加无意义的重复逻辑）；
//   - pod.delete：Pod 已删除，Value 为空串（删除事件不携带旧值：
//     旧值对消费方无增量意义，如需可在删除前自行 ListPods）。
//
// 背压策略（重要）：事件投递是非阻塞的，订阅者消费不及时则丢事件
// （慢消费者丢弃）。理由：SavePod/DeletePod 位于 EdgeHub 消息处理路径
// 上，阻塞会拖慢 Ack 返回（云端 5s 超时即重试）；且丢事件可被全量
// 对账兜底（消费方可在启动或断点处用 ListPods 重建全量状态）。
// 消费方应保证通道缓冲充足 / 及时消费，不要把本机制当作可靠事件流。
package metamanager

// 事件类型常量。
const (
	// EventPodUpsert 表示 Pod 已落盘（新增或覆盖更新，见文件头说明）。
	EventPodUpsert = "pod.upsert"
	// EventPodDelete 表示 Pod 已删除。
	EventPodDelete = "pod.delete"
)

// defaultSubscribeBuffer 是订阅事件通道的默认缓冲大小。
const defaultSubscribeBuffer = 100

// Event 是一条 Pod 变更事件。
type Event struct {
	Type      string // 事件类型：EventPodUpsert / EventPodDelete
	Namespace string // Pod 命名空间（与落盘 key 一致，缺省为 "default"）
	Name      string // Pod 名称
	Value     string // Pod JSON（delete 事件为空串）
}

// SubscribeOptions 是 Subscribe 的注入参数；零值使用默认值。
type SubscribeOptions struct {
	// BufferSize 是事件通道的缓冲大小（<=0 时用默认 100）。
	// 缓冲满时新事件被丢弃（见文件头背压策略），不会阻塞写路径。
	BufferSize int
}

// subscriber 是一个订阅者的内部状态：通道 + 是否仍在订阅表中。
// 是否可写由「是否还在 subscribers map 中」决定（见 Unsubscribe/notify
// 的互斥约定），因此这里只需要通道本身。
type subscriber struct {
	ch chan Event
}

// Subscribe 注册一个 Pod 变更订阅者，返回订阅 ID 与只读事件通道。
// 订阅后，SavePod/DeletePod 成功落盘会产生事件（见 notify.go 文件头）。
// 用 Unsubscribe(id) 注销；注销后事件通道被关闭（消费方可据此退出）。
// 返回的 error 位保留供未来扩展（如 Store 已关闭时拒绝订阅），当前恒为 nil。
func (s *Store) Subscribe(opts SubscribeOptions) (int, <-chan Event, error) {
	if opts.BufferSize <= 0 {
		opts.BufferSize = defaultSubscribeBuffer
	}
	s.subMu.Lock()
	defer s.subMu.Unlock()
	if s.subscribers == nil {
		s.subscribers = make(map[int]*subscriber)
	}
	s.nextSubID++
	id := s.nextSubID
	sub := &subscriber{ch: make(chan Event, opts.BufferSize)}
	s.subscribers[id] = sub
	return id, sub.ch, nil
}

// Unsubscribe 注销订阅：从订阅者表移除并关闭事件通道（消费方可据此退出）。
// 重复注销 / 未知 ID 静默成功（幂等）。
//
// 并发安全：与广播（notify）互斥于 subMu——notify 只在持有 subMu 时向
// 仍在订阅表中的通道发送（非阻塞），Unsubscribe 也在持有 subMu 时删除
// 并关闭，因此不存在「发送时通道已被关闭」的竞态。
func (s *Store) Unsubscribe(id int) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	if sub, ok := s.subscribers[id]; ok {
		delete(s.subscribers, id)
		close(sub.ch)
	}
}

// notifyPodUpsert 广播一条 Pod 落盘事件（SavePod 成功落盘后调用）。
func (s *Store) notifyPodUpsert(namespace, name, value string) {
	s.notify(Event{Type: EventPodUpsert, Namespace: namespace, Name: name, Value: value})
}

// notifyPodDelete 广播一条 Pod 删除事件（DeletePod 成功删除后调用）。
func (s *Store) notifyPodDelete(namespace, name string) {
	s.notify(Event{Type: EventPodDelete, Namespace: namespace, Name: name})
}

// notify 向全部订阅者广播事件（非阻塞投递，慢消费者丢弃，见文件头背压策略）。
// 持有 subMu 期间只做非阻塞通道写（select+default 永不阻塞），
// 因此广播本身不会阻塞 SavePod/DeletePod 的调用路径。
func (s *Store) notify(ev Event) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for _, sub := range s.subscribers {
		select {
		case sub.ch <- ev:
		default:
			// 通道已满：订阅者消费不及时，丢弃本事件（可被全量对账兜底）
		}
	}
}
