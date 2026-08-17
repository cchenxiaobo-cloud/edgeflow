// Package metamanager 实现 EdgeCore 的元数据管理模块（WBS 3.3）。
//
// 对标 KubeEdge 的 MetaManager：负责把边缘端元数据持久化到本地 SQLite，
// 保证边缘节点重启后数据仍在（M1 验收标准之一）。
//
// 设计：
//   - 通用 KV 表 meta_kv（key TEXT PRIMARY KEY, value TEXT, updated_at INTEGER），
//     节点注册信息、未来的 Pod/ConfigMap 状态等都存这张表，按 key 前缀区分业务域；
//   - 节点元数据专用方法封装在 NodeInfo 前缀（node:info:）之上，
//     上层只需面向 NodeInfo 编程，不需要关心 key 拼接；
//   - 开启 WAL 日志模式（journal_mode=WAL），写事务不阻塞读、崩溃恢复更稳；
//     设置 busy_timeout，多连接写冲突时等待而非立刻报错。
package metamanager

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	// 驱动注册：modernc.org/sqlite 是纯 Go 实现的 SQLite（无 CGO），
	// 交叉编译（linux/amd64、linux/arm64）不需要 CGO 工具链。
	_ "modernc.org/sqlite"
)

// 数据库路径配置。
const (
	// EnvDBPath 是覆盖数据库路径的环境变量。
	EnvDBPath = "EDGEFLOW_EDGECORE_DB_PATH"
	// DefaultDBPath 是数据库默认路径（相对进程工作目录，目录不存在自动创建）。
	DefaultDBPath = "data/edgeflow.db"
)

// metaTableName 是通用 KV 表名。
const metaTableName = "meta_kv"

// nodeInfoKeyPrefix 是节点注册信息的 key 前缀：
// 一条节点信息对应一个 key（node:info:<nodeID>），List 前缀扫描即可全部列出。
const nodeInfoKeyPrefix = "node:info:"

// busyTimeoutMs 是 SQLite 忙等待超时：多连接同时写时等待 5s 而不是立刻报
// SQLITE_BUSY（单连接场景下不会触发，属于防御性设置）。
const busyTimeoutMs = 5000

// KVEntry 是 meta_kv 表中的一行（List 返回值）。
type KVEntry struct {
	Key       string // 键
	Value     string // 值
	UpdatedAt int64  // 最后更新时间（毫秒时间戳）
}

// Store 是 SQLite 元数据存储，持有一个 *sql.DB 连接池。
//
// 并发说明：所有连接数上限设为 1——元数据读写量极小（每秒几次量级），
// 单连接足够；同时保证 Open 时设置的 PRAGMA（busy_timeout 等连接级参数）
// 一定作用在唯一连接上，不会因连接池新建连接而失效。
// （journal_mode=WAL 是数据库文件级属性，持久化在文件头，不受此限制。）
//
// 订阅者表（subMu/subscribers/nextSubID，见 notify.go）是纯内存态，
// 与 SQLite 连接相互独立：订阅不落盘，重启后需重新订阅（Edged 启动时
// 用 ListPods 全量对账即可覆盖重启窗口的变更）。
type Store struct {
	db   *sql.DB
	path string

	// subMu 保护订阅者表（Pod 变更通知，见 notify.go）。
	subMu       sync.Mutex
	subscribers map[int]*subscriber // 订阅 ID → 订阅者（nil 表示尚未有任何订阅）
	nextSubID   int                 // 订阅 ID 自增分配
}

// NodeInfo 是落盘的节点注册信息（以 JSON 字符串存入 meta_kv，
// 字段与 CloudHub 注册契约保持一致，便于后续云端核对）。
type NodeInfo struct {
	NodeID          string `json:"nodeID"`          // 边缘节点 ID（与 Register 消息一致）
	NodeName        string `json:"nodeName"`        // 云端在 RegisterAck 中分配的节点名
	CloudAddr       string `json:"cloudAddr"`       // 当前连接的云端地址
	Arch            string `json:"arch"`            // 节点架构（GOARCH）
	OS              string `json:"os"`              // 节点操作系统（GOOS）
	EdgeCoreVersion string `json:"edgecoreVersion"` // edgecore 版本
	RegisteredAt    int64  `json:"registeredAt"`    // 注册成功时间（毫秒时间戳）
}

// DefaultDBPathFromEnv 解析数据库路径，优先级：
//  1. 环境变量 EDGEFLOW_EDGECORE_DB_PATH（测试/部署时便于重定向）；
//  2. 默认 data/edgeflow.db（相对进程工作目录）。
func DefaultDBPathFromEnv() string {
	if v := os.Getenv(EnvDBPath); v != "" {
		return v
	}
	return DefaultDBPath
}

// Open 打开（必要时创建）SQLite 数据库并完成初始化：
//   - 自动创建数据库所在目录（如 data/）；
//   - 建表 meta_kv（不存在时）；
//   - 开启 WAL 日志模式；
//   - 设置 busy_timeout。
//
// dbPath 为空或非法（如父目录是普通文件）时返回错误。
func Open(dbPath string) (*Store, error) {
	if dbPath == "" {
		return nil, errors.New("数据库路径不能为空")
	}
	// 自动创建目录：edgecore 以 systemd/容器方式运行时工作目录可能不含 data/
	if dir := filepath.Dir(dbPath); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建数据库目录 %s 失败: %w", dir, err)
		}
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("打开数据库 %s 失败: %w", dbPath, err)
	}
	// 单连接：见 Store 结构注释（保证连接级 PRAGMA 生效）
	db.SetMaxOpenConns(1)

	// Ping 验证路径真实可写（如父目录是普通文件时在这里报错）
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("打开数据库 %s 失败: %w", dbPath, err)
	}

	s := &Store{db: db, path: dbPath}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// init 执行初始化 SQL：WAL 模式、busy_timeout、建表。
func (s *Store) init() error {
	// WAL 模式：写事务不阻塞读、崩溃后自动恢复，边缘端长期运行更可靠。
	// 该设置持久化在数据库文件头，重开后依然生效。
	// WAL checkpoint 策略（M1B P2-2 结论）：无显式 wal_checkpoint 调用或定时
	// 策略——本仓库写入量极低（每秒几次量级，单连接），SQLite 默认的自动
	// checkpoint（WAL 文件达到阈值页数时触发）足够及时，无需显式策略；
	// 未来写入量上升（如 Pod 状态高频落盘）时再评估显式 checkpoint。
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return fmt.Errorf("开启 WAL 日志模式失败: %w", err)
	}
	// busy_timeout：写冲突时等待而非立刻报 SQLITE_BUSY
	if _, err := s.db.Exec(fmt.Sprintf("PRAGMA busy_timeout=%d", busyTimeoutMs)); err != nil {
		return fmt.Errorf("设置 busy_timeout 失败: %w", err)
	}
	// 通用 KV 表：key 主键保证幂等，updated_at 记录最后写入时间
	_, err := s.db.Exec(fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s (
			key        TEXT PRIMARY KEY,
			value      TEXT NOT NULL,
			updated_at INTEGER NOT NULL
		)`, metaTableName))
	if err != nil {
		return fmt.Errorf("创建表 %s 失败: %w", metaTableName, err)
	}
	return nil
}

// Close 关闭数据库连接，并关闭全部订阅者的事件通道（消费方可据此退出，
// 避免 edgecore 退出时订阅方 goroutine 永久阻塞）。之后再次使用 Store
// 会报错；需要继续读写时重新 Open（订阅需重新注册）。
func (s *Store) Close() error {
	// 先注销全部订阅者再关库：notify 与 Unsubscribe 互斥于 subMu，
	// 此处清空订阅表后不会再有任何事件发送（见 notify.go 并发说明）。
	s.subMu.Lock()
	for id, sub := range s.subscribers {
		delete(s.subscribers, id)
		close(sub.ch)
	}
	s.subMu.Unlock()
	return s.db.Close()
}

// Path 返回数据库文件路径（日志/诊断用）。
func (s *Store) Path() string {
	return s.path
}

// Put 写入（或覆盖）一个键值对，updated_at 自动记录当前毫秒时间戳。
// 幂等：同 key 重复写入只更新 value 与时间。
func (s *Store) Put(key, value string) error {
	_, err := s.db.Exec(fmt.Sprintf(
		`INSERT INTO %s(key, value, updated_at) VALUES(?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		metaTableName), key, value, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("写入 %s 失败: %w", key, err)
	}
	return nil
}

// Get 读取一个键的值。键不存在时返回 (false, nil)，不视为错误。
func (s *Store) Get(key string) (string, bool, error) {
	var value string
	err := s.db.QueryRow(fmt.Sprintf("SELECT value FROM %s WHERE key = ?", metaTableName), key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("读取 %s 失败: %w", key, err)
	}
	return value, true, nil
}

// Delete 删除一个键。键不存在时静默成功（幂等）。
func (s *Store) Delete(key string) error {
	_, err := s.db.Exec(fmt.Sprintf("DELETE FROM %s WHERE key = ?", metaTableName), key)
	if err != nil {
		return fmt.Errorf("删除 %s 失败: %w", key, err)
	}
	return nil
}

// List 列出指定前缀下的全部键值对，按 key 升序返回。
// 前缀为空时返回全表内容。供节点状态、未来 Pod/Config 元数据复用。
//
// 实现说明：用 key 范围扫描（key >= prefix AND key < prefix 的严格上界）
// 替代 SQL LIKE——LIKE 会把 prefix 中的 %/_ 当作通配符（nodeID 含这些字符
// 时误匹配），且对 ASCII 大小写不敏感（把不同大小写的 key 混为一谈）；
// 范围比较走 SQLite 默认 BINARY 排序规则，大小写敏感、无通配符语义，
// 与 Get 的 key = ? 判定一致。上界由 prefixUpperBound 计算（最后一个非
// 0xff 字节加一、截断后缀），保证 [prefix, upper) 恰好覆盖"以 prefix 开头"
// 的全部 key；前缀为空或全为 0xff 时无有限上界，退化为 key >= prefix
// （此时该条件恰好等价于"以 prefix 开头"）。
func (s *Store) List(prefix string) ([]KVEntry, error) {
	upper, hasUpper := prefixUpperBound(prefix)
	var rows *sql.Rows
	var err error
	if hasUpper {
		rows, err = s.db.Query(
			fmt.Sprintf("SELECT key, value, updated_at FROM %s WHERE key >= ? AND key < ? ORDER BY key", metaTableName),
			prefix, upper)
	} else {
		rows, err = s.db.Query(
			fmt.Sprintf("SELECT key, value, updated_at FROM %s WHERE key >= ? ORDER BY key", metaTableName),
			prefix)
	}
	if err != nil {
		return nil, fmt.Errorf("查询前缀 %s 失败: %w", prefix, err)
	}
	defer func() { _ = rows.Close() }()

	entries := make([]KVEntry, 0, 8)
	for rows.Next() {
		var e KVEntry
		if err := rows.Scan(&e.Key, &e.Value, &e.UpdatedAt); err != nil {
			return nil, fmt.Errorf("读取查询结果失败: %w", err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历查询结果失败: %w", err)
	}
	return entries, nil
}

// prefixUpperBound 计算前缀扫描的严格上界：从末尾向前找到最后一个非 0xff
// 字节并加一、截断其后缀（如 "ab\xff" → "ac"）。这样 [prefix, upper)
// 恰好等于全部以 prefix 开头的 key（含 prefix 自身与 prefix+0xff… 形态）。
// 前缀为空或全为 0xff 时没有有限上界，返回 hasUpper=false（此时
// key >= prefix 本身就等价于"以 prefix 开头"，调用方无需上界条件）。
func prefixUpperBound(prefix string) (upper string, hasUpper bool) {
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] != 0xff {
			b[i]++
			return string(b[:i+1]), true
		}
	}
	return "", false
}

// SaveNodeInfo 把节点注册信息（JSON 字符串）落盘，key 为 node:info:<nodeID>。
// 重复注册（断线重连/重启）时覆盖旧记录，始终保留最新状态。
func (s *Store) SaveNodeInfo(nodeID, infoJSON string) error {
	return s.Put(nodeInfoKeyPrefix+nodeID, infoJSON)
}

// GetNodeInfo 读取节点注册信息 JSON。不存在时返回 ("", false, nil)。
func (s *Store) GetNodeInfo(nodeID string) (string, bool, error) {
	return s.Get(nodeInfoKeyPrefix + nodeID)
}

// DeleteNodeInfo 删除一条节点注册信息（节点下线/清理时用）。
func (s *Store) DeleteNodeInfo(nodeID string) error {
	return s.Delete(nodeInfoKeyPrefix + nodeID)
}

// ListNodes 返回全部已落盘的节点注册信息 JSON（通常只有本机一条；
// 多节点/单机多实例场景下也支持）。启动时用于日志展示"已加载 N 条节点元数据"。
func (s *Store) ListNodes() ([]string, error) {
	entries, err := s.List(nodeInfoKeyPrefix)
	if err != nil {
		return nil, err
	}
	infos := make([]string, 0, len(entries))
	for _, e := range entries {
		infos = append(infos, e.Value)
	}
	return infos, nil
}
