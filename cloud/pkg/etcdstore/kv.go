package etcdstore

import (
	"context"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// KVEntry 是 ListByPrefix 返回的单个键值对。
type KVEntry struct {
	Key   string
	Value []byte
}

// KVStore 是 etcdstore 对外固定的 KV 接口契约（供 registry/devicestatus
// 等消费方实现对接；本文件签名冻结，一字不改）。
//
// 语义约定：
//   - Put：覆盖写，单次操作 ctx 超时 5s；
//   - Get：未找到返回 (nil, nil)；
//   - ListByPrefix：返回按字节序排序的前缀命中集合（etcd 原生保证）；
//   - DeleteRange：删除前缀下全部键。
type KVStore interface {
	Put(ctx context.Context, key string, value []byte) error // 覆盖写；ctx 5s 超时
	Get(ctx context.Context, key string) ([]byte, error)     // 未找到返回 nil, nil
	ListByPrefix(ctx context.Context, prefix string) ([]KVEntry, error)
	Delete(ctx context.Context, key string) error
	DeleteRange(ctx context.Context, prefix string) error
	Close() error
}

// opTimeout 是单次读写操作的超时上限（对齐接口注释 5s 约定）。
const opTimeout = 5 * time.Second

// kvStore 是 KVStore 的 clientv3 薄封装（Put/Get/prefix Range/Delete/WithPrefix）。
type kvStore struct {
	client *clientv3.Client
	kv     clientv3.KV
}

// 编译期断言：*kvStore 满足固定接口。
var _ KVStore = (*kvStore)(nil)

// NewKVStore 创建指向任意 endpoints 的 KVStore。
// v0.5+ 外部 etcd 模式下跳过 embed、直接用本工厂连接 EDGEFLOW_CLOUDCORE_ETCD_ENDPOINTS
// 指向的集群（设计 §7：业务层零改动）。
func NewKVStore(endpoints []string) (KVStore, error) {
	client, err := clientv3.New(clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: opTimeout,
	})
	if err != nil {
		return nil, err
	}
	return &kvStore{client: client, kv: clientv3.NewKV(client)}, nil
}

// NewEmbeddedKV 一站式工厂：启动嵌入式 etcd（Start）并返回其 KVStore。
// 任一环节失败：关闭已启动资源并返回 error（调用方按 Config.Strict 决策降级/fail-fast）。
func NewEmbeddedKV(cfg Config) (*EmbeddedEtcd, KVStore, error) {
	et, err := Start(cfg)
	if err != nil {
		return nil, nil, err
	}
	kv, err := NewKVStore([]string{et.ClientURL()})
	if err != nil {
		et.Close()
		return nil, nil, err
	}
	return et, kv, nil
}

func (s *kvStore) Put(ctx context.Context, key string, value []byte) error {
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	_, err := s.kv.Put(cctx, key, string(value))
	return err
}

func (s *kvStore) Get(ctx context.Context, key string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	resp, err := s.kv.Get(cctx, key)
	if err != nil {
		return nil, err
	}
	if len(resp.Kvs) == 0 {
		return nil, nil // 未找到：nil, nil
	}
	return resp.Kvs[0].Value, nil
}

func (s *kvStore) ListByPrefix(ctx context.Context, prefix string) ([]KVEntry, error) {
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	resp, err := s.kv.Get(cctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	entries := make([]KVEntry, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		// 拷贝 Value，避免共享 mvcc 底层缓冲。
		entries = append(entries, KVEntry{Key: string(kv.Key), Value: append([]byte(nil), kv.Value...)})
	}
	return entries, nil
}

func (s *kvStore) Delete(ctx context.Context, key string) error {
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	_, err := s.kv.Delete(cctx, key)
	return err
}

func (s *kvStore) DeleteRange(ctx context.Context, prefix string) error {
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	_, err := s.kv.Delete(cctx, prefix, clientv3.WithPrefix())
	return err
}

func (s *kvStore) Close() error { return s.client.Close() }
