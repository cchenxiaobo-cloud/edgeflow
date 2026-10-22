// Package forwardstore 提供告警事件 webhook 转发（v0.53.0，spec 0026，
// replan G18「与物联网平台/数据湖标准接口对接（数据转发出口）」起点）。
//
// 职责：
//   - 转发规则（Rule）：目标 URL（http/https 绝对地址 ≤512B）+ 可选 HMAC-SHA256
//     签名密钥（≤128B；API GET 面不回显，只给 hasSecret）+ 事件过滤
//     （alarm.raised/acked/assigned/closed 非空子集）+ 启用位；JSON 快照持久化
//     （EDGEFLOW_CLOUDCORE_FORWARD_FILE，空=内存，对齐治理面快照惯例）。
//   - 送达台账（Delivery）：每规则环形保留 100 条（内存+快照）；attempts ≤3
//     （重试退避 500ms/2s），status delivered|failed。
//   - Dispatch：alarmstore 迁移挂点（SetTransitionHook 装配）入口——匹配启用规则 →
//     每规则一 goroutine 投递（POST JSON + X-EdgeFlow-Signature: sha256=<hmac-hex>，
//     单次尝试超时 5s）；goroutine panic 兜底，转发异常不伤告警主流程。
//
// 并发：规则/台账写共用 s.mu；投递在后台 goroutine，完成后 RecordDelivery 取锁
// 追加。台账随规则删除（审计长留档走告警台账/审计台账口径，本包不承担）。
package forwardstore

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"edgeflow/pkg/alarm"
	"edgeflow/pkg/log"
	"edgeflow/pkg/mqtt"
)

// 限额与常量（spec 0026 裁决）。
const (
	maxNameLen      = 64  // name 字节上限
	maxURLLen       = 512 // targetUrl 字节上限
	maxSecretLen    = 128 // secret 字节上限
	maxTopicLen     = 256 // mqtt sink topic 字节上限（v0.59.0）
	maxDeliveries   = 100 // 每规则台账环形上限
	maxAttempts     = 3   // 单次投递尝试上限
	dispatchTimeout = 5 * time.Second
)

// retryBackoff 是重试退避序列（attempt 2 前等 500ms，attempt 3 前等 2s）。
var retryBackoff = [maxAttempts - 1]time.Duration{500 * time.Millisecond, 2 * time.Second}

// ValidEvents 是合法事件全集（= "alarm." + alarm 状态名，与挂点事件名一致）。
var ValidEvents = []string{"alarm.raised", "alarm.acked", "alarm.assigned", "alarm.closed"}

// 错误面（API 层归一到状态码）。
var (
	ErrInvalid  = errors.New("forwardstore: 规则校验失败")
	ErrNotFound = errors.New("forwardstore: 规则不存在")
)

// Rule 是一条转发规则（Secret 仅持久化与投递签名用，API 读面输出 RuleView）。
type Rule struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	TargetURL string   `json:"targetUrl"`
	SinkType  string   `json:"sinkType"`        // v0.59.0：webhook（默认）| mqtt
	Topic     string   `json:"topic,omitempty"` // v0.59.0：mqtt sink 目标主题（空 = edgeflow/forward/<id>）
	Secret    string   `json:"secret,omitempty"`
	Events    []string `json:"events"`
	Enabled   bool     `json:"enabled"`
	CreatedAt int64    `json:"createdAt"`
	UpdatedAt int64    `json:"updatedAt"`
}

// RuleView 是 Rule 的脱敏视图（GET 面输出；Secret 不回显）。
type RuleView struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	TargetURL string   `json:"targetUrl"`
	SinkType  string   `json:"sinkType"`
	Topic     string   `json:"topic,omitempty"`
	HasSecret bool     `json:"hasSecret"`
	Events    []string `json:"events"`
	Enabled   bool     `json:"enabled"`
	CreatedAt int64    `json:"createdAt"`
	UpdatedAt int64    `json:"updatedAt"`
}

func (r *Rule) view() RuleView {
	return RuleView{
		ID: r.ID, Name: r.Name, TargetURL: r.TargetURL,
		SinkType: r.SinkType, Topic: r.Topic,
		HasSecret: r.Secret != "", Events: r.Events,
		Enabled: r.Enabled, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

// Delivery 是一次投递的台账记录（含全部重试的最终结果）。
type Delivery struct {
	ID         string `json:"id"`
	RuleID     string `json:"ruleId"`
	Event      string `json:"event"`
	AlarmID    string `json:"alarmId"`
	NodeID     string `json:"nodeId"`
	Severity   string `json:"severity,omitempty"`
	At         int64  `json:"at"` // 触发毫秒
	Attempts   int    `json:"attempts"`
	Status     string `json:"status"` // delivered | failed
	RespCode   int    `json:"respCode,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"durationMs"` // 含重试墙钟
}

// payload 是投递请求体。
type payload struct {
	Event    string `json:"event"`
	AlarmID  string `json:"alarmId"`
	NodeID   string `json:"nodeId"`
	State    string `json:"state"`
	Severity string `json:"severity"`
	Count    int64  `json:"count,omitempty"`
	Message  string `json:"message,omitempty"`
	At       int64  `json:"at"`
}

// UpdateReq 是 PUT 的部分更新请求（nil 字段=保留原值；Secret 指向空串=清除）。
type UpdateReq struct {
	Name      *string
	SinkType  *string
	TargetURL *string
	Topic     *string
	Secret    *string
	Events    []string
	Enabled   *bool
}

// snapshot 是持久化形态（rules 键控 + deliveries 按规则环形）。
type snapshot struct {
	Version    int                   `json:"version"`
	Rules      map[string]*Rule      `json:"rules"`
	Deliveries map[string][]Delivery `json:"deliveries"`
}

// Store 是转发规则与送达台账管理器。
type Store struct {
	mu     sync.Mutex
	file   string
	data   snapshot
	client *http.Client

	mutes MuteSource // v0.62.0 可选（spec 0035）；nil = 无静默过滤
}

// MuteSource 是静默窗口源（v0.62.0，spec 0035 裁决 1）：节点+级别处于生效
// 静默窗口的告警事件不投递（通知抑制，spec 0031 裁决 6 承接）。可选接口——
// 未设置时行为不变。与 hierarchyview.MuteSource 同型但含 severity 维度
// （避免跨包依赖，各自定义；口径差异见 mutestore.MatchesEvent）。
type MuteSource interface {
	MutedEvent(nodeID, severity string, nowMs int64) bool
}

// SetMutes 装配静默窗口源（启动期一次性调用；mutestore.Store 实现之）。
func (s *Store) SetMutes(m MuteSource) { s.mutes = m }

// New 构建存储（file 空 = 纯内存；Load() 触发恢复）。
func New(file string) *Store {
	return &Store{
		file:   file,
		client: &http.Client{Timeout: dispatchTimeout},
	}
}

// Load 从快照恢复（文件缺失=空库；损坏=报错由调用方决策降级）。
func (s *Store) Load() error {
	if s.file == "" {
		return nil
	}
	data, err := os.ReadFile(s.file)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("forwardstore: 读取快照失败: %w", err)
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("forwardstore: 快照解析失败: %w", err)
	}
	if snap.Rules == nil {
		snap.Rules = map[string]*Rule{}
	}
	if snap.Deliveries == nil {
		snap.Deliveries = map[string][]Delivery{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = snap
	return nil
}

// Create 创建转发规则；校验失败返回 ErrInvalid（消息含原因）。
func (s *Store) Create(name, sinkType, targetURL, topic, secret string, events []string, enabled bool, nowMs int64) (Rule, error) {
	r, err := newRule(name, sinkType, targetURL, topic, secret, events, enabled, nowMs)
	if err != nil {
		return Rule{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Rules == nil {
		s.data.Rules = map[string]*Rule{}
	}
	cp := r
	s.data.Rules[r.ID] = &cp
	if err := s.persistLocked(); err != nil {
		delete(s.data.Rules, r.ID)
		return Rule{}, err
	}
	return r, nil
}

// Update 部分更新规则（先全量校验副本，通过才落盘；失败回滚内存态）。
func (s *Store) Update(id string, req UpdateReq, nowMs int64) (Rule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.data.Rules[id]
	if !ok {
		return Rule{}, ErrNotFound
	}
	next := *cur
	if req.Name != nil {
		next.Name = strings.TrimSpace(*req.Name)
	}
	if req.SinkType != nil {
		next.SinkType = strings.TrimSpace(*req.SinkType)
		if next.SinkType == "" {
			next.SinkType = "webhook"
		}
	}
	if req.TargetURL != nil {
		next.TargetURL = strings.TrimSpace(*req.TargetURL)
	}
	if req.Topic != nil {
		next.Topic = strings.TrimSpace(*req.Topic)
	}
	if req.Secret != nil {
		next.Secret = strings.TrimSpace(*req.Secret)
	}
	if req.Events != nil {
		next.Events = normalizeEvents(req.Events)
	}
	if req.Enabled != nil {
		next.Enabled = *req.Enabled
	}
	if err := validateRuleFields(next.Name, next.SinkType, next.TargetURL, next.Topic, next.Secret, next.Events); err != nil {
		return Rule{}, err
	}
	next.UpdatedAt = nowMs
	cp := next
	s.data.Rules[id] = &cp
	if err := s.persistLocked(); err != nil {
		s.data.Rules[id] = cur
		return Rule{}, err
	}
	return next, nil
}

// Delete 删除规则（台账随规则删除）。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.Rules[id]; !ok {
		return ErrNotFound
	}
	delete(s.data.Rules, id)
	delete(s.data.Deliveries, id)
	return s.persistLocked()
}

// List 返回全部规则的脱敏视图（创建时间升序；空库返回空切片非 nil）。
func (s *Store) List() []RuleView {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RuleView, 0, len(s.data.Rules))
	for _, r := range s.data.Rules {
		out = append(out, r.view())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

// Get 返回单条规则的脱敏视图。
func (s *Store) Get(id string) (RuleView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.data.Rules[id]
	if !ok {
		return RuleView{}, false
	}
	return r.view(), true
}

// Deliveries 返回规则送达台账（新→旧；limit<=0 默认 50，钳 [1,200]）。
func (s *Store) Deliveries(ruleID string, limit int) []Delivery {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.data.Deliveries[ruleID]
	out := make([]Delivery, 0, len(src))
	for i := len(src) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, src[i])
	}
	return out
}

// Dispatch 是告警状态迁移挂点入口（alarmstore.SetTransitionHook 装配）：
// 拷贝匹配规则后立即返回（投递异步，每规则一 goroutine）。
// 在告警 store 锁外被调用；回调方约定不在本方法内同步等待投递结果。
func (s *Store) Dispatch(event string, a alarm.Alarm) {
	if s == nil {
		return
	}
	now0 := time.Now().UnixMilli()
	// v0.62.0（spec 0035 裁决 1）：静默联动通知抑制——节点+级别处于生效静默
	// 窗口 → 事件不投递、不记台账（通知未发生即无事件；severity 维度在此
	// 生效，层级活跃口径仍维持节点级）。未装配静默源时跳过判定。
	if s.mutes != nil && s.mutes.MutedEvent(a.NodeID, a.Severity, now0) {
		return
	}
	s.mu.Lock()
	matched := make([]Rule, 0, 2)
	for _, r := range s.data.Rules {
		if r == nil || !r.Enabled {
			continue
		}
		if slices.Contains(r.Events, event) {
			matched = append(matched, *r)
		}
	}
	s.mu.Unlock()
	if len(matched) == 0 {
		return
	}
	now := time.Now().UnixMilli()
	for _, r := range matched {
		go func(r Rule) {
			defer func() {
				if p := recover(); p != nil {
					log.Warnf("[forwardstore] 投递 panic 已兜底（rule=%s event=%s）: %v", r.ID, event, p)
				}
			}()
			s.deliver(r, event, a, now)
		}(r)
	}
}

// deliver 执行单规则投递（异步入口）：构造 payload → 同步投递 → 台账记录。
func (s *Store) deliver(rule Rule, event string, a alarm.Alarm, at int64) {
	body, err := json.Marshal(payload{
		Event: event, AlarmID: a.AlarmID, NodeID: a.NodeID,
		State: a.State, Severity: a.Severity, Count: a.Count,
		Message: a.Message, At: at,
	})
	if err != nil { // 不可达（纯 map/slice 结构）；防御性记失败
		s.recordDelivery(Delivery{ID: newSeqID(at), RuleID: rule.ID, Event: event,
			AlarmID: a.AlarmID, NodeID: a.NodeID, At: at, Status: "failed", Error: err.Error()})
		return
	}
	s.recordDelivery(s.deliverSync(rule, event, a, at, body))
}

// deliverSync 同步执行投递（webhook HTTP / mqtt publish 双路；≤maxAttempts 次；
// 成功 = 2xx / QoS1 确认完成），返回台账记录（不落台账——调用方负责）。
// v0.59.0（spec 0032 裁决 3）：按 sinkType 分叉。
func (s *Store) deliverSync(rule Rule, event string, a alarm.Alarm, at int64, body []byte) Delivery {
	start := time.Now()
	attempts := 0
	lastCode := 0
	lastErr := ""
	mk := func(status, errMsg string, respCode int) Delivery {
		return Delivery{ID: newSeqID(at), RuleID: rule.ID, Event: event,
			AlarmID: a.AlarmID, NodeID: a.NodeID, Severity: a.Severity,
			At: at, Attempts: attempts, Status: status,
			RespCode: respCode, Error: errMsg, DurationMs: time.Since(start).Milliseconds()}
	}
	if rule.SinkType == "mqtt" {
		// v0.59.0：mqtt sink——pkg/mqtt 短连接 QoS1 发布到目标 broker 主题
		//（成功 = Publish QoS1 确认完成；单轮 ack 上限 = mqtt 包内 ackTimeout）。
		topic := rule.Topic
		if topic == "" {
			topic = "edgeflow/forward/" + rule.ID
		}
		for i := 0; i < maxAttempts; i++ {
			attempts++
			if perr := mqttPublishQoS1(rule.TargetURL, topic, body); perr != nil {
				lastErr = perr.Error()
			} else {
				d := mk("delivered", "", 0)
				return d
			}
			if i < maxAttempts-1 {
				time.Sleep(retryBackoff[i])
			}
		}
		return mk("failed", lastErr, 0)
	}
	// webhook 路径（既有语义）。
	for i := 0; i < maxAttempts; i++ {
		attempts++
		req, rerr := http.NewRequest(http.MethodPost, rule.TargetURL, bytes.NewReader(body))
		if rerr != nil { // URL 非法（创建时已校验，防御）：不重试
			lastErr = rerr.Error()
			break
		}
		req.Header.Set("Content-Type", "application/json")
		if rule.Secret != "" {
			mac := hmac.New(sha256.New, []byte(rule.Secret))
			mac.Write(body)
			req.Header.Set("X-EdgeFlow-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		}
		resp, derr := s.client.Do(req)
		if derr == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			lastCode = resp.StatusCode
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return mk("delivered", "", lastCode)
			}
			lastErr = fmt.Sprintf("HTTP %d", resp.StatusCode)
		} else {
			lastErr = derr.Error()
		}
		if i < maxAttempts-1 {
			time.Sleep(retryBackoff[i])
		}
	}
	return mk("failed", lastErr, lastCode)
}

// mqttPublishQoS1 短连接发布：dial → PUBLISH(QoS1) → 等 PUBACK → DISCONNECT。
// 每次投递独立连接（低频事件面量级假设；连接池留需时立项）。
func mqttPublishQoS1(brokerAddr, topic string, payload []byte) error {
	// v0.61.0（R3 P2-2 处置）：校验器放行 [tcp://]mqtt:// 前缀形态，拨号前剥除
	//（net.Dial 不识别 mqtt:// scheme——不剥则被接受的配置永远投递失败）。
	addr := strings.TrimPrefix(strings.TrimPrefix(brokerAddr, "tcp://"), "mqtt://")
	c, err := mqtt.Dial(addr, mqtt.Options{
		ClientID:         "edgeflow-forward-" + newSeqID(time.Now().UnixMilli()),
		ProtocolVersion5: true,
	})
	if err != nil {
		return fmt.Errorf("mqtt dial: %w", err)
	}
	defer func() { _ = c.Close() }()
	return c.Publish(topic, 1, payload)
}

// TestDelivery 同步执行一次测试投递（事件名 "test"，绕过事件过滤——连通性
// 验证；spec 0032 裁决 2，KI §54#2 处置）。结果记台账并返回。
func (s *Store) TestDelivery(id string, nowMs int64) (Delivery, error) {
	s.mu.Lock()
	rule, ok := s.data.Rules[id]
	if !ok {
		s.mu.Unlock()
		return Delivery{}, ErrNotFound
	}
	r := *rule
	s.mu.Unlock()
	if !r.Enabled {
		return Delivery{}, fmt.Errorf("%w: 规则已禁用（启用后再测试）", ErrInvalid)
	}
	testAlarm := alarm.Alarm{AlarmID: "test-" + r.ID, NodeID: "-", Source: alarm.SourceSystem,
		Severity: alarm.SeverityInfo, State: alarm.StateRaised, Count: 1,
		RaisedAt: nowMs, UpdatedAt: nowMs, Message: "连通性测试投递"}
	body, err := json.Marshal(payload{
		Event: "test", AlarmID: testAlarm.AlarmID, NodeID: testAlarm.NodeID,
		State: testAlarm.State, Severity: testAlarm.Severity,
		Message: testAlarm.Message, At: nowMs,
	})
	if err != nil {
		return Delivery{}, fmt.Errorf("forwardstore: 测试 payload 序列化失败: %w", err)
	}
	d := s.deliverSync(r, "test", testAlarm, nowMs, body)
	s.recordDelivery(d)
	return d, nil
}

// recordDelivery 追加台账（环形截断；尽力持久化，失败不阻断投递）。
func (s *Store) recordDelivery(d Delivery) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Deliveries == nil {
		s.data.Deliveries = map[string][]Delivery{}
	}
	list := append(s.data.Deliveries[d.RuleID], d)
	if len(list) > maxDeliveries {
		list = list[len(list)-maxDeliveries:]
	}
	s.data.Deliveries[d.RuleID] = list
	if err := s.persistLocked(); err != nil {
		log.Warnf("[forwardstore] 台账持久化失败（rule=%s）: %v", d.RuleID, err)
	}
}

// persistLocked 原子写快照（temp+rename；file 空=内存模式跳过）。调用方持锁。
func (s *Store) persistLocked() error {
	if s.file == "" {
		return nil
	}
	s.data.Version = 1
	data, err := json.MarshalIndent(&s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("forwardstore: 快照序列化失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.file), 0o700); err != nil {
		return fmt.Errorf("forwardstore: 创建快照目录失败: %w", err)
	}
	tmp := s.file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("forwardstore: 写快照失败: %w", err)
	}
	return os.Rename(tmp, s.file)
}

// newRule 构建并校验新规则（trim + 事件去重 + 字段校验 + id 生成）。
func newRule(name, sinkType, targetURL, topic, secret string, events []string, enabled bool, nowMs int64) (Rule, error) {
	name = strings.TrimSpace(name)
	sinkType = strings.TrimSpace(sinkType)
	if sinkType == "" {
		sinkType = "webhook"
	}
	targetURL = strings.TrimSpace(targetURL)
	topic = strings.TrimSpace(topic)
	secret = strings.TrimSpace(secret)
	events = normalizeEvents(events)
	if err := validateRuleFields(name, sinkType, targetURL, topic, secret, events); err != nil {
		return Rule{}, err
	}
	id, err := randID("fw")
	if err != nil {
		return Rule{}, err
	}
	return Rule{ID: id, Name: name, TargetURL: targetURL, SinkType: sinkType, Topic: topic, Secret: secret,
		Events: events, Enabled: enabled, CreatedAt: nowMs, UpdatedAt: nowMs}, nil
}

// validateRuleFields 校验规则字段（Create/Update 共用）。地址语义按 sinkType 分叉：
// webhook = http(s) 绝对地址（既有语义）；mqtt = [tcp://]host:port（v0.59.0，spec 0032）。
func validateRuleFields(name, sinkType, targetURL, topic, secret string, events []string) error {
	if name == "" || len(name) > maxNameLen {
		return fmt.Errorf("%w: name 须 1..%d 字节", ErrInvalid, maxNameLen)
	}
	switch sinkType {
	case "", "webhook":
		if targetURL == "" || len(targetURL) > maxURLLen {
			return fmt.Errorf("%w: targetUrl 须 1..%d 字节", ErrInvalid, maxURLLen)
		}
		u, err := url.Parse(targetURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%w: targetUrl 须为 http(s) 绝对地址", ErrInvalid)
		}
	case "mqtt":
		if targetURL == "" || len(targetURL) > maxURLLen {
			return fmt.Errorf("%w: targetUrl 须 1..%d 字节", ErrInvalid, maxURLLen)
		}
		host := strings.TrimPrefix(strings.TrimPrefix(targetURL, "tcp://"), "mqtt://")
		if host == "" || strings.Contains(host, "/") {
			return fmt.Errorf("%w: mqtt sink targetUrl 须为 [tcp://]host:port", ErrInvalid)
		}
		if _, _, serr := net.SplitHostPort(host); serr != nil {
			return fmt.Errorf("%w: mqtt sink targetUrl 缺少端口", ErrInvalid)
		}
		if len(topic) > maxTopicLen {
			return fmt.Errorf("%w: topic 须 ≤%d 字节", ErrInvalid, maxTopicLen)
		}
	default:
		return fmt.Errorf("%w: 非法 sinkType %q（webhook|mqtt）", ErrInvalid, sinkType)
	}
	if len(secret) > maxSecretLen {
		return fmt.Errorf("%w: secret 须 ≤%d 字节", ErrInvalid, maxSecretLen)
	}
	if len(events) == 0 {
		return fmt.Errorf("%w: events 须为非空事件子集 %v", ErrInvalid, ValidEvents)
	}
	for _, ev := range events {
		if !slices.Contains(ValidEvents, ev) {
			return fmt.Errorf("%w: 非法事件 %q（合法全集 %v）", ErrInvalid, ev, ValidEvents)
		}
	}
	return nil
}

// normalizeEvents 事件去重保序。
func normalizeEvents(in []string) []string {
	out := make([]string, 0, len(in))
	for _, ev := range in {
		ev = strings.TrimSpace(ev)
		if ev != "" && !slices.Contains(out, ev) {
			out = append(out, ev)
		}
	}
	return out
}

// randID 生成 <prefix>-<8 字节随机 hex>。
func randID(prefix string) (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("forwardstore: 生成 id 失败: %w", err)
	}
	return prefix + "-" + hex.EncodeToString(buf[:]), nil
}

// newSeqID 生成台账记录 id：dl-<触发毫秒>-<4 字节随机 hex>
// （同毫秒并发投递由随机段规避重号；ID 不作索引，仅审计可读）。
func newSeqID(atMs int64) string {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("dl-%d", atMs) // 随机源退化：ID 可读性降级不阻断投递
	}
	return fmt.Sprintf("dl-%d-%s", atMs, hex.EncodeToString(buf[:]))
}
