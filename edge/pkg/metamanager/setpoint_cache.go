// 设定值缓存（v0.40.0，spec 0013 US-5/US-6）：边缘侧最近设定值的持久化。
//
// 用途：设定值指令（class=setpoint）执行成功后按 (namespace, property) UPSERT
// 最近值与关联建单 ID——断网/链路断开期间 Twin.Desired（内存态）与本缓存保持
// 最近设定值（"断网按缓存值继续执行"语义的数据面；边缘进程重启后期望态
// 不回填——按缓存回填 Desired 为后续候选，KI §41）；恢复后云端重投建单闭环回告。
//
// 存储选型：独立 SQL 表 setpoint_cache（键值语义 UPSERT，行数 = 设备属性数）；
// Store 单连接串行化。
package metamanager

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// setpointCacheTable 是设定值缓存表名。
const setpointCacheTable = "setpoint_cache"

// SetpointCacheRecord 是一条最近设定值缓存。
type SetpointCacheRecord struct {
	Namespace  string  `json:"namespace"`  // 命名空间
	Property   string  `json:"property"`   // 目标属性名
	DeviceName string  `json:"deviceName"` // 设备名
	Value      float64 `json:"value"`      // 最近设定值
	SetpointID string  `json:"setpointId"` // 关联云端建单 ID
	Ts         int64   `json:"ts"`         // 写入时间（毫秒）
}

// SetpointCache 是设定值缓存：封装 setpoint_cache 表的读写。
type SetpointCache struct {
	store *Store
}

// NewSetpointCache 创建设定值缓存并完成初始化（建表幂等）。
func NewSetpointCache(store *Store) (*SetpointCache, error) {
	if store == nil {
		return nil, errors.New("设定值缓存依赖的 Store 不能为 nil")
	}
	if _, err := store.db.Exec(fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s (
			namespace   TEXT    NOT NULL,
			property    TEXT    NOT NULL,
			device_name TEXT    NOT NULL,
			value       REAL    NOT NULL,
			setpoint_id TEXT    NOT NULL DEFAULT '',
			ts          INTEGER NOT NULL,
			PRIMARY KEY(namespace, property)
		)`, setpointCacheTable)); err != nil {
		return nil, fmt.Errorf("创建设定值缓存表 %s 失败: %w", setpointCacheTable, err)
	}
	return &SetpointCache{store: store}, nil
}

// SaveSetpointCache 以 (namespace, property) 为键 UPSERT 最近设定值。
func (c *SetpointCache) SaveSetpointCache(rec SetpointCacheRecord) error {
	if rec.Namespace == "" || rec.Property == "" {
		return errors.New("设定值缓存记录缺少 namespace 或 property")
	}
	if rec.Ts == 0 {
		rec.Ts = time.Now().UnixMilli()
	}
	_, err := c.store.db.Exec(fmt.Sprintf(
		`INSERT INTO %s(namespace, property, device_name, value, setpoint_id, ts)
		 VALUES(?, ?, ?, ?, ?, ?)
		 ON CONFLICT(namespace, property) DO UPDATE SET
		   device_name = excluded.device_name, value = excluded.value,
		   setpoint_id = excluded.setpoint_id, ts = excluded.ts`, setpointCacheTable),
		rec.Namespace, rec.Property, rec.DeviceName, rec.Value, rec.SetpointID, rec.Ts)
	if err != nil {
		return fmt.Errorf("写入设定值缓存失败: %w", err)
	}
	return nil
}

// GetSetpointCache 读取一条最近设定值；无记录返回 (nil, nil)。
func (c *SetpointCache) GetSetpointCache(namespace, property string) (*SetpointCacheRecord, error) {
	row := c.store.db.QueryRow(fmt.Sprintf(
		`SELECT namespace, property, device_name, value, setpoint_id, ts FROM %s
		 WHERE namespace = ? AND property = ?`, setpointCacheTable), namespace, property)
	var rec SetpointCacheRecord
	if err := row.Scan(&rec.Namespace, &rec.Property, &rec.DeviceName, &rec.Value, &rec.SetpointID, &rec.Ts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取设定值缓存失败: %w", err)
	}
	return &rec, nil
}

// CountSetpointCache 返回缓存行数（测试与诊断用）。
func (c *SetpointCache) CountSetpointCache() (int, error) {
	var n int
	if err := c.store.db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", setpointCacheTable)).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计设定值缓存失败: %w", err)
	}
	return n, nil
}
