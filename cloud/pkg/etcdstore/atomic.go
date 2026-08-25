// v0.6.0「真多活」扩展面：CAS 原子原语（clientv3 Txn）。
//
// 多副本写并发安全的两块基石（设计定稿 §1.5/§3/§5.1 + 风险审稿 R4-1/R5-1）：
//   - CompareAndPut：SetDesired / Register 的读-改-写串行化（ModRevision 等值
//     比较；expectRev=0 → create-if-absent）；
//   - CompareAndDelete / GuardedDelete：GC 守卫删除（rev 不匹配 = 键已被重写/
//     重注册 → 拒绝删除；guard 键存在活租约 = 节点其实活着 → 拒绝删除）。
//
// 消费面接口 KVStore 签名冻结（v0.4.0 契约），本文件以独立接口扩展方式
// 追加（设计 §0.1-D5 修订：扩展以新接口/新方法追加），由 AsExtended 在
// 装配层断言。
package etcdstore

import (
	"context"
	"fmt"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// AtomicKV 是 CAS 原子原语扩展面（外部模式装配要求 kv 满足）。
type AtomicKV interface {
	// GetWithRev 返回键值与 ModRevision；键不存在 → (nil, 0, nil)。
	GetWithRev(ctx context.Context, key string) ([]byte, int64, error)

	// CompareAndPut 是「读-改-写」串行化的提交原语：
	//   - expectRev > 0：Compare(ModRevision(key) == expectRev) → Put。
	//     命中 → (true, nil)；冲突（他写者已提交）→ (false, nil)，不写；
	//   - expectRev == 0：create-if-absent，Compare(CreateRevision(key) == 0)
	//     （键从未存在 / 已被删除——见 atomic_test.go 对删除后重建键
	//     CreateRevision 语义的实测锁定）→ Put。
	// 其他错误（网络等）→ (false, err)。
	CompareAndPut(ctx context.Context, key string, value []byte, expectRev int64) (bool, error)

	// CompareAndDelete 是带版本守卫的删除（GC 用，R4-1）：
	// Compare(ModRevision(key) == expectRev) → Delete(key)。
	// deleted=true 表示「调用后 key 已不存在」（本次删除；expectRev==0 且本就
	// 缺失时等值命中，视同完成）；deleted=false 表示守卫阻止（rev 已被重写 =
	// 节点重注册/他副本刚写过）→ 调用方必须放弃删除并恢复。
	CompareAndDelete(ctx context.Context, key string, expectRev int64) (deleted bool, err error)

	// GuardedDelete 是心跳键守卫删除（设计 §1.5 最后一道闸）：
	// Txn{ If: Compare(Lease(guardKey) == 0), Then: Delete(targetKey) }。
	// guardKey（心跳键）存在活租约 → (false, nil)，调用方必须撤销删除
	// （节点其实活着）；否则执行 Delete（幂等，target 本就缺失视同完成）。
	GuardedDelete(ctx context.Context, guardKey, targetKey string) (deleted bool, err error)
}

// 编译期断言：*kvStore 满足扩展面。
var _ AtomicKV = (*kvStore)(nil)

// GetWithRev 实现 AtomicKV（见接口注释）。
func (s *kvStore) GetWithRev(ctx context.Context, key string) ([]byte, int64, error) {
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	resp, err := s.kv.Get(cctx, key)
	if err != nil {
		return nil, 0, err
	}
	if len(resp.Kvs) == 0 {
		return nil, 0, nil // 不存在：nil, 0, nil
	}
	return append([]byte(nil), resp.Kvs[0].Value...), resp.Kvs[0].ModRevision, nil
}

// CompareAndPut 实现 AtomicKV（见接口注释）。
func (s *kvStore) CompareAndPut(ctx context.Context, key string, value []byte, expectRev int64) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	var cmp clientv3.Cmp
	if expectRev == 0 {
		// create-if-absent：键的 CreateRevision == 0 ⇔ 键不存在（含删除后未重建）。
		// 实测锁定：etcd 删除键后再重建会分配全新 CreateRevision（不恢复旧值），
		// 故该比较等价于「当前不存在」（见 atomic_test.go TestCompareAndPutCreateIfAbsent）。
		cmp = clientv3.Compare(clientv3.CreateRevision(key), "=", 0)
	} else {
		cmp = clientv3.Compare(clientv3.ModRevision(key), "=", expectRev)
	}
	txn := s.kv.Txn(cctx)
	resp, err := txn.If(cmp).Then(clientv3.OpPut(key, string(value))).Commit()
	if err != nil {
		return false, err
	}
	return resp.Succeeded, nil
}

// CompareAndDelete 实现 AtomicKV（见接口注释）。
func (s *kvStore) CompareAndDelete(ctx context.Context, key string, expectRev int64) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	cmp := clientv3.Compare(clientv3.ModRevision(key), "=", expectRev)
	resp, err := s.kv.Txn(cctx).If(cmp).Then(clientv3.OpDelete(key)).Commit()
	if err != nil {
		return false, err
	}
	return resp.Succeeded, nil
}

// GuardedDelete 实现 AtomicKV（见接口注释）。
func (s *kvStore) GuardedDelete(ctx context.Context, guardKey, targetKey string) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	cmp := clientv3.Compare(clientv3.LeaseValue(guardKey), "=", 0)
	resp, err := s.kv.Txn(cctx).If(cmp).Then(clientv3.OpDelete(targetKey)).Commit()
	if err != nil {
		return false, fmt.Errorf("etcdstore: GuardedDelete(%s, %s) txn 失败: %w", guardKey, targetKey, err)
	}
	return resp.Succeeded, nil
}
