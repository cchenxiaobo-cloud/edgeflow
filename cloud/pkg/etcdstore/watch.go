// v0.6.0「真多活」扩展面：watch 缓存同步（设计定稿 §2）。
//
// 多副本读一致 = 启动全量 Load（revision 锚定）→ watch 从 anchorRev+1 增量
// → 断线全量重放。本文件提供锚定前缀扫描 + 前缀 watch（含错误/终止事件化）。
package etcdstore

import (
	"context"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// WatchEventType 是 watch 事件类型。
type WatchEventType int

// watch 事件类型（WatchEventErr 为终止/错误哨兵：任何终止——连接断开、
// ErrCompacted、ctx 取消——都先发一条 Err（Value 携带错误文本）再关通道，
// 应用器据此区分「正常流」与「重放信号」，统一走全量重放，幂等）。
const (
	WatchEventPut WatchEventType = iota
	WatchEventDelete
	WatchEventErr
)

// WatchEvent 是 WatchPrefix 投递的单个事件。
type WatchEvent struct {
	Type        WatchEventType
	Key         string
	Value       []byte // Delete 事件为 nil
	ModRevision int64
}

// WatchKV 是 watch 缓存同步扩展面（外部模式装配要求 kv 满足）。
type WatchKV interface {
	// ListByPrefixRev 前缀扫描 + 返回响应头 revision（load 锚点）。
	// 锚定契约：返回 rev 之后的所有事件都 ≥ rev+1，watch 从 rev+1 起步不漏事件。
	ListByPrefixRev(ctx context.Context, prefix string) ([]KVEntry, int64, error)

	// WatchPrefix 订阅前缀事件。startRev>0 含该 revision 起（调用方传
	// anchorRev+1）；==0 仅后续新事件。事件按 revision 升序；任何终止/错误
	// （含 ctx 取消）：先发 WatchEvent{WatchEventErr} 再关通道。
	WatchPrefix(ctx context.Context, prefix string, startRev int64) <-chan WatchEvent
}

// 编译期断言：*kvStore 满足扩展面。
var _ WatchKV = (*kvStore)(nil)

// ListByPrefixRev 实现 WatchKV（见接口注释）。
func (s *kvStore) ListByPrefixRev(ctx context.Context, prefix string) ([]KVEntry, int64, error) {
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	resp, err := s.kv.Get(cctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, 0, err
	}
	rev := int64(0)
	if resp.Header != nil {
		rev = resp.Header.Revision
	}
	entries := make([]KVEntry, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		entries = append(entries, KVEntry{
			Key:      string(kv.Key),
			Value:    append([]byte(nil), kv.Value...),
			Revision: kv.ModRevision,
		})
	}
	return entries, rev, nil
}

// WatchPrefix 实现 WatchKV（见接口注释）。
//
// 实现说明：clientv3 Watch 通道在连接错误/compaction/ctx 取消时关闭——
// 这是唯一可靠的终止信号（事件本身不带错误面）。在独立 goroutine 中转发：
// 收到关闭 → 先发 WatchEventErr 再 close(out)，使应用器拿到「重放信号」。
func (s *kvStore) WatchPrefix(ctx context.Context, prefix string, startRev int64) <-chan WatchEvent {
	out := make(chan WatchEvent)
	go func() {
		defer close(out)
		wch := s.watchClient().Watch(ctx, prefix, clientv3.WithPrefix(), clientv3.WithRev(startRev))
		for {
			select {
			case <-ctx.Done():
				out <- WatchEvent{Type: WatchEventErr, Value: []byte(ctx.Err().Error())}
				return
			case resp, ok := <-wch:
				if !ok {
					// clientv3 通道关闭（连接断开/客户端错误）：Err 事件 → 应用器重放
					out <- WatchEvent{Type: WatchEventErr, Value: []byte("watch channel closed")}
					return
				}
				if err := resp.Err(); err != nil {
					// ErrCompacted（startRev 早于压缩点）等 watch 级错误 → 重放信号
					out <- WatchEvent{Type: WatchEventErr, Value: []byte(err.Error())}
					return
				}
				for _, ev := range resp.Events {
					t := WatchEventPut
					if ev.Type == clientv3.EventTypeDelete {
						t = WatchEventDelete
					}
					var val []byte
					if ev.Kv != nil {
						val = append([]byte(nil), ev.Kv.Value...)
					}
					out <- WatchEvent{
						Type:        t,
						Key:         string(ev.Kv.Key),
						Value:       val,
						ModRevision: ev.Kv.ModRevision,
					}
				}
			}
		}
	}()
	return out
}

// StartRevision 是给应用器的锚定辅助：anchorRev 之后的首个可订阅 revision。
func StartRevision(anchorRev int64) int64 { return anchorRev + 1 }