// Package etcdstore 提供嵌入式 etcd（go.etcd.io/etcd/server/v3/embed）基础层：
// 配置解析、embed 生命周期（Start/Close/就绪等待）、clientv3 KV 薄封装。
//
// 本包是 EdgeFlow v0.4.0 云端存储改造（内存态 → 嵌入式 etcd 写穿）的基础设施层，
// 被 registry / devicestatus 的 etcd 包装实现与 cmd/cloudcore 装配区消费。
// 设计依据：.cluster/edgeflow-v040/subagent_02.md（§4 配置表、§6 关停与坏库降级、
// §9 实施顺序第 2 步）与 subagent_03.md 前置条件（quota≤256MB、auto-compaction、
// 127.0.0.1 绑定、优雅关闭、kill -9 冒烟）。
package etcdstore

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// 环境变量名（与 cmd/cloudcore 装配约定一致，统一 EDGEFLOW_CLOUDCORE_ 前缀，
// 风格对齐 nodecontroller.DurationsFromEnv：非法值 fail-fast 报错，不静默回退）。
const (
	// EnvEnabled 总开关：false = 完全退回 v0.3.x 纯内存行为（不建目录、不占端口、不写盘）。
	EnvEnabled = "EDGEFLOW_CLOUDCORE_ETCD_ENABLED"
	// EnvDataDir embed 数据目录（相对工作目录，自动 MkdirAll）。
	EnvDataDir = "EDGEFLOW_CLOUDCORE_ETCD_DATA_DIR"
	// EnvClientURL 客户端监听（只绑 127.0.0.1，安全底线）。
	EnvClientURL = "EDGEFLOW_CLOUDCORE_ETCD_CLIENT_URL"
	// EnvPeerURL peer 监听（单成员，仅用于 embed 内部）。
	EnvPeerURL = "EDGEFLOW_CLOUDCORE_ETCD_PEER_URL"
	// EnvQuotaBackendBytes 后端配额（默认 256MiB，见设计 §4）。
	EnvQuotaBackendBytes = "EDGEFLOW_CLOUDCORE_ETCD_QUOTA_BACKEND_BYTES"
	// EnvAutoCompactionMode 自动压缩模式：periodic / revision。
	EnvAutoCompactionMode = "EDGEFLOW_CLOUDCORE_ETCD_AUTO_COMPACTION_MODE"
	// EnvAutoCompactionRetention 压缩保留期（periodic 为时长，支持 "1h" 或秒数）。
	EnvAutoCompactionRetention = "EDGEFLOW_CLOUDCORE_ETCD_AUTO_COMPACTION_RETENTION"
	// EnvStrict 严格模式：空=off（启动失败降级纯内存 + 告警），'1'=fail-fast（拒绝启动）。
	EnvStrict = "EDGEFLOW_CLOUDCORE_ETCD_STRICT"
	// EnvAllowInsecure 明文护栏逃生门（设计 §1.2 M10）："1" 放行非回环明文
	// 外部连接（装配区打印大告警）；"0"/空 = 关（默认，M9 拒绝启动）。
	EnvAllowInsecure = "EDGEFLOW_CLOUDCORE_ETCD_ALLOW_INSECURE"
	// EnvEndpoints 外部 etcd 模式开关（v0.5.0，方案④）：逗号分隔 URL 列表，
	// 非空且总开关 Enabled → 跳过 embed（不建目录/不占端口/忽略 DATA_DIR），
	// clientv3 直连该集群；Enabled=false 总开关优先，一刀切纯内存。
	EnvEndpoints = "EDGEFLOW_CLOUDCORE_ETCD_ENDPOINTS"
	// EnvTLSCA 外部 etcd TLS CA 证书 PEM 路径；非空即启用 TLS（https 端点）。
	EnvTLSCA = "EDGEFLOW_CLOUDCORE_ETCD_TLS_CA"
	// EnvTLSCert / EnvTLSKey 外部 etcd 客户端证书/私钥 PEM 路径（与 CA 同设即 mTLS）。
	EnvTLSCert = "EDGEFLOW_CLOUDCORE_ETCD_TLS_CERT"
	EnvTLSKey  = "EDGEFLOW_CLOUDCORE_ETCD_TLS_KEY"
)

// 默认配置（对齐设计 §4 配置表；端口选 12379/12380 避开标准 2379/2380，
// 与开发机既有真实 etcd 部署共存不冲突）。
const (
	DefaultEnabled                 = true
	DefaultDataDir                 = "data/etcd"
	DefaultClientURL               = "http://127.0.0.1:12379"
	DefaultPeerURL                 = "http://127.0.0.1:12380"
	DefaultQuotaBackendBytes       = 256 * 1024 * 1024 // 256MiB（设计约束：≤256MB）
	DefaultAutoCompactionMode      = "periodic"
	DefaultAutoCompactionRetention = 1 * time.Hour
)

// Config 是 etcdstore 基础层配置（由 ConfigFromEnv 从环境变量解析）。
type Config struct {
	// Enabled 总开关；false = 纯内存（v0.3.x 行为），不创建目录、不占端口。
	Enabled bool
	// DataDir embed 数据目录（启动时 MkdirAll 自动创建）。
	DataDir string
	// ClientURL 客户端监听地址（只允许回环）。
	ClientURL string
	// PeerURL peer 监听地址（单成员，只允许回环）。
	PeerURL string
	// QuotaBackendBytes 后端配额（字节）。
	QuotaBackendBytes int64
	// AutoCompactionMode 自动压缩模式：periodic / revision。
	AutoCompactionMode string
	// AutoCompactionRetention 压缩保留期（periodic 模式为时长）。
	AutoCompactionRetention time.Duration
	// Strict 严格模式：true = embed 启动失败即拒绝启动（fail-fast）；
	// false（默认）= 降级纯内存 + 告警（由装配区消费，见设计 §6.5）。
	Strict bool
	// Endpoints 外部 etcd 集群地址（逗号分隔 URL，v0.5.0 方案④）；
	// 非空且 Enabled → 外部模式直连，跳过 embed。
	Endpoints []string
	// CAFile / CertFile / KeyFile 外部 etcd TLS PEM 文件路径。
	// CAFile 非空 = TLS 启用；CertFile+KeyFile 同设 = mTLS 客户端证书。
	CAFile   string
	CertFile string
	KeyFile  string
	// AllowInsecure 明文护栏逃生门（仅外部模式消费）：非回环端点 + 未启用
	// TLS 时默认拒绝启动；=1 时放行但装配区打印大告警（设计 §1.2 M9/M10）。
	// 注意：Enabled=false 时该字段不解析、不校验（M1 短路的逃生语义）。
	AllowInsecure bool
	// MaxSnapFiles / MaxWALFiles 自动清理保留上限（etcd 原生语义）：0 = 禁用
	// purge 自动清理（测试环境与高保留场景）；默认 5（etcd 默认值，由
	// DefaultConfig 提供）。注意非零时 embed 每 30s 清理旧快照/WAL——测试
	// 环境中目录若先于 purge tick 被清理，etcd 内部 zap.Fatal 会直接 os.Exit
	// 杀死测试进程，故测试一律用 0 禁用；生产默认保留自动清理。
	MaxSnapFiles int
	MaxWALFiles  int
}

// DefaultConfig 返回默认配置（设计 §4 配置表的默认值全集）。
func DefaultConfig() Config {
	return Config{
		Enabled:                 DefaultEnabled,
		DataDir:                 DefaultDataDir,
		ClientURL:               DefaultClientURL,
		PeerURL:                 DefaultPeerURL,
		QuotaBackendBytes:       DefaultQuotaBackendBytes,
		AutoCompactionMode:      DefaultAutoCompactionMode,
		AutoCompactionRetention: DefaultAutoCompactionRetention,
		MaxSnapFiles:            5, // etcd 原生默认（embed.NewConfig 同值）
		MaxWALFiles:             5,
	}
}

// ConfigFromEnv 从环境变量解析配置（未设置用默认值；非法值返回错误，
// 风格对齐 nodecontroller.DurationsFromEnv：装配期 fail-fast，不静默回退）。
func ConfigFromEnv() (Config, error) {
	cfg := DefaultConfig()

	if v := os.Getenv(EnvEnabled); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("etcdstore: 环境变量 %s=%q 不是合法布尔值（true/false/1/0）", EnvEnabled, v)
		}
		cfg.Enabled = b
	}
	if v := os.Getenv(EnvDataDir); v != "" {
		cfg.DataDir = v
	}
	if v := os.Getenv(EnvClientURL); v != "" {
		cfg.ClientURL = v
	}
	if v := os.Getenv(EnvPeerURL); v != "" {
		cfg.PeerURL = v
	}
	if v := os.Getenv(EnvQuotaBackendBytes); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("etcdstore: 环境变量 %s=%q 必须为正整数（字节）", EnvQuotaBackendBytes, v)
		}
		cfg.QuotaBackendBytes = n
	}
	if v := os.Getenv(EnvAutoCompactionMode); v != "" {
		if v != "periodic" && v != "revision" {
			return Config{}, fmt.Errorf("etcdstore: 环境变量 %s=%q 必须是 periodic 或 revision", EnvAutoCompactionMode, v)
		}
		cfg.AutoCompactionMode = v
	}
	if v := os.Getenv(EnvAutoCompactionRetention); v != "" {
		d, err := parseDurationEnv(EnvAutoCompactionRetention, v)
		if err != nil {
			return Config{}, err
		}
		cfg.AutoCompactionRetention = d
	}
	if v := os.Getenv(EnvStrict); v != "" {
		switch v {
		case "1":
			cfg.Strict = true
		case "", "0":
			cfg.Strict = false
		default:
			return Config{}, fmt.Errorf("etcdstore: 环境变量 %s=%q 只支持空/'0'（off）或 '1'（fail-fast）", EnvStrict, v)
		}
	}

	// 外部模式解析（ENDPOINTS/TLS/ALLOW_INSECURE）只在 Enabled=true 时执行：
	// M1 短路——总开关关闭时不解析、不校验、不创建，配错也不阻断纯内存逃生。
	if cfg.Enabled {
		if v := os.Getenv(EnvEndpoints); v != "" {
			endpoints, err := parseEndpointList(EnvEndpoints, v)
			if err != nil {
				return Config{}, err
			}
			cfg.Endpoints = endpoints
		}
		// 外部段门控（设计 §1.4）：TLS/逃生门/一致性/护栏只在外部模式
		// （ENDPOINTS 非空）下解析校验；embed 模式完全忽略本段——外部变量
		// 配错（TLS_CERT-only、非法 ALLOW_INSECURE 等）不得阻断 embed 启动
		// （M2 回归锚点不被新外部变量串扰）。
		if len(cfg.Endpoints) > 0 {
			if v := os.Getenv(EnvTLSCA); v != "" {
				if err := requirePEMFile(EnvTLSCA, v); err != nil {
					return Config{}, err
				}
				cfg.CAFile = v
			}
			cert, key := os.Getenv(EnvTLSCert), os.Getenv(EnvTLSKey)
			switch {
			case cert != "" && key == "":
				return Config{}, fmt.Errorf("etcdstore: 环境变量 %s=%q 与 %s 必须成对设置（mTLS 客户端证书）", EnvTLSCert, cert, EnvTLSKey)
			case cert == "" && key != "":
				return Config{}, fmt.Errorf("etcdstore: 环境变量 %s=%q 与 %s 必须成对设置（mTLS 客户端证书）", EnvTLSKey, key, EnvTLSCert)
			case cert != "" && key != "":
				if err := requirePEMFile(EnvTLSCert, cert); err != nil {
					return Config{}, err
				}
				if err := requirePEMFile(EnvTLSKey, key); err != nil {
					return Config{}, err
				}
				cfg.CertFile, cfg.KeyFile = cert, key
			}
			// 客户端证书未配 CA = TLS 未启用却提供证书：无意义配置，fail-fast
			// （静默忽略会让运维误以为 mTLS 已生效，安全底线）。
			if (cfg.CertFile != "" || cfg.KeyFile != "") && cfg.CAFile == "" {
				return Config{}, fmt.Errorf("etcdstore: 设置了客户端证书但 %s 为空——TLS 未启用（CA 非空才启用 TLS）", EnvTLSCA)
			}
			if v := os.Getenv(EnvAllowInsecure); v != "" {
				switch v {
				case "1":
					cfg.AllowInsecure = true
				case "0":
					cfg.AllowInsecure = false
				default:
					return Config{}, fmt.Errorf("etcdstore: 环境变量 %s=%q 只支持空/'0'（关）或 '1'（开）", EnvAllowInsecure, v)
				}
			}
			// 设计 §2.2：scheme 与 TLS 启用状态必须一致（全 http 或全 https），
			// 混合 scheme 会让坏一半端点被 clientv3 failover 掩盖，配置期拒绝。
			if err := cfg.verifySchemeConsistency(); err != nil {
				return Config{}, err
			}
			// M9：明文护栏——非回环端点 + 未启用 TLS + 未开逃生门 = 拒绝启动。
			if err := cfg.InsecureGuardError(); err != nil {
				return Config{}, err
			}
		}
	}

	// embed 段校验（仅 embed 模式执行；外部模式忽略 CLIENT_URL/PEER_URL——
	// 设计 M3：两段配置互不串扰，外部模式下 embed 段配置被忽略而非报错）。
	if len(cfg.Endpoints) == 0 {
		if _, err := parseListenURL(EnvClientURL, cfg.ClientURL); err != nil {
			return Config{}, err
		}
		if _, err := parseListenURL(EnvPeerURL, cfg.PeerURL); err != nil {
			return Config{}, err
		}
	}
	return cfg, nil
}

// TLSEnabled 报告外部 etcd TLS 是否启用（CA 非空即启用）。
func (c Config) TLSEnabled() bool { return c.CAFile != "" }

// External 报告是否为外部 etcd 模式（ENDPOINTS 非空且总开关开启）。
// 注意：仅反映解析结果；Enabled=false 时 Endpoints 恒为空（M1 短路）。
func (c Config) External() bool { return len(c.Endpoints) > 0 }

// InsecureGuardError 实现 M9 明文护栏：外部模式下 endpoints 含非回环地址、
// 未启用 TLS、且未开逃生门 → 返回拒绝启动错误（点名 ALLOW_INSECURE 逃生指引）。
// 全回环端点组合不触发（未暴露到网络）；TLS 启用天然通过（M11）。
func (c Config) InsecureGuardError() error {
	if !c.External() || c.TLSEnabled() || c.AllowInsecure {
		return nil
	}
	for _, ep := range c.Endpoints {
		u, err := url.Parse(ep)
		if err != nil {
			continue // 已过 parseEndpointList 校验，理论不可达
		}
		if !hostIsLoopback(u.Hostname()) {
			return fmt.Errorf("etcdstore: 明文护栏（M9）——外部 etcd 端点 %q 非回环且未启用 TLS（%s）——任何能触达 %s 的客户端可读写全部键空间；设置 %s 启用 TLS，或显式设置 %s=1（明文暴露，风险自担）", ep, EnvTLSCA, u.Hostname(), EnvTLSCA, EnvAllowInsecure)
		}
	}
	return nil
}

// verifySchemeConsistency 落实设计 §2.2 的 scheme⇔TLS 一致性：TLS 启用
// （CA 非空）→ 全部端点必须 https；未启用 → 全部必须 http。混合 scheme
// 会让坏一半端点被 clientv3 failover 掩盖（TLS 关闭时 https 端点静默失效），
// 配置期直接拒绝，错误点名违规端点与 EnvEndpoints。
func (c Config) verifySchemeConsistency() error {
	want, desc := "http", "未启用 TLS"
	if c.TLSEnabled() {
		want, desc = "https", fmt.Sprintf("已启用 TLS（%s）", EnvTLSCA)
	}
	for _, ep := range c.Endpoints {
		u, err := url.Parse(ep)
		if err != nil {
			continue // 已过 parseEndpointList 校验，理论不可达
		}
		if u.Scheme != want {
			return fmt.Errorf("etcdstore: 外部 etcd 端点 %q 使用 %s 协议但当前%s——环境变量 %s 的全部端点必须一致使用 %s（混合 scheme 会导致坏端点被 failover 掩盖）", ep, u.Scheme, desc, EnvEndpoints, want)
		}
	}
	return nil
}

// hostIsLoopback 判定主机名是否为回环（127.0.0.1 / localhost / ::1 / 其他回环 IP）。
func hostIsLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// HostIsLoopback 是 hostIsLoopback 的导出包装（供 cmd/cloudcore 装配区告警判定）。
func HostIsLoopback(host string) bool { return hostIsLoopback(host) }

// EndpointHost 返回外部 etcd 端点 URL 的 host（去掉 scheme 与路径；已过
// parseEndpointList 校验，Parse 失败原样返回原始串）。
func EndpointHost(ep string) string {
	u, err := url.Parse(ep)
	if err != nil {
		return ep
	}
	return u.Hostname()
}

// BuildTLS 按配置构造 clientv3 TLS 配置：CA 池（校验服务端证书）+ 可选客户端
// 证书（mTLS）。未启用 TLS 时返回 nil（明文）。证书内容非法 → 返回错误
// （fail-fast，由装配层在连接外部集群前暴露）。
func (c Config) BuildTLS() (*tls.Config, error) {
	if !c.TLSEnabled() {
		return nil, nil
	}
	pool := x509.NewCertPool()
	pemData, err := os.ReadFile(c.CAFile)
	if err != nil {
		return nil, fmt.Errorf("etcdstore: 读取 CA 文件 %s 失败: %w", c.CAFile, err)
	}
	if !pool.AppendCertsFromPEM(pemData) {
		return nil, fmt.Errorf("etcdstore: CA 文件 %s 不含合法 PEM 证书", c.CAFile)
	}
	tlsCfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if c.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("etcdstore: 加载客户端证书失败（cert=%s key=%s）: %w", c.CertFile, c.KeyFile, err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	return tlsCfg, nil
}

// parseEndpointList 解析外部 etcd endpoints（逗号分隔 URL 列表）：
// 每条校验 scheme（http/https）+ 非空 host；空条目/非法 scheme/无法解析
// 一律 fail-fast（对齐 nodecontroller.DurationsFromEnv：装配期报错，不静默回退）。
func parseEndpointList(envName, raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	endpoints := make([]string, 0, len(parts))
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, fmt.Errorf("etcdstore: 环境变量 %s=%q 含空条目（第 %d 个，检查是否有多余逗号）", envName, raw, i+1)
		}
		u, err := url.Parse(p)
		if err != nil {
			return nil, fmt.Errorf("etcdstore: 环境变量 %s 第 %d 个 %q 不是合法 URL: %v", envName, i+1, p, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("etcdstore: 环境变量 %s 第 %d 个 %q 必须使用 http 或 https 协议（当前 %q）", envName, i+1, p, u.Scheme)
		}
		if u.Hostname() == "" {
			return nil, fmt.Errorf("etcdstore: 环境变量 %s 第 %d 个 %q 缺少主机地址（格式: http(s)://host:port）", envName, i+1, p)
		}
		if u.Port() == "" {
			return nil, fmt.Errorf("etcdstore: 环境变量 %s 第 %d 个 %q 缺少端口（格式: http(s)://host:port）", envName, i+1, p)
		}
		if u.Path != "" {
			return nil, fmt.Errorf("etcdstore: 环境变量 %s 第 %d 个 %q 不支持 URL 路径（当前 %q）", envName, i+1, p, u.Path)
		}
		if u.RawQuery != "" {
			return nil, fmt.Errorf("etcdstore: 环境变量 %s 第 %d 个 %q 不支持查询参数（当前 %q）", envName, i+1, p, u.RawQuery)
		}
		if u.Fragment != "" {
			return nil, fmt.Errorf("etcdstore: 环境变量 %s 第 %d 个 %q 不支持 fragment", envName, i+1, p)
		}
		if u.User != nil {
			return nil, fmt.Errorf("etcdstore: 环境变量 %s 第 %d 个 %q 不支持 URL 内凭证——外部 etcd 鉴权请在 etcd 侧配置（v0.5.0 不透传）", envName, i+1, p)
		}
		endpoints = append(endpoints, p)
	}
	return endpoints, nil
}

// requirePEMFile 校验 PEM 文件存在且可读（TLS 配置 fail-fast 前置检查）。
func requirePEMFile(envName, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("etcdstore: 环境变量 %s=%q 指定的文件不可读: %v", envName, path, err)
	}
	defer f.Close()
	return nil
}

// parseDurationEnv 解析时长环境变量：优先 Go duration（"1h"、"90s"），
// 回退纯秒数（"90"）；非法或非正值返回错误（对齐 nodecontroller 约定）。
func parseDurationEnv(name, v string) (time.Duration, error) {
	if d, err := time.ParseDuration(v); err == nil {
		if d <= 0 {
			return 0, fmt.Errorf("etcdstore: 环境变量 %s=%q 必须为正时长", name, v)
		}
		return d, nil
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
		return time.Duration(n) * time.Second, nil
	}
	return 0, fmt.Errorf("etcdstore: 环境变量 %s=%q 不是合法时长（支持 Go duration 如 \"1h\" 或秒数）", name, v)
}

// parseListenURL 解析并校验监听地址：只允许 http 协议 + 回环地址
// （127.0.0.1 / localhost / ::1）。安全底线：embed 不暴露到非回环。
func parseListenURL(envName, raw string) (url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return url.URL{}, fmt.Errorf("etcdstore: 环境变量 %s=%q 不是合法 URL: %v", envName, raw, err)
	}
	if u.Scheme != "http" {
		return url.URL{}, fmt.Errorf("etcdstore: 环境变量 %s=%q 必须使用 http 协议（当前 %q）", envName, raw, u.Scheme)
	}
	host := u.Hostname()
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return url.URL{}, fmt.Errorf("etcdstore: 环境变量 %s=%q 只允许绑定回环地址（127.0.0.1/localhost/::1），当前 %q（安全底线：不暴露到非回环）", envName, raw, host)
	}
	return *u, nil
}
