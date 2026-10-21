// Package drstore 提供治理面快照备份管理（v0.52.0，spec 0025，
// DR-ASSESSMENT 差距 #5「备份副本管理」承接）。
//
// 职责边界：
//   - 备份对象：main.go 装配注入的治理面 JSON 快照文件（region/tag/logstore，
//     对应 EDGEFLOW_CLOUDCORE_*_FILE env）；源文件不存在 = 未配置/未落盘，跳过（常态）。
//   - 备份形态：<dir>/<id>/ 子目录（id = bk-<毫秒>-<随机 hex>），内含快照副本与
//     manifest.json（文件清单 + sha256 + 字节数 + Verified 位）。
//   - 校验：Create 完成后对副本自检（逐文件重算 sha256 与 manifest 比对）；
//     Verify(id) 重跑同一复核（API ?verify=true 触发），结果回写 manifest。
//     注：校验的是「副本与 manifest 一致性」，不是「副本与当前源一致」（源在变，
//     备份的意义就是冻结时点）。
//   - 轮转：保留最近 Keep 份（默认 7，DR_KEEP 钳 [1,100]），超出删最旧整目录。
//   - 范围外：etcd 数据面快照（scripts/dr-drill.sh 演练编排 + docs/DR-RUNBOOK.md）、
//     异地副本同步（部署侧职责，docs/CONFIG-BASELINE.md 登记）、恢复执行
//     （演练 SOP——不设 API 恢复面，避免误操作放大面）。
//
// 并发：Create/Verify 共持互斥锁（同刻至多一个写型操作）；List/Get 走 manifest
// 文件扫描（无内存态依赖，进程重启后自动发现历史备份）。
package drstore

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"edgeflow/pkg/log"
)

// Source 描述一个被备份的快照文件（Name=语义名，Path=磁盘路径）。
type Source struct {
	Name string
	Path string
}

// FileRec 是 manifest 中的单文件记录。
type FileRec struct {
	Name   string `json:"name"`   // 备份目录内文件名（<name>.snapshot.json）
	Source string `json:"source"` // 源路径（审计口径：这份副本从哪来）
	Sha256 string `json:"sha256"` // 副本内容摘要（hex）
	Bytes  int64  `json:"bytes"`
}

// Backup 是一份备份的 manifest 记录。
type Backup struct {
	ID         string    `json:"id"`
	CreatedAt  int64     `json:"createdAt"` // 毫秒时间戳
	Trigger    string    `json:"trigger"`   // manual | drill
	Files      []FileRec `json:"files"`
	TotalBytes int64     `json:"totalBytes"`
	Verified   bool      `json:"verified"` // 最近一次完整性复核结果
}

// ErrDisabled 备份未启用（dir 空）。
var ErrDisabled = errors.New("drstore: 备份未启用（备份目录未配置）")

// ErrNotFound 指定备份不存在。
var ErrNotFound = errors.New("drstore: 备份不存在")

// manifestName 是备份目录内 manifest 固定文件名。
const manifestName = "manifest.json"

// Store 是备份管理器。
type Store struct {
	dir     string
	sources []Source
	keep    int

	mu sync.Mutex // 序列化 Create/Verify（写型操作）
}

// New 构建备份管理器；dir 为空 = 未启用；keep ≤0 取默认 7。
func New(dir string, sources []Source, keep int) *Store {
	if keep <= 0 {
		keep = 7
	}
	return &Store{dir: dir, sources: sources, keep: keep}
}

// Enabled 报告备份是否启用。
func (s *Store) Enabled() bool { return s != nil && s.dir != "" }

// Create 执行一次备份：复制存在的源文件 → 自检 → 写 manifest → 轮转。
// trigger 记录触发来源（manual = API 手动 / drill = 演练脚本）。
func (s *Store) Create(trigger string, nowMs int64) (*Backup, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	id, err := newID(nowMs)
	if err != nil {
		return nil, err
	}
	bdir := filepath.Join(s.dir, id)
	if err := os.MkdirAll(bdir, 0o700); err != nil {
		return nil, fmt.Errorf("drstore: 创建备份目录失败: %w", err)
	}
	b := &Backup{ID: id, CreatedAt: nowMs, Trigger: trigger, Files: []FileRec{}}
	for _, src := range s.sources {
		if src.Path == "" {
			continue
		}
		data, err := os.ReadFile(src.Path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // 未配置/未落盘：跳过（常态，不算错误）
			}
			return nil, fmt.Errorf("drstore: 读取源文件 %s 失败: %w", src.Path, err)
		}
		name := src.Name + ".snapshot.json"
		if err := os.WriteFile(filepath.Join(bdir, name), data, 0o600); err != nil {
			return nil, fmt.Errorf("drstore: 写副本 %s 失败: %w", name, err)
		}
		sum := sha256.Sum256(data)
		b.Files = append(b.Files, FileRec{
			Name: name, Source: src.Path,
			Sha256: hex.EncodeToString(sum[:]), Bytes: int64(len(data)),
		})
		b.TotalBytes += int64(len(data))
	}
	if err := s.verifyLocked(b); err != nil {
		return nil, err
	}
	if !b.Verified {
		log.Warnf("[dr] 备份 %s 自检未通过（副本与 manifest 不一致，磁盘/外部干预可疑，请立即复核）", b.ID)
	}
	if err := s.writeManifest(bdir, b); err != nil {
		return nil, err
	}
	if err := s.rotateLocked(); err != nil {
		return nil, err
	}
	return b, nil
}

// List 返回全部备份（创建时间倒序；空目录返回空切片非 nil）。
// 单个 manifest 损坏跳过（不毒化整个清单；Create 原子写保证罕见）。
func (s *Store) List() ([]Backup, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	ids, err := s.scanIDs()
	if err != nil {
		return nil, err
	}
	out := make([]Backup, 0, len(ids))
	for _, id := range ids {
		b, err := s.readManifest(filepath.Join(s.dir, id))
		if err != nil {
			continue
		}
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt > out[j].CreatedAt
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// Get 返回单份备份 manifest；不存在返回 ErrNotFound。
func (s *Store) Get(id string) (*Backup, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	if !safeID(id) {
		return nil, ErrNotFound
	}
	b, err := s.readManifest(filepath.Join(s.dir, id))
	if err != nil {
		return nil, ErrNotFound
	}
	return b, nil
}

// Verify 对指定备份执行副本完整性复核（逐文件重算 sha256 与 manifest 比对），
// 结果回写 manifest 并返回更新后的记录。
func (s *Store) Verify(id string) (*Backup, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !safeID(id) {
		return nil, ErrNotFound
	}
	bdir := filepath.Join(s.dir, id)
	b, err := s.readManifest(bdir)
	if err != nil {
		return nil, ErrNotFound
	}
	if err := s.verifyLocked(b); err != nil {
		return nil, err
	}
	if err := s.writeManifest(bdir, b); err != nil {
		return nil, err
	}
	return b, nil
}

// verifyLocked 对备份目录内副本逐文件重算 sha256，与 manifest 记录比对；
// 全部一致 → Verified=true；任一缺失/字节不符 → false。
// 调用方持锁（Create 内部自检 / Verify 持锁路径）。
func (s *Store) verifyLocked(b *Backup) error {
	bdir := filepath.Join(s.dir, b.ID)
	verified := true
	for i := range b.Files {
		rec := &b.Files[i]
		data, err := os.ReadFile(filepath.Join(bdir, rec.Name))
		if err != nil {
			verified = false
			continue
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != rec.Sha256 || int64(len(data)) != rec.Bytes {
			verified = false
		}
	}
	b.Verified = verified
	return nil
}

// writeManifest 原子写 manifest（temp+rename，对齐治理面快照惯例）。
func (s *Store) writeManifest(bdir string, b *Backup) error {
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return fmt.Errorf("drstore: manifest 序列化失败: %w", err)
	}
	tmp := filepath.Join(bdir, manifestName+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("drstore: 写 manifest 失败: %w", err)
	}
	return os.Rename(tmp, filepath.Join(bdir, manifestName))
}

// readManifest 读取并校验 manifest（ID 为空判损坏）。
func (s *Store) readManifest(bdir string) (*Backup, error) {
	data, err := os.ReadFile(filepath.Join(bdir, manifestName))
	if err != nil {
		return nil, err
	}
	var b Backup
	if err := json.Unmarshal(data, &b); err != nil || b.ID == "" {
		return nil, fmt.Errorf("drstore: manifest 损坏（%s）", bdir)
	}
	if b.Files == nil {
		b.Files = []FileRec{}
	}
	return &b, nil
}

// scanIDs 列出含合法 manifest 的备份子目录 id（排序由 List/rotate 负责）。
func (s *Store) scanIDs() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("drstore: 扫描备份目录失败: %w", err)
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && safeID(e.Name()) {
			if _, err := os.Stat(filepath.Join(s.dir, e.Name(), manifestName)); err == nil {
				ids = append(ids, e.Name())
			}
		}
	}
	return ids, nil
}

// rotateLocked 删除超出保留数（s.keep）的最旧备份整目录。
func (s *Store) rotateLocked() error {
	ids, err := s.scanIDs()
	if err != nil {
		return err
	}
	type item struct {
		id string
		ts int64
	}
	items := make([]item, 0, len(ids))
	for _, id := range ids {
		b, err := s.readManifest(filepath.Join(s.dir, id))
		if err != nil {
			continue
		}
		items = append(items, item{id: id, ts: b.CreatedAt})
	}
	if len(items) <= s.keep {
		return nil
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].ts != items[j].ts {
			return items[i].ts < items[j].ts // 旧在前
		}
		return items[i].id < items[j].id
	})
	for _, it := range items[:len(items)-s.keep] {
		if err := os.RemoveAll(filepath.Join(s.dir, it.id)); err != nil {
			return fmt.Errorf("drstore: 轮转删除 %s 失败: %w", it.id, err)
		}
	}
	return nil
}

// newID 生成备份 id：bk-<毫秒>-<4 字节随机 hex>（同毫秒多次创建由随机段规避碰撞）。
func newID(nowMs int64) (string, error) {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("drstore: 生成备份 id 失败: %w", err)
	}
	return fmt.Sprintf("bk-%d-%s", nowMs, hex.EncodeToString(buf[:])), nil
}

// safeID 限制 id 字符集 [0-9a-zA-Z-]（防路径穿越；扫描与直读两路共用）。
func safeID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r == '-':
		default:
			return false
		}
	}
	return true
}
