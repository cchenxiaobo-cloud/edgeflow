// v0.6.0「真多活」扩展面：ExtendedKV 聚合接口 + 装配断言。
//
// 设计定稿 §12.1：KVStore（v0.4.0 冻结面）不动，外部模式装配要求 kv
// 额外满足 Lease + Atomic + Watch 三个扩展面；embed 模式的 kvStore 同样
// 实现（CAS 统一走同一实现，行为等价——D4），但 watch/租约仅在外部模式
// 被装配消费。
package etcdstore

// ExtendedKV 是外部模式装配要求的完整 KV 面：
// KVStore（既有冻结面）+ LeaseKV + AtomicKV + WatchKV。
type ExtendedKV interface {
	KVStore
	LeaseKV
	AtomicKV
	WatchKV
}

// 编译期断言：*kvStore 满足完整扩展面。
var _ ExtendedKV = (*kvStore)(nil)

// AsExtended 断言 kv 是否满足 ExtendedKV（外部模式装配用）：
// 不满足 → fail-fast（理论不可达：etcdstore 自身实现恒满足；防御断言）。
func AsExtended(kv KVStore) (ExtendedKV, bool) {
	e, ok := kv.(ExtendedKV)
	return e, ok
}