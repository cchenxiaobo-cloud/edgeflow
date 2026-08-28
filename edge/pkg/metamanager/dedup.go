// 幂等去重键持久化（CHN-03 / WBS 4.6 可靠投递·边缘侧持久化）。
//
// 背景（CHN-03）：EdgeHub 的幂等去重键此前仅存内存（edge/pkg/edgehub/ack.go
// 的 processed map + FIFO 队列），edgecore 重启后缓存清空——云端对「已成功
// 处理但 Ack 未达」的消息按 QoS 1 重试同 ID，边缘会重复执行（如重复落盘
// Pod 元数据）。本文件把去重键持久化到 metamanager SQLite，重启后去重窗口
// 不丢，云端重试同 ID 消息被直接去重（回 Ack 不再执行）。
//
// 持久层设计：
//   - 独立表 dedup_keys（复用 EdgeHub Client 的 *metamanager.Store 单连接），
//     msg_id TEXT PRIMARY KEY 天然幂等：INSERT OR IGNORE 并发/重复写不冲突；
//   - TTL：expires_at（毫秒）索引列，写入时按 DedupTTL（默认 24h）展期；
//     已过期键视为「未处理过」（IsProcessed 返回 false）；
//   - 容量上限与淘汰：超过 DedupMaxKeys 时按 expires_at 升序淘汰最旧一批
//     （LRU-by-expiry：最旧先过期先淘汰，语义与 FIFO 淘汰一致）；
//   - TTL 清理时机：① 后台清理协程每 DedupCleanupInterval（默认 1h）批量
//     DELETE 过期行；② 每次写入后的慢路径维护（见 DedupStore.maybeMaintain）
//     达到写入间隔阈值时顺带清一次；③ 进程启动时清理一次（重启即回收）。
//
// 主路径不阻塞（验收 3）：IsProcessed/MarkProcessed 同步路径只访问内存
// SQLite（单行 PRIMARY KEY 点查/写，微秒级，与 MetaManager 既有落盘路径
// 同量级，不构成额外阻塞点）；批量淘汰与范围 DELETE（可能锁表毫秒级）
// 全部移出关键路径：写入后按间隔阈值异步触发，由独立维护协程执行，
// 失败只记日志、不影响消息处理。
package metamanager

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"edgeflow/pkg/log"
)

// 去重键持久化常量（容量/TTL/清理时机——验收 4 的参数表）。
const (
	// DedupTTL 是去重键的存活时长（验收：24h）。语义：一条消息成功处理
	// 后，其 ID 在 24h 内再次到达会被去重；超过 24h 后视为新消息。
	// 量级依据：云端重试窗口（指数退避，分钟级）远小于 24h；24h 覆盖
	// 「边缘断网一天、恢复后云端补投」的极端场景。
	DedupTTL = 24 * time.Hour

	// DedupMaxKeys 是去重键的容量上限（条数）。超出后按过期时间升序
	// 淘汰最旧（最旧先过期先淘汰）。量级评估：下发类消息（PodSync /
	// DeviceCommand）正常工况远低于该频率；10000 条 × ~100 B/行 ≈ 1 MB
	// SQLite 空间上限，空间成本可忽略。与原内存缓存（1000 条）相比扩大
	// 10 倍——持久层成本远低于内存态，且淘汰仍保底可控。
	DedupMaxKeys = 10000

	// DedupEvictBatch 是单次淘汰的批量大小：超过上限时每次删除
	// DedupEvictBatch 条最旧键（而非逐条删到上限内），摊薄维护成本。
	DedupEvictBatch = 500

	// DedupCleanupInterval 是后台 TTL 清理协程的运行间隔。
	// 过期键在被清理前不占用去重语义（IsProcessed 已判过期），清理只是
	// 回收空间，低频即可。
	DedupCleanupInterval = time.Hour

	// DedupMaintainEvery 是写入路径触发异步维护的间隔：每累计
	// DedupMaintainEvery 次写入触发一次（淘汰 + 过期清理）。以写入次数
	// 而非时间驱动，保证低频场景下维护次数与写入量成正比、不空转。
	DedupMaintainEvery = 200
)

// dedupTableName 是去重键表名。
const dedupTableName = "dedup_keys"

// sentinel 错误（哨兵错误风格，调用方用 errors.Is 判定）。
var (
	// ErrDedupStoreNil 表示去重存储依赖的底层 Store 为 nil。
	ErrDedupStoreNil = errors.New("去重存储依赖的 Store 不能为 nil")
)

// DedupStore 是持久化幂等去重键存储。
//
// 并发说明：所有方法可安全并发调用。内部用 mu 保护写入计数器（维护触发
// 用），SQLite 访问走 Store 的单连接（database/sql 串行化，PRIMARY KEY
// 保证同 ID 写入幂等）。异步维护协程（RunMaintenanceLoop）与同步路径
// 互不阻塞：维护失败只记日志。
type DedupStore struct {
	store *Store

	mu       sync.Mutex
	writes   int  // 写入计数（触发异步维护用）
	maintain bool // 已有维护任务排队中（防重复投递）
}

// NewDedupStore 创建去重键存储并建表（幂等，重复打开不报错）。
// 启动时立即清理一次过期键：重启后残留的过期键被回收，不占容量。
// store 为 nil 时返回 ErrDedupStoreNil。
func NewDedupStore(store *Store) (*DedupStore, error) {
	if store == nil {
		return nil, ErrDedupStoreNil
	}
	if _, err := store.db.Exec(fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s (
			msg_id     TEXT PRIMARY KEY,
			expires_at INTEGER NOT NULL
		)`, dedupTableName)); err != nil {
		return nil, fmt.Errorf("创建去重键表 %s 失败: %w", dedupTableName, err)
	}
	// 容量淘汰按 expires_at 升序取最旧，TTL 清理按 expires_at 范围删除，
	// 同一索引同时服务两条路径。
	if _, err := store.db.Exec(fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS idx_dedup_expires ON %s(expires_at)`, dedupTableName)); err != nil {
		return nil, fmt.Errorf("创建去重键索引失败: %w", err)
	}
	d := &DedupStore{store: store}
	// 启动清理一次：失败不阻断启动（过期键不影响去重判定，后台会再清）
	_, _ = d.CleanupExpired()
	return d, nil
}

// IsProcessed 报告消息 ID 是否已成功处理过且未过 TTL（去重查询）。
// 不存在或已过期 → false（已过期键视为未处理，云端重试可重新执行）。
// 查询失败（磁盘异常等）按安全侧返回 false：允许重新执行，云端侧自身
// 也有幂等语义兜底（Pod/Config 覆盖写）。
func (d *DedupStore) IsProcessed(msgID string) bool {
	if d == nil || msgID == "" {
		return false
	}
	var expiresAt int64
	err := d.store.db.QueryRow(fmt.Sprintf(
		`SELECT expires_at FROM %s WHERE msg_id = ?`, dedupTableName), msgID).Scan(&expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		log.Warnf("去重键查询失败（按未处理处理，msgID=%s）: %v", msgID, err)
		return false
	}
	return time.Now().UnixMilli() < expiresAt
}

// MarkProcessed 记录消息 ID 为已成功处理（写入即按 DedupTTL 展期）。
// 返回是否成功持久化；失败只记日志（去重是尽力而为的增强，主路径不因
// 持久化失败而失败——消息本身已成功执行）。
//
// 异步维护触发：累计 DedupMaintainEvery 次写入后投递一次维护（容量淘汰
// + 过期清理），在独立协程执行，不阻塞调用方。
func (d *DedupStore) MarkProcessed(msgID string) bool {
	if d == nil || msgID == "" {
		return false
	}
	expiresAt := time.Now().Add(DedupTTL).UnixMilli()
	_, err := d.store.db.Exec(fmt.Sprintf(
		`INSERT INTO %s(msg_id, expires_at) VALUES(?, ?)
		 ON CONFLICT(msg_id) DO UPDATE SET expires_at=excluded.expires_at`, dedupTableName),
		msgID, expiresAt)
	if err != nil {
		log.Warnf("去重键写入失败（msgID=%s）: %v", msgID, err)
		return false
	}
	d.maybeMaintain()
	return true
}

// maybeMaintain 写入计数达到 DedupMaintainEvery 时投递一次异步维护。
// 已有维护排队时跳过（合并），不重复投递。
func (d *DedupStore) maybeMaintain() {
	d.mu.Lock()
	d.writes++
	if d.writes < DedupMaintainEvery || d.maintain {
		d.mu.Unlock()
		return
	}
	d.writes = 0
	d.maintain = true
	d.mu.Unlock()
	go func() {
		d.maintainOnce("写入阈值")
		d.mu.Lock()
		d.maintain = false
		d.mu.Unlock()
	}()
}

// maintainOnce 执行一次维护：容量淘汰 + 过期清理。
func (d *DedupStore) maintainOnce(reason string) {
	if n, err := d.EvictOverLimit(); err != nil {
		log.Warnf("去重键容量淘汰失败（%s）: %v", reason, err)
	} else if n > 0 {
		log.Infof("去重键容量淘汰：已删除 %d 条最旧键（%s）", n, reason)
	}
	if n, err := d.CleanupExpired(); err != nil {
		log.Warnf("去重键过期清理失败（%s）: %v", reason, err)
	} else if n > 0 {
		log.Infof("去重键过期清理：已删除 %d 条过期键（%s）", n, reason)
	}
}

// EvictOverLimit 容量淘汰：超过 DedupMaxKeys 时按 expires_at 升序删除
// 最旧一批（DedupEvictBatch 条）。返回本次删除条数。
// 大于上限但小于上限+批量的场景也会执行删除（宁多删最旧不超限运行）；
// 无需淘汰时只执行一条 COUNT。
func (d *DedupStore) EvictOverLimit() (int, error) {
	var n int
	if err := d.store.db.QueryRow(fmt.Sprintf(
		`SELECT COUNT(*) FROM %s`, dedupTableName)).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计去重键条数失败: %w", err)
	}
	if n <= DedupMaxKeys {
		return 0, nil
	}
	res, err := d.store.db.Exec(fmt.Sprintf(
		`DELETE FROM %s WHERE msg_id IN (
			SELECT msg_id FROM %s ORDER BY expires_at ASC LIMIT ?
		)`, dedupTableName, dedupTableName), DedupEvictBatch)
	if err != nil {
		return 0, fmt.Errorf("淘汰最旧去重键失败: %w", err)
	}
	removed, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取淘汰条数失败: %w", err)
	}
	return int(removed), nil
}

// CleanupExpired 删除已过期（expires_at < now）的去重键。返回删除条数。
func (d *DedupStore) CleanupExpired() (int, error) {
	res, err := d.store.db.Exec(fmt.Sprintf(
		`DELETE FROM %s WHERE expires_at < ?`, dedupTableName), time.Now().UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("清理过期去重键失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取清理条数失败: %w", err)
	}
	return int(n), nil
}

// RunMaintenanceLoop 启动后台 TTL 清理协程（间隔 DedupCleanupInterval）。
// 由装配层以 goroutine 启动；ctx 取消即退出。容量淘汰只由写入路径触发
// （有写入才可能超限），本协程只负责过期清理——空转成本极低。
func (d *DedupStore) RunMaintenanceLoop(ctx context.Context) {
	ticker := time.NewTicker(DedupCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if n, err := d.CleanupExpired(); err != nil {
				log.Warnf("去重键定期清理失败: %v", err)
			} else if n > 0 {
				log.Infof("去重键定期清理：已删除 %d 条过期键", n)
			}
		case <-ctx.Done():
			return
		}
	}
}

// Count 返回去重键总数（含未过期与已过期未清理，测试与诊断用）。
func (d *DedupStore) Count() (int, error) {
	var n int
	if err := d.store.db.QueryRow(fmt.Sprintf(
		`SELECT COUNT(*) FROM %s`, dedupTableName)).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计去重键条数失败: %w", err)
	}
	return n, nil
}

// DropAll 清空全部去重键（仅测试用：模拟「去重缓存为空」的对照场景）。
func (d *DedupStore) DropAll() error {
	_, err := d.store.db.Exec(fmt.Sprintf(`DELETE FROM %s`, dedupTableName))
	if err != nil {
		return fmt.Errorf("清空去重键失败: %w", err)
	}
	return nil
}
