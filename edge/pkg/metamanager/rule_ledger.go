// 规则事件台账（v0.37.0）：规则引擎触发事件的持久化记录。
//
// 用途：规则评估器每次触发产生一条事件——本台账把事件落盘（SQLite），
// 支持按 规则/设备/时间范围 组合查询，超过保留期（默认 30 天）自动清理；
// 与 op_ledger（设备操作台账）正交：op_ledger 记录"采集/指令"操作，
// rule_events 记录"规则触发"结果，两者共同构成边缘侧执行留痕。
//
// 存储选型与并发：独立 SQL 表 rule_events（同 op_ledger 的选型理由——
// 追加写历史流水 + 范围删除清理 + 组合过滤查询）；Store 单连接串行化。
package metamanager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"edgeflow/pkg/log"
)

// 规则事件台账常量。
const (
	// RuleEventRetentionDays 是规则事件默认保留天数（与操作台账同口径：30 天）。
	RuleEventRetentionDays = 30
	// ruleEventsTable 是规则事件表名。
	ruleEventsTable = "rule_events"
	// defaultRuleEventLimit 是 ListRuleEvents 默认返回条数上限。
	defaultRuleEventLimit = 200
)

// RuleEventRecord 是一条规则触发事件记录（台账行）。
type RuleEventRecord struct {
	ID        int64  `json:"id"`        // 自增主键
	Ts        int64  `json:"ts"`        // 触发时间（毫秒时间戳）
	RuleID    string `json:"ruleId"`    // 规则 ID
	DeviceID  string `json:"deviceId"`  // 设备名
	Namespace string `json:"namespace"` // 命名空间
	Severity  string `json:"severity"`  // 严重级：info/warning/critical
	Value     string `json:"value"`     // 触发时值（%g 字符串，便于异构表达）
	Message   string `json:"message"`   // 事件消息（已渲染）
}

// RuleEventFilter 是 ListRuleEvents 的查询条件（零值字段不参与过滤）。
type RuleEventFilter struct {
	RuleID   string // 非空时按规则 ID 精确过滤
	DeviceID string // 非空时按设备名精确过滤
	StartTs  int64  // >0 时按 ts >= StartTs 过滤（毫秒）
	EndTs    int64  // >0 时按 ts <= EndTs 过滤（毫秒）
	Limit    int    // <=0 时用默认上限 200
}

// RuleLedger 是规则事件台账：封装 rule_events 表的读写与保留期清理。
// 由装配层在 Store 之上创建（NewRuleLedger），规则事件出口通过 SaveEvent 记录。
type RuleLedger struct {
	store *Store
}

// NewRuleLedger 创建规则事件台账并完成初始化：
//   - 建表 rule_events 与查询索引（幂等，重复打开不报错）；
//   - 启动时执行一次保留期清理（重启后过期记录立即被清掉）。
func NewRuleLedger(store *Store) (*RuleLedger, error) {
	if store == nil {
		return nil, errors.New("规则事件台账依赖的 Store 不能为 nil")
	}
	if _, err := store.db.Exec(fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			ts         INTEGER NOT NULL,
			rule_id    TEXT    NOT NULL,
			device_id  TEXT    NOT NULL,
			namespace  TEXT    NOT NULL DEFAULT 'default',
			severity   TEXT    NOT NULL,
			value      TEXT    NOT NULL,
			message    TEXT    NOT NULL DEFAULT ''
		)`, ruleEventsTable)); err != nil {
		return nil, fmt.Errorf("创建规则事件表 %s 失败: %w", ruleEventsTable, err)
	}
	// 查询索引：按时间范围、规则+时间、设备+时间（ListRuleEvents 主要过滤路径）
	for _, idx := range []string{
		"CREATE INDEX IF NOT EXISTS idx_rule_events_ts ON rule_events(ts)",
		"CREATE INDEX IF NOT EXISTS idx_rule_events_rule_ts ON rule_events(rule_id, ts)",
		"CREATE INDEX IF NOT EXISTS idx_rule_events_device_ts ON rule_events(device_id, ts)",
	} {
		if _, err := store.db.Exec(idx); err != nil {
			return nil, fmt.Errorf("创建规则事件索引失败: %w", err)
		}
	}
	l := &RuleLedger{store: store}
	if _, err := l.CleanupEvents(RuleEventRetentionDays * 24 * time.Hour); err != nil {
		// 启动清理失败不阻断（下次定期清理会再试）
		return l, nil
	}
	return l, nil
}

// SaveEvent 追加一条规则事件记录（自动分配 ID；rec.Ts 为 0 时填充当前时间）。
// 追加语义：同一事件绝不覆盖，历史流水只增不减。
func (l *RuleLedger) SaveEvent(rec RuleEventRecord) error {
	if rec.RuleID == "" {
		return errors.New("规则事件记录缺少 ruleId")
	}
	if rec.DeviceID == "" {
		return errors.New("规则事件记录缺少 deviceId")
	}
	if rec.Namespace == "" {
		rec.Namespace = "default"
	}
	if rec.Severity == "" {
		rec.Severity = "warning"
	}
	if rec.Ts == 0 {
		rec.Ts = time.Now().UnixMilli()
	}
	_, err := l.store.db.Exec(fmt.Sprintf(
		`INSERT INTO %s(ts, rule_id, device_id, namespace, severity, value, message)
		 VALUES(?, ?, ?, ?, ?, ?, ?)`, ruleEventsTable),
		rec.Ts, rec.RuleID, rec.DeviceID, rec.Namespace, rec.Severity, rec.Value, rec.Message)
	if err != nil {
		return fmt.Errorf("写入规则事件失败: %w", err)
	}
	return nil
}

// ListRuleEvents 按条件查询规则事件（按时间倒序返回，最新的在前）。
// 无匹配记录时返回空切片（非 nil），不视为错误。
func (l *RuleLedger) ListRuleEvents(f RuleEventFilter) ([]RuleEventRecord, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultRuleEventLimit
	}
	q := fmt.Sprintf(
		`SELECT id, ts, rule_id, device_id, namespace, severity, value, message
		 FROM %s WHERE 1=1`, ruleEventsTable)
	args := make([]any, 0, 4)
	if f.RuleID != "" {
		q += " AND rule_id = ?"
		args = append(args, f.RuleID)
	}
	if f.DeviceID != "" {
		q += " AND device_id = ?"
		args = append(args, f.DeviceID)
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
		return nil, fmt.Errorf("查询规则事件失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]RuleEventRecord, 0, 16)
	for rows.Next() {
		var rec RuleEventRecord
		if err := rows.Scan(&rec.ID, &rec.Ts, &rec.RuleID, &rec.DeviceID,
			&rec.Namespace, &rec.Severity, &rec.Value, &rec.Message); err != nil {
			return nil, fmt.Errorf("读取规则事件结果失败: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历规则事件结果失败: %w", err)
	}
	return out, nil
}

// CleanupEvents 删除超过保留期的规则事件（retention 如 30*24h）。
// 返回本次删除的记录条数（0 表示无过期记录）。
func (l *RuleLedger) CleanupEvents(retention time.Duration) (int, error) {
	cutoff := time.Now().Add(-retention).UnixMilli()
	res, err := l.store.db.Exec(fmt.Sprintf("DELETE FROM %s WHERE ts < ?", ruleEventsTable), cutoff)
	if err != nil {
		return 0, fmt.Errorf("清理过期规则事件失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取清理条数失败: %w", err)
	}
	return int(n), nil
}

// RunCleanupLoop 定期执行保留期清理（间隔 interval）。ctx 取消即退出。
func (l *RuleLedger) RunCleanupLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if n, err := l.CleanupEvents(RuleEventRetentionDays * 24 * time.Hour); err != nil {
				log.Errorf("规则事件定期清理失败: %v", err)
			} else if n > 0 {
				log.Infof("规则事件定期清理：已删除 %d 条超过 %d 天的记录", n, RuleEventRetentionDays)
			}
		case <-ctx.Done():
			return
		}
	}
}

// CountEvents 返回规则事件记录总数（测试与诊断用）。
func (l *RuleLedger) CountEvents() (int, error) {
	var n int
	if err := l.store.db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", ruleEventsTable)).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计规则事件条数失败: %w", err)
	}
	return n, nil
}
