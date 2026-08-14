// 操作台账（WBS 5.2 验收核心）：设备上报/下发操作的持久化记录。
//
// 用途：所有 Mapper 的采集（上报）与指令（下发）操作都落一条 OpRecord，
// 可按 设备/方向/时间范围 组合条件查询，超过保留期（默认 30 天）自动清理——
// 满足验收「双向读写验证 + 台账可查保留 30 天」。
//
// 存储选型：独立 SQL 表 op_ledger（而非复用 meta_kv KV 表），理由：
//   - 查询模式：台账按 device_id/direction/ts 组合过滤 + 排序 + LIMIT，
//     SQL 的 WHERE/索引/ORDER BY 一条语句完成，KV 前缀扫描需全量拉取后
//     在内存过滤（记录量大时不可行）；
//   - 清理模式：保留期删除是范围删除（DELETE WHERE ts < cutoff），
//     KV 需先 List 前缀再逐条 Delete，慢且易留孤儿；
//   - 语义差异：meta_kv 是「覆盖写」的当前态存储（同 key 只留最新），
//     台账是「追加写」的历史流水（每条操作一条记录，绝不覆盖）。
//
// 并发说明：Store 是单连接（SetMaxOpenConns(1)），SQLite 内部串行化，
// SaveOp 与 CleanupOps 天然互斥，无需额外加锁。
package metamanager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"edgeflow/pkg/log"
)

// 台账常量。
const (
	// DirUp 是上报方向（采集/读寄存器）。
	DirUp = "up"
	// DirDown 是下发方向（指令/写寄存器）。
	DirDown = "down"
	// LedgerRetentionDays 是台账默认保留天数（验收要求：30 天）。
	LedgerRetentionDays = 30
	// opLedgerTable 是台账表名。
	opLedgerTable = "op_ledger"
	// defaultOpLimit 是 ListOps 默认返回条数上限。
	defaultOpLimit = 200
)

// OpRecord 是一条设备操作记录（台账行）。
type OpRecord struct {
	ID        int64  `json:"id"`        // 自增主键
	Ts        int64  `json:"ts"`        // 操作时间（毫秒时间戳）
	DeviceID  string `json:"deviceId"`  // 设备名（如 mb-sensor-01）
	Direction string `json:"direction"` // 方向：up=上报采集 / down=下发指令
	RegAddr   string `json:"regAddr"`   // 寄存器地址描述（如 0x0000、0x0010、coil:0x0020）
	Value     string `json:"value"`     // 操作值（原始寄存器值或物理值，字符串便于异构表达）
	Result    string `json:"result"`    // 结果：ok / error
	Message   string `json:"message"`   // 补充信息（错误详情 / 回读验证结果）
}

// OpFilter 是 ListOps 的查询条件（零值字段不参与过滤）。
type OpFilter struct {
	DeviceID  string // 非空时按设备名精确过滤
	Direction string // 非空时按方向过滤（up / down）
	StartTs   int64  // >0 时按 ts >= StartTs 过滤（毫秒）
	EndTs     int64  // >0 时按 ts <= EndTs 过滤（毫秒）
	Limit     int    // <=0 时用默认上限 200
}

// Ledger 是设备操作台账：封装 op_ledger 表的读写与保留期清理。
// 由装配层在 Store 之上创建（NewLedger），Mapper 通过 SaveOp 记录操作。
type Ledger struct {
	store *Store
}

// NewLedger 创建台账并完成初始化：
//   - 建表 op_ledger 与查询索引（幂等，重复打开不报错）；
//   - 启动时执行一次保留期清理（重启后过期记录立即被清掉，不依赖定期任务）。
func NewLedger(store *Store) (*Ledger, error) {
	if store == nil {
		return nil, errors.New("台账依赖的 Store 不能为 nil")
	}
	if _, err := store.db.Exec(fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			ts         INTEGER NOT NULL,
			device_id  TEXT    NOT NULL,
			direction  TEXT    NOT NULL,
			reg_addr   TEXT    NOT NULL,
			value      TEXT    NOT NULL,
			result     TEXT    NOT NULL,
			message    TEXT    NOT NULL DEFAULT ''
		)`, opLedgerTable)); err != nil {
		return nil, fmt.Errorf("创建台账表 %s 失败: %w", opLedgerTable, err)
	}
	// 查询索引：按时间范围、设备+时间、方向+时间（ListOps 主要过滤路径）
	for _, idx := range []string{
		"CREATE INDEX IF NOT EXISTS idx_op_ledger_ts ON op_ledger(ts)",
		"CREATE INDEX IF NOT EXISTS idx_op_ledger_device_ts ON op_ledger(device_id, ts)",
		"CREATE INDEX IF NOT EXISTS idx_op_ledger_dir_ts ON op_ledger(direction, ts)",
	} {
		if _, err := store.db.Exec(idx); err != nil {
			return nil, fmt.Errorf("创建台账索引失败: %w", err)
		}
	}
	l := &Ledger{store: store}
	if _, err := l.CleanupOps(LedgerRetentionDays * 24 * time.Hour); err != nil {
		// 启动清理失败不阻断（记录告警由调用方负责），下次定期清理会再试
		return l, nil
	}
	return l, nil
}

// SaveOp 追加一条操作记录（自动分配 ID 与当前毫秒时间戳；op.Ts 为 0 时填充）。
// 追加语义：同一条操作绝不覆盖，历史流水只增不减（与 meta_kv 覆盖语义不同）。
func (l *Ledger) SaveOp(op OpRecord) error {
	if op.DeviceID == "" {
		return errors.New("台账记录缺少设备名（deviceId）")
	}
	if op.Direction != DirUp && op.Direction != DirDown {
		return fmt.Errorf("台账记录方向 %q 非法（仅支持 up/down）", op.Direction)
	}
	if op.RegAddr == "" {
		return errors.New("台账记录缺少寄存器地址（regAddr）")
	}
	if op.Result == "" {
		op.Result = "ok"
	}
	if op.Ts == 0 {
		op.Ts = time.Now().UnixMilli()
	}
	_, err := l.store.db.Exec(fmt.Sprintf(
		`INSERT INTO %s(ts, device_id, direction, reg_addr, value, result, message)
		 VALUES(?, ?, ?, ?, ?, ?, ?)`, opLedgerTable),
		op.Ts, op.DeviceID, op.Direction, op.RegAddr, op.Value, op.Result, op.Message)
	if err != nil {
		return fmt.Errorf("写入台账记录失败: %w", err)
	}
	return nil
}

// ListOps 按条件查询台账记录（按时间倒序返回，最新的在前）。
// 支持条件：设备名 / 方向 / 时间范围 / 条数上限；零值字段不参与过滤。
// 无匹配记录时返回空切片（非 nil），不视为错误。
func (l *Ledger) ListOps(f OpFilter) ([]OpRecord, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultOpLimit
	}
	q := fmt.Sprintf(
		`SELECT id, ts, device_id, direction, reg_addr, value, result, message
		 FROM %s WHERE 1=1`, opLedgerTable)
	args := make([]any, 0, 4)
	if f.DeviceID != "" {
		q += " AND device_id = ?"
		args = append(args, f.DeviceID)
	}
	if f.Direction != "" {
		q += " AND direction = ?"
		args = append(args, f.Direction)
	}
	if f.StartTs > 0 {
		q += " AND ts >= ?"
		args = append(args, f.StartTs)
	}
	if f.EndTs > 0 {
		q += " AND ts <= ?"
		args = append(args, f.EndTs)
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := l.store.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("查询台账失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	ops := make([]OpRecord, 0, 16)
	for rows.Next() {
		var op OpRecord
		if err := rows.Scan(&op.ID, &op.Ts, &op.DeviceID, &op.Direction,
			&op.RegAddr, &op.Value, &op.Result, &op.Message); err != nil {
			return nil, fmt.Errorf("读取台账结果失败: %w", err)
		}
		ops = append(ops, op)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历台账结果失败: %w", err)
	}
	return ops, nil
}

// CleanupOps 删除超过保留期的台账记录（retention 如 30*24h）。
// 返回本次删除的记录条数（0 表示无过期记录）。
func (l *Ledger) CleanupOps(retention time.Duration) (int, error) {
	cutoff := time.Now().Add(-retention).UnixMilli()
	res, err := l.store.db.Exec(fmt.Sprintf("DELETE FROM %s WHERE ts < ?", opLedgerTable), cutoff)
	if err != nil {
		return 0, fmt.Errorf("清理过期台账失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取清理条数失败: %w", err)
	}
	return int(n), nil
}

// RunCleanupLoop 定期执行保留期清理（间隔 interval，如 24h）。
// 由装配层以 goroutine 启动；ctx 取消即退出。用于长期运行的 edgecore。
func (l *Ledger) RunCleanupLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if n, err := l.CleanupOps(LedgerRetentionDays * 24 * time.Hour); err != nil {
				log.Errorf("台账定期清理失败: %v", err)
			} else if n > 0 {
				log.Infof("台账定期清理：已删除 %d 条超过 %d 天的记录", n, LedgerRetentionDays)
			}
		case <-ctx.Done():
			return
		}
	}
}

// Count 返回台账记录总数（测试与诊断用）。
func (l *Ledger) Count() (int, error) {
	var n int
	if err := l.store.db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", opLedgerTable)).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计台账条数失败: %w", err)
	}
	return n, nil
}
