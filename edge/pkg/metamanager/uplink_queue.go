// 上行补传队列（v0.39.0，spec 0012 US-1）：规则事件等上行消息的持久化
// 待发队列 + 分级统计。
//
// 用途：弱网/断网场景下，上行消息先落盘入队、由补传 worker 陆续发送；
// 发送失败不删除（至少一次语义），网络恢复后重发；重复由云端幂等
// 去重消化。未 Ack 集即"待补传集合"（游标语义）。
//
// 存储选型（与 op_ledger/rule_ledger 同库理由）：SQLite 单连接串行化，
// 追加写 + 极简查询；队列表按 (priority DESC, id ASC) 组织——出队序 =
// 高优先级先行、同级 FIFO；计数（sent/dropped）在 uplink_meta 持久累计。
package metamanager

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"
)

// 上行队列常量。
const (
	// DefaultUplinkMaxRows 是队列默认容量（行数）。
	DefaultUplinkMaxRows = 100000
	// MaxUplinkMsgBytes 是单条上行消息的 JSON 上限（64KB，防爆库）。
	MaxUplinkMsgBytes = 64 * 1024
	// defaultUplinkDequeueLimit 是批量出队默认上限。
	defaultUplinkDequeueLimit = 32
	// UplinkPriorityLow/Normal/High 是优先级档位（severity 映射见装配层）。
	UplinkPriorityLow    = 0
	UplinkPriorityNormal = 1
	UplinkPriorityHigh   = 2

	uplinkQueueTable = "uplink_queue"
	uplinkMetaTable  = "uplink_meta"

	// uplinkMetaSent/Dropped 是计数键：累计成功上送 / 累计容量丢弃。
	uplinkMetaSent    = "sent"
	uplinkMetaDropped = "dropped"

	// uplinkDropWarnIntervalMs 是容量丢弃告警的最小间隔（限频）。
	uplinkDropWarnIntervalMs = 60 * 1000
)

// UplinkItem 是队列中的一条待上送消息。
type UplinkItem struct {
	ID        int64             // 队列行 ID（Ack 用）
	Priority  int               // 优先级（0 低 / 1 普通 / 2 高）
	Msg       *protocol.Message // 完整消息（含 ID——云端幂等键随消息保留）
	CreatedAt int64             // 入队时间（毫秒）
}

// UplinkStats 是队列观测统计（补传可视化数据源）。
type UplinkStats struct {
	Total    int   // 当前积压总数
	High     int   // priority >= 2 条数
	Normal   int   // priority = 1 条数
	Low      int   // priority <= 0 条数
	Sent     int64 // 累计成功上送（Ack 计数）
	Dropped  int64 // 累计容量丢弃
	OldestTs int64 // 最老积压条目入队时间（毫秒；空队列为 0）
}

// UplinkQueue 是持久化上行补传队列（表 uplink_queue + 计数表 uplink_meta）。
// 零值不可用；由 NewUplinkQueue 构造。方法并发安全（依赖 Store 单连接
// 串行化；dropWarnAt 为原子字段）。
type UplinkQueue struct {
	store   *Store
	maxRows int

	// dropWarnAt 是容量丢弃告警的最近时间（毫秒），用于限频。
	dropWarnAt atomic.Int64
}

// NewUplinkQueue 创建上行队列并完成初始化（建表幂等，重复打开不报错）。
// maxRows <= 0 时用 DefaultUplinkMaxRows。
func NewUplinkQueue(store *Store, maxRows int) (*UplinkQueue, error) {
	if store == nil {
		return nil, errors.New("上行队列需要非空 Store")
	}
	if maxRows <= 0 {
		maxRows = DefaultUplinkMaxRows
	}
	// 队列表：AUTOINCREMENT 保证 id 严格递增不复用（删除后新插入不回收
	// 旧 id）——FIFO 序（同级 id ASC）依赖这一点；出队索引按
	// (priority DESC, id ASC) 与查询排序一致，避免全表排序。
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			priority   INTEGER NOT NULL DEFAULT 0,
			msg        TEXT NOT NULL,
			created_at INTEGER NOT NULL
		)`, uplinkQueueTable),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_uplink_queue_dequeue
			ON %s(priority DESC, id ASC)`, uplinkQueueTable),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			k TEXT PRIMARY KEY,
			v INTEGER NOT NULL
		)`, uplinkMetaTable),
	}
	for _, stmt := range stmts {
		if _, err := store.db.Exec(stmt); err != nil {
			return nil, fmt.Errorf("初始化上行队列失败: %w", err)
		}
	}
	return &UplinkQueue{store: store, maxRows: maxRows}, nil
}

// MaxRows 返回队列容量上限（诊断/日志用）。
func (q *UplinkQueue) MaxRows() int { return q.maxRows }

// EnqueueUplink 入队一条消息，返回队列行 ID。
//   - priority 归一到 [0, 2]（越界 clamp）；
//   - msg 序列化为 JSON 持久（含 ID——云端幂等键）；
//   - 单条 JSON 超过 MaxUplinkMsgBytes 拒绝（错误）；
//   - 入库后超容量时按 (priority ASC, id ASC) 序丢弃（最低优先级中最老），
//     逐条丢弃至达标，dropped 累计（限频 Warn）。
//
// 入队 + 容量修剪在同一事务内（原子）。
func (q *UplinkQueue) EnqueueUplink(priority int, msg *protocol.Message) (int64, error) {
	if msg == nil {
		return 0, errors.New("上行消息不能为空")
	}
	if priority < UplinkPriorityLow {
		priority = UplinkPriorityLow
	}
	if priority > UplinkPriorityHigh {
		priority = UplinkPriorityHigh
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return 0, fmt.Errorf("序列化上行消息失败: %w", err)
	}
	if len(raw) > MaxUplinkMsgBytes {
		return 0, fmt.Errorf("上行消息超过单条上限（%d > %d 字节）", len(raw), MaxUplinkMsgBytes)
	}

	tx, err := q.store.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(fmt.Sprintf(
		`INSERT INTO %s(priority, msg, created_at) VALUES(?, ?, ?)`, uplinkQueueTable),
		priority, string(raw), time.Now().UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("入队失败: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("读取入队行 ID 失败: %w", err)
	}

	// 容量修剪：超限丢(最低优先级, 最老)条目。
	var count int
	if err := tx.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %s`, uplinkQueueTable)).Scan(&count); err != nil {
		return 0, fmt.Errorf("统计队列行数失败: %w", err)
	}
	if overflow := count - q.maxRows; overflow > 0 {
		del, err := tx.Exec(fmt.Sprintf(
			`DELETE FROM %s WHERE id IN (
				SELECT id FROM %s ORDER BY priority ASC, id ASC LIMIT ?)`,
			uplinkQueueTable, uplinkQueueTable), overflow)
		if err != nil {
			return 0, fmt.Errorf("容量修剪失败: %w", err)
		}
		if n, _ := del.RowsAffected(); n > 0 {
			if err := bumpUplinkMeta(tx, uplinkMetaDropped, n); err != nil {
				return 0, fmt.Errorf("更新丢弃计数失败: %w", err)
			}
			now := time.Now().UnixMilli()
			if last := q.dropWarnAt.Load(); now-last >= uplinkDropWarnIntervalMs {
				q.dropWarnAt.Store(now)
				log.Warnf("上行补传队列超过容量上限 %d，已丢弃 %d 条最低优先级最老消息", q.maxRows, n)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("提交入队事务失败: %w", err)
	}
	return id, nil
}

// DequeueUplinkBatch 批量取出待发条目（不删除；发送成功后用 AckUplink 删除）。
// 排序 priority DESC, id ASC（高优先级先行、同级 FIFO）；limit <= 0 用默认 32。
// 坏行（JSON 无法解析）跳过并清除（自愈，避免卡队列）。
func (q *UplinkQueue) DequeueUplinkBatch(limit int) ([]UplinkItem, error) {
	if limit <= 0 {
		limit = defaultUplinkDequeueLimit
	}
	rows, err := q.store.db.Query(fmt.Sprintf(
		`SELECT id, priority, msg, created_at FROM %s ORDER BY priority DESC, id ASC LIMIT ?`,
		uplinkQueueTable), limit)
	if err != nil {
		return nil, fmt.Errorf("出队查询失败: %w", err)
	}
	items := make([]UplinkItem, 0, limit)
	var badIDs []int64
	for rows.Next() {
		var it UplinkItem
		var raw string
		if err := rows.Scan(&it.ID, &it.Priority, &raw, &it.CreatedAt); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("读取队列行失败: %w", err)
		}
		var m protocol.Message
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			log.Warnf("上行队列条目 %d 解析失败（跳过并清除）: %v", it.ID, err)
			badIDs = append(badIDs, it.ID)
			continue
		}
		it.Msg = &m
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("遍历队列行失败: %w", err)
	}
	_ = rows.Close()
	for _, id := range badIDs {
		if _, err := q.store.db.Exec(fmt.Sprintf(`DELETE FROM %s WHERE id = ?`, uplinkQueueTable), id); err != nil {
			log.Warnf("清除坏队列条目 %d 失败: %v", id, err)
		}
	}
	return items, nil
}

// AckUplink 确认一条已成功上送的消息（删除 + sent 计数）。
// 行不存在时静默成功（幂等）。sent 仅在确有行被删除时 +1。
func (q *UplinkQueue) AckUplink(id int64) error {
	tx, err := q.store.db.Begin()
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(fmt.Sprintf(`DELETE FROM %s WHERE id = ?`, uplinkQueueTable), id)
	if err != nil {
		return fmt.Errorf("确认删除失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		if err := bumpUplinkMeta(tx, uplinkMetaSent, 1); err != nil {
			return fmt.Errorf("更新上送计数失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交确认事务失败: %w", err)
	}
	return nil
}

// UplinkDepth 返回队列观测统计（积压分布/最老时间/累计计数）。
func (q *UplinkQueue) UplinkDepth() (UplinkStats, error) {
	var st UplinkStats
	err := q.store.db.QueryRow(fmt.Sprintf(
		`SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN priority >= 2 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN priority = 1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN priority <= 0 THEN 1 ELSE 0 END), 0),
			COALESCE(MIN(created_at), 0)
		FROM %s`, uplinkQueueTable)).Scan(&st.Total, &st.High, &st.Normal, &st.Low, &st.OldestTs)
	if err != nil {
		return st, fmt.Errorf("统计队列失败: %w", err)
	}
	rows, err := q.store.db.Query(fmt.Sprintf(
		`SELECT k, v FROM %s WHERE k IN (?, ?)`, uplinkMetaTable), uplinkMetaSent, uplinkMetaDropped)
	if err != nil {
		return st, fmt.Errorf("读取计数失败: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var k string
		var v int64
		if err := rows.Scan(&k, &v); err != nil {
			return st, fmt.Errorf("读取计数失败: %w", err)
		}
		switch k {
		case uplinkMetaSent:
			st.Sent = v
		case uplinkMetaDropped:
			st.Dropped = v
		}
	}
	if err := rows.Err(); err != nil {
		return st, fmt.Errorf("遍历计数失败: %w", err)
	}
	return st, nil
}

// bumpUplinkMeta 在事务内对计数键累加 delta（不存在则创建）。
func bumpUplinkMeta(tx *sql.Tx, key string, delta int64) error {
	_, err := tx.Exec(fmt.Sprintf(
		`INSERT INTO %s(k, v) VALUES(?, ?)
		 ON CONFLICT(k) DO UPDATE SET v = v + ?`, uplinkMetaTable), key, delta, delta)
	return err
}
