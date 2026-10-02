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
	wanted atomic.Bool // 视频配置声明了 media.enabled
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
