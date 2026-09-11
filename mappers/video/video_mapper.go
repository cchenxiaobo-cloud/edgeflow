// Package video 实现视频流设备的 Mapper（v0.31.0，阶段一，specs/0004）：
// 把视频帧源（阶段一为合成源）接入 EdgeFlow Mapper 框架——帧源出帧经
// latest-wins 背压槽送推理服务，结果按两条路径交付：
//  1. 数字指标面（framesTotal/inferTotal/detectionsLast/fps 等）经 Collect()
//     汇入设备影子上报链（Mapper 不感知上报链路，框架约定）；
//  2. 详细推理结果 JSON 可选留痕（metamanager 台账）与事件上行
//     （EventPublisher 注入，主题 edgeflow/video/{device}/inference）。
//
// 配置文件化沿 FR-S1-06 先例：JSON 配置经 EDGEFLOW_VIDEO_MAPPER_CONFIG
// 指定，文件存在才装配（opt-in，无配置零行为）。
package video

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"

	"edgeflow/edge/pkg/mapper"
	"edgeflow/edge/pkg/metamanager"
	"edgeflow/pkg/log"
	pkgvideo "edgeflow/pkg/video"
)

// DefaultName 是 Mapper 注册名（注册表唯一键）。
const DefaultName = "video"

// InferenceTopicTpl 是推理结果事件主题模板（fmt %s = 设备名）。
const InferenceTopicTpl = "edgeflow/video/%s/inference"

// EnvConfig 是视频 Mapper 配置文件路径的环境变量名。
const EnvConfig = "EDGEFLOW_VIDEO_MAPPER_CONFIG"

// EventPublisher 是推理结果事件上行的小接口：*eventbus.EventBus 天然实现
// （Publish(topic, payload) 同签名），mapper 依赖接口便于测试注入。
type EventPublisher interface {
	Publish(topic string, payload []byte) error
}

// SourceConfig 是帧源配置（v0.34.0 阶段二：synthetic | mjpeg | bridge）。
// mjpeg：MJPEG over HTTP 直连（url）；bridge：外部进程桥（command/args，
// 如 ffmpeg 转 RTSP→MJPEG stdout）。未知值显式拒绝，不做静默降级。
type SourceConfig struct {
	Type        string                   `json:"type"`
	URL         string                   `json:"url,omitempty"`
	Command     string                   `json:"command,omitempty"`
	Args        []string                 `json:"args,omitempty"`
	ReconnectMs int                      `json:"reconnectMs,omitempty"`
	TimeoutMs   int                      `json:"timeoutMs,omitempty"`
	Synth       pkgvideo.SyntheticConfig `json:"synthetic"`
}

// InferConfig 是推理服务配置。
type InferConfig struct {
	URL       string `json:"url"`
	TimeoutMs int    `json:"timeoutMs"`
	Model     string `json:"model,omitempty"`
}

// Config 是视频 Mapper 配置文件结构。
type Config struct {
	DeviceName string       `json:"deviceName"`
	Namespace  string       `json:"namespace"`
	Source     SourceConfig `json:"source"`
	Inference  InferConfig  `json:"inference"`
	// Ledger 与 EventBus 声明是否启用留痕/事件上行；实例由装配层注入
	//（mapper 不自建基础设施）。
	Ledger   bool `json:"ledger"`
	EventBus bool `json:"eventbus"`
}

// validate 校验配置：设备名非空；source.type ∈ {synthetic, mjpeg, bridge}
// 且对应必填字段齐全；推理 URL 非空。
func (c *Config) validate() error {
	if c.DeviceName == "" {
		return errors.New("video: deviceName 不能为空")
	}
	switch c.Source.Type {
	case "synthetic":
	case "mjpeg":
		if c.Source.URL == "" {
			return errors.New("video: source.type=mjpeg 需要 url")
		}
	case "bridge":
		if c.Source.Command == "" {
			return errors.New("video: source.type=bridge 需要 command")
		}
	default:
		return fmt.Errorf("video: source.type=%q 不支持（synthetic/mjpeg/bridge）", c.Source.Type)
	}
	if c.Inference.URL == "" {
		return errors.New("video: inference.url 不能为空")
	}
	return nil
}

// LoadConfig 从 JSON 配置文件加载并校验。
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("video: 读配置失败: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("video: 配置解析失败: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Options 是 Mapper 可选项（测试与装配注入）。
type Option func(*VideoMapper)

// WithLedger 注入台账（nil = 不留痕）。
func WithLedger(l *metamanager.Ledger) Option {
	return func(m *VideoMapper) { m.ledger = l }
}

// WithEventPublisher 注入事件上行（nil = 不上行）。
func WithEventPublisher(p EventPublisher) Option {
	return func(m *VideoMapper) { m.publisher = p }
}

// WithSource 覆盖帧源工厂（测试注入 stub；生产走 cfg.Source 构造合成源）。
func WithSource(f func(cfg *Config) (pkgvideo.FrameSource, error)) Option {
	return func(m *VideoMapper) { m.sourceFactory = f }
}

// WithInferencer 覆盖推理器构造（测试注入 stub）。
func WithInferencer(f func(cfg *Config) pkgvideo.Inferencer) Option {
	return func(m *VideoMapper) { m.inferFactory = f }
}

// metrics 是指标面快照（互斥保护；数字面经 Collect 汇入影子）。
type metrics struct {
	framesTotal    uint64
	inferTotal     uint64
	inferFail      uint64
	sourceErrors   uint64 // v0.34.0：帧源错误导致的拉流终止次数（降级可见）
	detectionsLast float64
	avgScoreLast   float64
	frameSeqLast   float64
	fpsEma         float64
}

// VideoMapper 是视频流设备 Mapper：默认启动即拉流推理（配置即运行）；
// stream 指令可运行中启停。
type VideoMapper struct {
	cfg       *Config
	ledger    *metamanager.Ledger
	publisher EventPublisher

	sourceFactory func(cfg *Config) (pkgvideo.FrameSource, error)
	inferFactory  func(cfg *Config) pkgvideo.Inferencer

	mu       sync.Mutex
	running  bool
	stopCh   chan struct{} // 当前 run 的停机信号（Start 重建）
	stopOnce *sync.Once    // 当前 run 的 close(stopCh) 唯一入口（Stop 与源错误自收口共用；v0.34.0）
	runDone  chan struct{} // 当前 run 全部 goroutine 退出后关闭（Stop 等待收口）
	met      metrics
	slot     *pkgvideo.LatestSlot
}

// NewMapper 创建视频 Mapper（不启动；Start 启动流）。
func NewMapper(cfg *Config, opts ...Option) (*VideoMapper, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	m := &VideoMapper{
		cfg:  cfg,
		slot: pkgvideo.NewLatestSlot(),
	}
	for _, o := range opts {
		o(m)
	}
	if m.sourceFactory == nil {
		m.sourceFactory = func(c *Config) (pkgvideo.FrameSource, error) {
			return pkgvideo.NewSource(pkgvideo.SourceConfig{
				Type:        c.Source.Type,
				URL:         c.Source.URL,
				Command:     c.Source.Command,
				Args:        c.Source.Args,
				ReconnectMs: c.Source.ReconnectMs,
				TimeoutMs:   c.Source.TimeoutMs,
				Synth:       c.Source.Synth,
			})
		}
	}
	if m.inferFactory == nil {
		m.inferFactory = func(c *Config) pkgvideo.Inferencer {
			return pkgvideo.NewHTTPInferencer(c.Inference.URL, c.DeviceName, c.Inference.Model,
				time.Duration(c.Inference.TimeoutMs)*time.Millisecond)
		}
	}
	return m, nil
}

// Name 实现 DeviceMapper。
func (m *VideoMapper) Name() string { return DefaultName }

// DeviceNames 实现 DeviceNameResolver（按设备名路由）。
func (m *VideoMapper) DeviceNames() []string { return []string{m.cfg.DeviceName} }

// DeviceNamespace 实现 DeviceNamespaceResolver。
func (m *VideoMapper) DeviceNamespace() string { return m.cfg.Namespace }

// Start 实现 DeviceMapper：启动拉流推理（幂等，已运行直接返回）。
// 生命周期收口（复核 P1-1 修复）：每次 run 使用独立的局部 WaitGroup 与
// runDone 通道——Stop 关当前 stopCh 后等 runDone；再次 Start 重建两者，
// 重启序列（stream 0→1→0→1…）无共享状态残留，不会卡死后续指令。
func (m *VideoMapper) Start(_ context.Context) error {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return nil
	}
	src, err := m.sourceFactory(m.cfg)
	if err != nil {
		m.mu.Unlock()
		return fmt.Errorf("video: 构造帧源失败: %w", err)
	}
	infer := m.inferFactory(m.cfg)
	m.stopCh = make(chan struct{})
	m.stopOnce = &sync.Once{}
	m.runDone = make(chan struct{})
	stop := m.stopCh
	done := m.runDone
	m.running = true
	m.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(2)
	go m.produceLoop(stop, &wg, src)
	go m.inferLoop(stop, &wg, infer)
	go func() {
		wg.Wait()
		m.mu.Lock()
		if m.stopCh == stop { // 本 run 仍是当前 run（未被新 run 取代）
			// v0.34.0：running 统一在收口处置 false——Stop 正常停止与
			// 源错误自收口共用同一路径（streamOn 随 Collect 自然下降）。
			m.running = false
		}
		m.mu.Unlock()
		close(done)
	}()
	log.Infof("VideoMapper %s: 拉流推理已启动（source=%s，inference=%s）",
		m.cfg.DeviceName, m.cfg.Source.Type, m.cfg.Inference.URL)
	return nil
}

// Stop 实现 DeviceMapper：停止拉流推理（幂等；指标保留供 Collect 读取）。
// stopOnce 保证 stopCh 绝不二次 close（v0.34.0：与源错误自收口共用）；
// running=false 由收口 goroutine 置（<-done 后可见），重启序列无残留。
func (m *VideoMapper) Stop() error {
	m.mu.Lock()
	if !m.running {
		m.mu.Unlock()
		return nil
	}
	stop := m.stopCh
	once := m.stopOnce
	done := m.runDone
	m.mu.Unlock()
	if once != nil {
		once.Do(func() { close(stop) })
	}
	<-done
	log.Infof("VideoMapper %s: 已停止", m.cfg.DeviceName)
	return nil
}

// stopRun 触发当前 run 停机（幂等；Stop 与源错误自收口共用 stopOnce）。
func (m *VideoMapper) stopRun() {
	m.mu.Lock()
	stop := m.stopCh
	once := m.stopOnce
	m.mu.Unlock()
	if once != nil && stop != nil {
		once.Do(func() { close(stop) })
	}
}

// HandleCommand 实现 DeviceMapper：property=stream（value 1/0 运行中启停），
// 其余拒绝。
func (m *VideoMapper) HandleCommand(cmd mapper.DeviceCommand) (mapper.DeviceReport, error) {
	if cmd.Property != "stream" {
		return mapper.DeviceReport{}, fmt.Errorf("video: 未知属性 %q（支持 stream）", cmd.Property)
	}
	if cmd.Value >= 1 {
		if err := m.Start(context.Background()); err != nil {
			return mapper.DeviceReport{}, err
		}
	} else {
		// 运行中暂停：Stop 停循环；已停则幂等。
		if err := m.Stop(); err != nil {
			return mapper.DeviceReport{}, err
		}
	}
	return m.snapshot(), nil
}

// Collect 实现 DeviceMapper：返回指标面数字（影子上报链消费）；
// framesDropped 实时读自背压槽（槽计数是唯一真相源）。
func (m *VideoMapper) Collect() (map[string]float64, error) {
	m.mu.Lock()
	met := m.met
	running := m.running
	m.mu.Unlock()
	props := map[string]float64{
		"framesTotal":    float64(met.framesTotal),
		"inferTotal":     float64(met.inferTotal),
		"inferFailTotal": float64(met.inferFail),
		"sourceErrors":   float64(met.sourceErrors),
		"framesDropped":  float64(m.slot.Dropped()),
		"detectionsLast": met.detectionsLast,
		"avgScoreLast":   met.avgScoreLast,
		"frameSeqLast":   met.frameSeqLast,
		"fps":            met.fpsEma,
	}
	if running {
		props["streamOn"] = 1
	} else {
		props["streamOn"] = 0
	}
	return props, nil
}

// produceLoop 出帧循环：按源节奏产帧入背压槽；stop 退出。
// v0.34.0：出帧错误（源错误）→ sourceErrors++ + 全 run 自收口（可见降级，
// KNOWN-ISSUES §32 登记项闭环）；stop 关闭仍走正常退出（不计数）。
func (m *VideoMapper) produceLoop(stop <-chan struct{}, wg *sync.WaitGroup, src pkgvideo.FrameSource) {
	defer wg.Done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if c, ok := src.(io.Closer); ok {
		// 循环终止（含「帧交付间隙 select <-stop 退出」路径）统一回收源：
		// 桥源进程须保证 Wait 回收（v0340 复核 P1——无僵尸残留）。
		defer func() { _ = c.Close() }()
	}
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	for {
		f, err := src.Next(ctx)
		if err != nil {
			select {
			case <-stop:
				return
			default:
			}
			if ctx.Err() != nil {
				return
			}
			// v0.34.0 可见降级（v0310 复核 P2-1 闭环）：源错误不再静默
			// 退出——记录 sourceErrors + 全 run 自收口（infer 循环同步停，
			// running→false 使 Collect 的 streamOn=0）；stream=1 可重启。
			m.mu.Lock()
			m.met.sourceErrors++
			errs := m.met.sourceErrors // 锁内取值：日志行不裸读共享字段（v0340 复核 P2-5）
			m.mu.Unlock()
			log.Warnf("VideoMapper %s: 拉流终止（源错误，streamOn→0，累计 %d 次）: %v",
				m.cfg.DeviceName, errs, err)
			m.stopRun()
			return
		}
		m.mu.Lock()
		m.met.framesTotal++
		m.mu.Unlock()
		m.slot.Put(f)
		select {
		case <-stop:
			return
		default:
		}
	}
}

// inferLoop 推理循环：取最新帧 → 推理 → 指标/留痕/事件；stop 退出。
func (m *VideoMapper) inferLoop(stop <-chan struct{}, wg *sync.WaitGroup, infer pkgvideo.Inferencer) {
	defer wg.Done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	var lastSeq uint64
	var lastInferAt time.Time
	for {
		f, ok := m.slot.Take(ctx, lastSeq)
		if !ok {
			return // ctx 取消（stop）
		}
		lastSeq = f.Seq
		res, err := infer.Infer(ctx, f)
		now := time.Now()
		if !lastInferAt.IsZero() {
			if dt := now.Sub(lastInferAt).Seconds(); dt > 0 {
				fps := 1 / dt
				m.mu.Lock()
				if m.met.fpsEma == 0 {
					m.met.fpsEma = fps
				} else {
					m.met.fpsEma = 0.3*fps + 0.7*m.met.fpsEma
				}
				m.mu.Unlock()
			}
		}
		lastInferAt = now
		if err != nil {
			select {
			case <-stop:
				return
			default:
			}
			m.mu.Lock()
			m.met.inferFail++
			m.mu.Unlock()
			log.Warnf("VideoMapper %s: 第 %d 帧推理失败: %v", m.cfg.DeviceName, f.Seq, err)
			m.saveOp(f, nil, err)
			continue
		}
		if res == nil {
			m.mu.Lock()
			m.met.inferFail++
			m.mu.Unlock()
			continue
		}
		m.recordResult(f, res)
		m.saveOp(f, res, nil)
		m.publish(res)
		select {
		case <-stop:
			return
		default:
		}
	}
}

// recordResult 更新指标面；结果 seq 为 0（服务端省略）时以帧序兜底
// （HTTPInferencer 已回填，此处防御直接构造的结果）。
func (m *VideoMapper) recordResult(f *pkgvideo.Frame, res *pkgvideo.InferenceResult) {
	var sum float64
	for _, d := range res.Detections {
		sum += d.Score
	}
	avg := 0.0
	if len(res.Detections) > 0 {
		avg = sum / float64(len(res.Detections))
	}
	seq := res.FrameSeq
	if seq == 0 {
		seq = f.Seq
	}
	m.mu.Lock()
	m.met.inferTotal++
	m.met.detectionsLast = float64(len(res.Detections))
	m.met.avgScoreLast = avg
	m.met.frameSeqLast = float64(seq)
	m.mu.Unlock()
}

// saveOp 推理结果台账留痕（ledger 未注入零副作用）：Direction=up（台账
// 方向白名单仅 up/down，推理结果属上报语义），RegAddr=frame:帧序，
// Value=检测框数，Message=截断 JSON（512B）。
func (m *VideoMapper) saveOp(f *pkgvideo.Frame, res *pkgvideo.InferenceResult, err error) {
	if m.ledger == nil {
		return
	}
	rec := metamanager.OpRecord{
		Ts:        time.Now().UnixMilli(),
		DeviceID:  m.cfg.DeviceName,
		Direction: metamanager.DirUp,
		RegAddr:   "frame:" + strconv.FormatUint(f.Seq, 10),
		Result:    "ok",
	}
	if err != nil {
		rec.Result = "error"
		rec.Message = err.Error()
	} else {
		rec.Value = strconv.Itoa(len(res.Detections))
		if b, jerr := json.Marshal(res); jerr == nil {
			rec.Message = pkgvideo.SanitizeInferenceJSON(string(b), 512)
		}
	}
	if serr := m.ledger.SaveOp(rec); serr != nil {
		log.Warnf("VideoMapper %s: 台账记录失败: %v", m.cfg.DeviceName, serr)
	}
}

// publish 推理结果事件上行（publisher 未注入零副作用）；发布失败只告警
// 不影响推理循环。
func (m *VideoMapper) publish(res *pkgvideo.InferenceResult) {
	if m.publisher == nil {
		return
	}
	b, err := json.Marshal(res)
	if err != nil {
		log.Warnf("VideoMapper %s: 结果编码失败: %v", m.cfg.DeviceName, err)
		return
	}
	if err := m.publisher.Publish(fmt.Sprintf(InferenceTopicTpl, m.cfg.DeviceName), b); err != nil {
		log.Warnf("VideoMapper %s: 结果事件发布失败: %v", m.cfg.DeviceName, err)
	}
}

// snapshot 组装设备状态快照（指令返回值）。
func (m *VideoMapper) snapshot() mapper.DeviceReport {
	props, _ := m.Collect()
	return mapper.DeviceReport{
		DeviceName: m.cfg.DeviceName,
		Namespace:  m.cfg.Namespace,
		Properties: props,
		ReportedAt: time.Now().UnixMilli(),
	}
}
