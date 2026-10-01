// 告警台账（v0.40.0，spec 0013 US-2）：边缘侧告警的持久化记录与活跃索引。
//
// 用途：alarmManager 产出的告警落盘（SQLite），支撑 ① 去重聚合（按 dedup_key
// 查活跃告警，命中则 count++）；② 断网本地缓存（告警先留痕再上行）。
// 注意（KI §41）：边侧重启后进行中 episode 不自动延续（active 表为内存态，
// 重启即新 episode）——聚合身份跨重启延续（台账回填 active）为后续候选；
// 与 rule_events（触发流水）正交：rule_events 是逐次触发流水，
// alarm_ledger 是聚合后的告警事实（同 ID 覆盖更新）。
//
// 存储选型与并发：独立 SQL 表 alarms（追加/覆盖 + 覆盖更新）；Store 单连接串行化。
package metamanager

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"edgeflow/pkg/alarm"
)

// alarmsTable 是告警台账表名。
const alarmsTable = "alarms"

// AlarmLedger 是边缘告警台账：封装 alarms 表的读写。
// 由装配层在 Store 之上创建（NewAlarmLedger）。
type AlarmLedger struct {
	store *Store
}

// NewAlarmLedger 创建告警台账并完成初始化（建表幂等，重复打开不报错）。
func NewAlarmLedger(store *Store) (*AlarmLedger, error) {
	if store == nil {
		return nil, errors.New("告警台账依赖的 Store 不能为 nil")
	}
	if _, err := store.db.Exec(fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s (
			alarm_id  TEXT PRIMARY KEY,
			dedup_key TEXT    NOT NULL,
			state     TEXT    NOT NULL,
			severity  TEXT    NOT NULL,
			alarm     TEXT    NOT NULL,
			ts        INTEGER NOT NULL
		)`, alarmsTable)); err != nil {
		return nil, fmt.Errorf("创建告警表 %s 失败: %w", alarmsTable, err)
	}
	for _, idx := range []string{
		"CREATE INDEX IF NOT EXISTS idx_alarms_dedup ON alarms(dedup_key, ts)",
		"CREATE INDEX IF NOT EXISTS idx_alarms_ts ON alarms(ts)",
	} {
		if _, err := store.db.Exec(idx); err != nil {
			return nil, fmt.Errorf("创建告警索引失败: %w", err)
		}
	}
	return &AlarmLedger{store: store}, nil
}

// UpsertAlarm 写入/覆盖一条告警（按 alarm_id 主键覆盖；alarm JSON 为唯一事实形态）。
func (l *AlarmLedger) UpsertAlarm(a alarm.Alarm) error {
	if a.AlarmID == "" {
		return errors.New("告警记录缺少 alarmId")
	}
	raw, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("序列化告警失败: %w", err)
	}
	ts := a.UpdatedAt
	if ts == 0 {
		ts = a.RaisedAt
	}
	_, err = l.store.db.Exec(fmt.Sprintf(
		`INSERT INTO %s(alarm_id, dedup_key, state, severity, alarm, ts)
		 VALUES(?, ?, ?, ?, ?, ?)
		 ON CONFLICT(alarm_id) DO UPDATE SET
		   state = excluded.state, severity = excluded.severity,
		   alarm = excluded.alarm, ts = excluded.ts`, alarmsTable),
		a.AlarmID, a.DedupKey(), a.State, a.Severity, string(raw), ts)
	if err != nil {
		return fmt.Errorf("写入告警失败: %w", err)
	}
	return nil
}

// ActiveAlarmByDedupKey 返回该去重键下最新的非 closed 告警（活跃 episode）；
// 无活跃告警返回 (nil, nil)（调用方据此判定"新建 episode"）。
func (l *AlarmLedger) ActiveAlarmByDedupKey(dedupKey string) (*alarm.Alarm, error) {
	row := l.store.db.QueryRow(fmt.Sprintf(
		`SELECT alarm FROM %s WHERE dedup_key = ? AND state != ? ORDER BY ts DESC, alarm_id DESC LIMIT 1`,
		alarmsTable), dedupKey, alarm.StateClosed)
	var raw string
	if err := row.Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("查询活跃告警失败: %w", err)
	}
	var a alarm.Alarm
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		// 坏行自愈：按无活跃告警处理（下次触发新建 episode），不阻断采集链。
		return nil, nil
	}
	return &a, nil
}

// CountAlarms 返回告警记录总数（测试与诊断用）。
func (l *AlarmLedger) CountAlarms() (int, error) {
	var n int
	if err := l.store.db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", alarmsTable)).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计告警条数失败: %w", err)
	}
	return n, nil
}
