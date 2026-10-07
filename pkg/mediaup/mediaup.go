// Package mediaup 提供边缘侧媒资（快照/片段）采集后处理与上行分片（v0.43.0，
// spec 0016 US-1）。
//
// 职责：
//   - Clip → spool 目录落盘（本地副本：未完成上传的重放兜底与审计）；
//   - 分片构建（原始 ≤32KB/片，base64 后低于上行单条 64KB 上限）→ 经注入的
//     Enqueuer 入上送补传队列（至少一次语义；断网补传由既有 worker 承担）；
//   - Janitor 周期：全部上行分片离队（云端已收 Ack / 容量修剪丢弃）→ 清理
//     本地副本；超龄（默认 24h）未完成 → 清理并计数；孤儿文件清理。
//
// 依赖方向：仅 protocol + 标准库（Enqueuer/Notifier 由装配层注入，解耦
// metamanager 与 edgehub）。
package mediaup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"
)

// 常量。
const (
	// ChunkBytes 是单个分片原始字节数（base64 后 ~44KB，低于补传单条 64KB 上限）。
	ChunkBytes = 32 << 10
	// MaxMediaBytes 是单段媒资字节上限（防御；超过拒绝并计数）。
	MaxMediaBytes = 16 << 20
	// DefaultRetentionMs 是 spool 副本保留上限（未完成上传的超龄清理；毫秒）。
	DefaultRetentionMs = 24 * 60 * 60 * 1000
	// janitorInterval 是 Janitor 周期。
	janitorInterval = 30 * time.Second
	// clipQueueDepth 是采集处理队列深度（HandleClip 异步化，不阻塞采集线程）。
	clipQueueDepth = 64
	// orphanGraceMs 是孤儿文件（bin 无 meta / meta 损坏）宽限（毫秒）。
	orphanGraceMs = 60 * 60 * 1000

	// 媒资种类。
	KindSnapshot   = "snapshot"
	KindSegment    = "segment"
	KindHardSample = "hard-sample" // v0.45.0（G25）：困难样本（告警触发捕获的 JPEG 快照帧）
)

// Clip 是一次采集（快照 + 片段），由视频管道在检出时组装。
type Clip struct {
	DeviceName string // 设备/流名
	Snapshot   []byte // JPEG 单帧
	Segment    []byte // MJPEG（JPEG 序列拼接）
	FrameCount int    // 片段帧数
	Detections int    // 触发检出数
	WhenMs     int64  // 采集时刻（毫秒）
	AlarmID    string // 告警关联（留位；本版空）
}

// UploadChunk 是 MediaUpload 消息负载（云边同构，spec 0016 US-1）。
type UploadChunk struct {
	MediaID     string `json:"mediaId"`           // 媒资唯一 ID
	Kind        string `json:"kind"`              // snapshot | segment
	NodeID      string `json:"nodeId"`            // 上报节点（云端以连接身份复核）
	DeviceName  string `json:"deviceName"`        // 设备名
	StreamName  string `json:"streamName"`        // 关联 VideoStream（默认=设备名）
	AlarmID     string `json:"alarmId,omitempty"` // 告警关联（留位）
	CapturedAt  int64  `json:"capturedAt"`        // 采集时刻（毫秒）
	ContentType string `json:"contentType"`       // image/jpeg | video/x-mjpeg
	FrameCount  int    `json:"frameCount"`        // 帧数（快照=1）
	TotalBytes  int    `json:"totalBytes"`        // 整段字节数
	SHA256      string `json:"sha256"`            // 整段 sha256（hex）
	ChunkSeq    int    `json:"chunkSeq"`          // 分片序号（0 起）
	ChunkTotal  int    `json:"chunkTotal"`        // 分片总数（≥1）
	ChunkData   []byte `json:"chunkData"`         // 分片原始字节（JSON base64）
}

// Validate 校验分片负载（云端接收与边侧构造共用）。
func (u *UploadChunk) Validate() error {
	if u.MediaID == "" || u.Kind == "" || u.DeviceName == "" {
		return errors.New("mediaup: mediaId/kind/deviceName 必填")
	}
	if u.Kind != KindSnapshot && u.Kind != KindSegment && u.Kind != KindHardSample {
		return fmt.Errorf("mediaup: 未知媒资种类 %q", u.Kind)
	}
	if u.ChunkTotal < 1 || u.ChunkSeq < 0 || u.ChunkSeq >= u.ChunkTotal {
		return errors.New("mediaup: 分片序号/总数非法")
	}
	if len(u.ChunkData) == 0 {
		return errors.New("mediaup: 分片数据为空")
	}
	if u.TotalBytes <= 0 || u.TotalBytes > MaxMediaBytes {
		return fmt.Errorf("mediaup: 整段字节数非法（%d）", u.TotalBytes)
	}
	return nil
}

// Enqueuer 是上送队列的最小接口（metamanager.UplinkQueue 适配）。
type Enqueuer interface {
	EnqueueUplink(priority int, msg *protocol.Message) (int64, error)
	PendingUplinkIDs(ids []int64) ([]int64, error)
}

// Notifier 唤醒补传 worker（uplinkRelay.Notify 适配）。
type Notifier func()

// meta 是 spool 元数据（<mediaId>.meta.json）。
type meta struct {
	MediaID   string  `json:"mediaId"`
	Kind      string  `json:"kind"`
	CreatedAt int64   `json:"createdAt"` // 毫秒
	ChunkIDs  []int64 `json:"chunkIds"`  // 上行队列行 ID
}

// Uploader 是媒资 spool 与分片上传器。零值不可用；由 NewUploader 构造。
// HandleClip 异步（缓冲队列，不阻塞采集线程）；Start 启动处理与 Janitor。
type Uploader struct {
	dir       string
	nodeID    string
	q         Enqueuer
	notify    Notifier
	retention time.Duration

	clips chan Clip
	stop  chan struct{}
	wg    sync.WaitGroup

	seq      atomic.Uint64
	stopped  atomic.Bool
	flushed  atomic.Uint64 // spool 副本完成上行清理计数
	expired  atomic.Uint64 // 超龄清理计数
	gorphans atomic.Uint64 // 孤儿清理计数
	gdropped atomic.Uint64 // 队列满丢弃计数
}

// NewUploader 创建媒资上传器；dir 为 spool 目录（自动创建）；retention <= 0
// 用 DefaultRetentionMs。
func NewUploader(dir, nodeID string, q Enqueuer, notify Notifier, retention time.Duration) *Uploader {
	if retention <= 0 {
		retention = DefaultRetentionMs
	}
	return &Uploader{
		dir:       dir,
		nodeID:    nodeID,
		q:         q,
		notify:    notify,
		retention: retention,
		clips:     make(chan Clip, clipQueueDepth),
		stop:      make(chan struct{}),
	}
}

// Start 启动采集处理 worker 与 Janitor。
func (u *Uploader) Start() {
	u.wg.Add(2)
	go u.clipLoop()
	go u.janitorLoop()
}

// Stop 停止（幂等）；缓冲中未处理的采集在停止后丢弃（本地已采集数据由
// 调用方决定是否等待——本版同步等待当前处理中的一条完成）。
func (u *Uploader) Stop() {
	if u.stopped.CompareAndSwap(false, true) {
		close(u.stop)
	}
	u.wg.Wait()
}

// HandleClip 提交一次采集（异步；队列满丢弃并计数）。未 Start 时亦可调用
// （缓冲排队），但需 Start 才会被处理。
func (u *Uploader) HandleClip(c Clip) {
	if u.stopped.Load() {
		u.gdropped.Add(1)
		return
	}
	select {
	case u.clips <- c:
	default:
		u.gdropped.Add(1)
		log.Warnf("媒资采集处理队列已满，丢弃一次采集（device=%s）", c.DeviceName)
	}
}

// Stats 返回计数快照（观测）。
func (u *Uploader) Stats() (flushed, expired, orphans, dropped uint64) {
	return u.flushed.Load(), u.expired.Load(), u.gorphans.Load(), u.gdropped.Load()
}

// clipLoop 串行处理采集：spool 落盘 → 分片入队 → 唤醒补传 worker。
func (u *Uploader) clipLoop() {
	defer u.wg.Done()
	for {
		select {
		case <-u.stop:
			return
		case c := <-u.clips:
			if err := u.processClip(c); err != nil {
				log.Warnf("媒资 spool 失败（device=%s）: %v", c.DeviceName, err)
			}
		}
	}
}

// processClip 处理一次采集：快照与片段各作为一段媒资落盘并分片入队。
func (u *Uploader) processClip(c Clip) error {
	if len(c.Snapshot) == 0 && len(c.Segment) == 0 {
		return errors.New("mediaup: 空采集（快照与片段均为空）")
	}
	when := c.WhenMs
	if when <= 0 {
		when = time.Now().UnixMilli()
	}
	if len(c.Snapshot) > 0 {
		if err := u.spoolAndEnqueue(KindSnapshot, "image/jpeg", c, when, c.Snapshot, 1); err != nil {
			return err
		}
	}
	if len(c.Segment) > 0 {
		fc := c.FrameCount
		if fc < 1 {
			fc = 1
		}
		if err := u.spoolAndEnqueue(KindSegment, "video/x-mjpeg", c, when, c.Segment, fc); err != nil {
			return err
		}
	}
	if u.notify != nil {
		u.notify()
	}
	return nil
}

// EnqueueHardSample 是困难样本专用入口（v0.45.0，G25）：只取 Clip.Snapshot
// （JPEG 单帧）以 KindHardSample 落盘入队；Snapshot 为空时返回 nil（调用方
// 在帧缺失时通常已自行过滤，此处防御）。与既有 HandleClip/processClip 的
// snapshot/segment 双段语义隔离，互不影响。
func (u *Uploader) EnqueueHardSample(c Clip) error {
	if len(c.Snapshot) == 0 {
		return nil
	}
	return u.spoolAndEnqueue(KindHardSample, "image/jpeg", c, c.WhenMs, c.Snapshot, 1)
}

// spoolAndEnqueue 落盘一段媒资并逐分片入队（写盘先行；meta 最后写——Janitor
// 不会看到无盘的 meta）。
func (u *Uploader) spoolAndEnqueue(kind, contentType string, c Clip, whenMs int64, data []byte, frameCount int) error {
	if len(data) > MaxMediaBytes {
		return fmt.Errorf("mediaup: 媒资超限（%d > %d）", len(data), MaxMediaBytes)
	}
	if err := os.MkdirAll(u.dir, 0o755); err != nil {
		return fmt.Errorf("mediaup: 创建 spool 目录失败: %w", err)
	}
	mediaID := fmt.Sprintf("m-%s-%d-%d", kind, whenMs, u.seq.Add(1))
	binPath := filepath.Join(u.dir, mediaID+".bin")
	if err := os.WriteFile(binPath, data, 0o644); err != nil {
		return fmt.Errorf("mediaup: 写 spool 失败: %w", err)
	}
	sum := sha256.Sum256(data)
	chunkTotal := (len(data) + ChunkBytes - 1) / ChunkBytes
	var chunkIDs []int64
	streamName := c.DeviceName
	for seq := 0; seq < chunkTotal; seq++ {
		lo := seq * ChunkBytes
		hi := lo + ChunkBytes
		if hi > len(data) {
			hi = len(data)
		}
		up := UploadChunk{
			MediaID:     mediaID,
			Kind:        kind,
			NodeID:      u.nodeID,
			DeviceName:  c.DeviceName,
			StreamName:  streamName,
			AlarmID:     c.AlarmID,
			CapturedAt:  whenMs,
			ContentType: contentType,
			FrameCount:  frameCount,
			TotalBytes:  len(data),
			SHA256:      hex.EncodeToString(sum[:]),
			ChunkSeq:    seq,
			ChunkTotal:  chunkTotal,
			ChunkData:   data[lo:hi],
		}
		msg, err := protocol.NewMessage(protocol.TypeMediaUpload, u.nodeID, "cloud", up)
		if err != nil {
			return fmt.Errorf("mediaup: 构造分片消息失败: %w", err)
		}
		id, err := u.q.EnqueueUplink(1 /* Normal */, msg)
		if err != nil {
			// 入队失败（超限/库错）：已入队分片保留（重放由队列承担），
			// 本条中断——本段媒资不完整，由 Janitor 超龄清理。
			return fmt.Errorf("mediaup: 分片入队失败（seq=%d/%d）: %w", seq, chunkTotal, err)
		}
		chunkIDs = append(chunkIDs, id)
	}
	m := meta{MediaID: mediaID, Kind: kind, CreatedAt: time.Now().UnixMilli(), ChunkIDs: chunkIDs}
	raw, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("mediaup: 序列化 meta 失败: %w", err)
	}
	if err := os.WriteFile(filepath.Join(u.dir, mediaID+".meta.json"), raw, 0o644); err != nil {
		return fmt.Errorf("mediaup: 写 meta 失败: %w", err)
	}
	log.Infof("媒资已入队（%s，%s，%d 字节，%d 分片）", mediaID, kind, len(data), chunkTotal)
	return nil
}

// janitorLoop 周期清理。
func (u *Uploader) janitorLoop() {
	defer u.wg.Done()
	ticker := time.NewTicker(janitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-u.stop:
			return
		case <-ticker.C:
			u.JanitorSweep()
		}
	}
}

// JanitorSweep 执行一轮清理（导出供测试直接调用；生产由 janitorLoop 周期触发）：
//  1. 全部上行分片离队 → 本地副本完成使命，删除（flushed++）；
//  2. meta 超龄（retention）未完成 → 删除（expired++）；
//  3. 孤儿文件（bin 无 meta / meta 损坏）超宽限 → 删除（orphans++）。
func (u *Uploader) JanitorSweep() {
	entries, err := os.ReadDir(u.dir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warnf("媒资 Janitor 读取 spool 目录失败: %v", err)
		}
		return
	}
	now := time.Now().UnixMilli()
	haveMeta := make(map[string]bool)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".meta.json") {
			continue
		}
		mediaID := strings.TrimSuffix(e.Name(), ".meta.json")
		haveMeta[mediaID] = true
		raw, err := os.ReadFile(filepath.Join(u.dir, e.Name()))
		if err != nil {
			continue
		}
		var m meta
		if err := json.Unmarshal(raw, &m); err != nil {
			// 损坏 meta：宽限后清理（含 bin）。
			if fi, statErr := e.Info(); statErr == nil && now-fi.ModTime().UnixMilli() > orphanGraceMs {
				u.removeMedia(mediaID)
				u.gorphans.Add(1)
			}
			continue
		}
		// 超龄：未完成上传（或已完成但上行队列长期离队前）清理。
		if now-m.CreatedAt > u.retention.Milliseconds() {
			u.removeMedia(mediaID)
			u.expired.Add(1)
			log.Warnf("媒资 spool 超龄清理（%s，未完成上行）", m.MediaID)
			continue
		}
		// 完成判定：全部分片离队（Ack 后删行 / 容量修剪丢弃）。
		if u.q == nil {
			continue
		}
		pending, err := u.q.PendingUplinkIDs(m.ChunkIDs)
		if err != nil {
			log.Warnf("媒资 Janitor 查询待发行失败: %v", err)
			continue
		}
		if len(pending) == 0 {
			u.removeMedia(m.MediaID)
			u.flushed.Add(1)
		}
	}
	// 孤儿 bin（无 meta）清理。
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".bin") {
			continue
		}
		mediaID := strings.TrimSuffix(e.Name(), ".bin")
		if haveMeta[mediaID] {
			continue
		}
		if fi, err := e.Info(); err == nil && now-fi.ModTime().UnixMilli() > orphanGraceMs {
			_ = os.Remove(filepath.Join(u.dir, e.Name()))
			u.gorphans.Add(1)
		}
	}
}

// removeMedia 删除一段媒资的 spool 文件（bin + meta；忽略不存在）。
func (u *Uploader) removeMedia(mediaID string) {
	_ = os.Remove(filepath.Join(u.dir, mediaID+".bin"))
	_ = os.Remove(filepath.Join(u.dir, mediaID+".meta.json"))
}
