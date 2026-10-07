// 媒资上传装配（v0.43.0，spec 0016 US-2）：视频 Mapper 采集的媒资经
// pkg/mediaup 入上行补传队列（断网留盘、恢复续传；至少一次）。
//
// 装配顺序说明：视频 Mapper 在 buildMapperRegistry 阶段先于补传队列构造
// （uplink relay 晚于 Mapper 创建）——因此出口用 mediaSinkHolder 延迟注入：
// Mapper 持有 holder（采集经 holder 转发）；补传队列就绪后 Set(真实
// Uploader)。初始化窗口内采集静默丢弃（瞬时语义，日志可查）。
package main

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	videomapper "edgeflow/mappers/video"
	"edgeflow/pkg/log"
	"edgeflow/pkg/mediaup"
)

// EnvMediaSpoolDir 是媒资 spool 目录环境变量（默认 data/media）。
const EnvMediaSpoolDir = "EDGEFLOW_MEDIA_SPOOL_DIR"

// mediaSinkHolder 是延迟注入的媒资出口（实现 videomapper.MediaSink）。
type mediaSinkHolder struct {
	mu     sync.Mutex
	sink   videomapper.MediaSink
	snap   videomapper.LatestSnapshotSource // 困难样本帧源（v0.45.0；独立于 sink——sink 会被 uploader 覆盖，帧源不能）
	wanted atomic.Bool                      // 视频配置声明了 media.enabled
}

// SetSnapSource 登记困难样本帧源（视频 mapper 注册时调用；与 Set 相互独立）。
func (h *mediaSinkHolder) SetSnapSource(s videomapper.LatestSnapshotSource) {
	h.mu.Lock()
	h.snap = s
	h.mu.Unlock()
}

// HandleClip 转发到当前出口（未就绪时静默丢弃——初始化窗口）。
func (h *mediaSinkHolder) HandleClip(c mediaup.Clip) {
	h.mu.Lock()
	s := h.sink
	h.mu.Unlock()
	if s != nil {
		s.HandleClip(c)
	}
}

// Set 注入真实出口（*mediaup.Uploader）。
// LatestSnapshot 返回登记帧源的最新 JPEG 快照帧（v0.45.0 US-3；帧源经
// SetSnapSource 独立登记——不随 sink 被 uploader 覆盖而丢失）。未登记/无帧
// → ok=false。弱关联语义：取「最后注册的视频 mapper」，多视频设备时快照
// 可能非告警设备帧（spec 0018 边界登记；样本经 AlarmID 元数据留痕可追溯，
// 精确设备-帧流映射属后续迭代——复核 P2-2 备忘）。
func (h *mediaSinkHolder) LatestSnapshot() (jpeg []byte, whenMs int64, ok bool) {
	h.mu.Lock()
	s := h.snap
	h.mu.Unlock()
	if s == nil {
		return nil, 0, false
	}
	return s.LatestSnapshot()
}

func (h *mediaSinkHolder) Set(s videomapper.MediaSink) {
	h.mu.Lock()
	h.sink = s
	h.mu.Unlock()
}

// wireMediaUpload 装配媒资上传：采集启用（holder.wanted）且补传可用时创建
// 并启动 Uploader。返回 Uploader（nil = 未启用/不可用；调用方停机时 Stop）。
func wireMediaUpload(holder *mediaSinkHolder, uplinkRel *uplinkRelay, nodeID string) *mediaup.Uploader {
	if holder == nil || !holder.wanted.Load() {
		return nil
	}
	if uplinkRel == nil {
		log.Warnf("视频媒资采集已启用，但上行补传未开启（需 %s=on）——媒资上传禁用", envUplinkEnabled)
		return nil
	}
	dir := os.Getenv(EnvMediaSpoolDir)
	if dir == "" {
		dir = filepath.Join("data", "media")
	}
	up := mediaup.NewUploader(dir, nodeID, uplinkRel.q, uplinkRel.Notify, 0)
	up.Start()
	holder.Set(up)
	log.Infof("媒资上传已启用（spool=%s）", dir)
	return up
}
