package etcdstore

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/server/v3/embed"

	"edgeflow/pkg/log"
)

const (
	// embedName 是单成员成员名（嵌入唯一实例；多实例在测试中以独立数据目录隔离）。
	embedName = "edgeflow"
	// embedToken 是 initial-cluster-token（同一数据目录的集群身份，改动会使旧库视为外集群）。
	embedToken     = "edgeflow-v0.4.0"
	startupTimeout = 10 * time.Second // ReadyNotify 等待上限（设计 §6 参考值 1~3s，10s 富余）
)

// EmbeddedEtcd 持有嵌入式 etcd 的生命周期与客户端句柄。
type EmbeddedEtcd struct {
	cfg       Config
	e         *embed.Etcd
	client    *clientv3.Client
	clientURL string // 实际客户端地址（http://127.0.0.1:port；端口 0 时取真实端口）

	closeOnce sync.Once
}

// Start 启动单节点嵌入式 etcd：
//   - 自动 MkdirAll 数据目录；
//   - CLIENT_URL / PEER_URL 只允许绑定回环地址（parseListenURL 强制，安全底线）；
//   - 等待 e.Server.ReadyNotify() 就绪（10s 超时），超时/失败返回 error；
//   - 数据目录损坏、端口被占等启动失败一律返回 error（不 panic），
//     由装配区按 Config.Strict 决定"降级纯内存"还是"拒绝启动"（设计 §6.5）。
func Start(cfg Config) (*EmbeddedEtcd, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("etcdstore: 创建数据目录 %s 失败: %w", cfg.DataDir, err)
	}
	lcurl, err := parseListenURL(EnvClientURL, cfg.ClientURL)
	if err != nil {
		return nil, err
	}
	pcurl, err := parseListenURL(EnvPeerURL, cfg.PeerURL)
	if err != nil {
		return nil, err
	}

	ecfg := embed.NewConfig()
	ecfg.Name = embedName
	ecfg.Dir = cfg.DataDir
	ecfg.ListenClientUrls = []url.URL{lcurl}
	ecfg.AdvertiseClientUrls = []url.URL{lcurl}
	ecfg.ListenPeerUrls = []url.URL{pcurl}
	ecfg.AdvertisePeerUrls = []url.URL{pcurl}
	ecfg.InitialCluster = embedName + "=" + pcurl.String()
	ecfg.InitialClusterToken = embedToken
	ecfg.QuotaBackendBytes = cfg.QuotaBackendBytes
	ecfg.AutoCompactionMode = cfg.AutoCompactionMode
	ecfg.AutoCompactionRetention = compactionRetentionString(cfg.AutoCompactionMode, cfg.AutoCompactionRetention)
	ecfg.Logger = "zap"
	// etcd 内部日志压到 warn（业务日志走 edgeflow/pkg/log，避免 stderr 噪声）。
	ecfg.LogLevel = "warn"

	e, err := embed.StartEtcd(ecfg)
	if err != nil {
		return nil, fmt.Errorf("etcdstore: embed 启动失败（dir=%s, strict=%v）: %w", cfg.DataDir, cfg.Strict, err)
	}

	// 就绪等待：ReadyNotify 成功 / errc 报错 / 超时 三路竞争。
	select {
	case <-e.Server.ReadyNotify():
	case err := <-e.Err():
		e.Close()
		return nil, fmt.Errorf("etcdstore: embed 启动失败（dir=%s）: %w", cfg.DataDir, err)
	case <-time.After(startupTimeout):
		e.Close()
		return nil, fmt.Errorf("etcdstore: embed 启动超时（%s 内未就绪，dir=%s）", startupTimeout, cfg.DataDir)
	}

	// 实际客户端地址（配置端口为 0 时取真实监听端口）。
	clientURL := "http://" + e.Clients[0].Addr().String()

	client, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{clientURL},
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		e.Close()
		return nil, fmt.Errorf("etcdstore: 创建 clientv3 客户端失败（%s）: %w", clientURL, err)
	}

	log.Infof("[etcdstore] 嵌入式 etcd 已启动 name=%s dir=%s client=%s peer=%s quota=%d compaction=%s/%s",
		embedName, cfg.DataDir, clientURL, pcurl.String(), cfg.QuotaBackendBytes,
		cfg.AutoCompactionMode, cfg.AutoCompactionRetention)
	return &EmbeddedEtcd{cfg: cfg, e: e, client: client, clientURL: clientURL}, nil
}

// Close 按设计 §6.1 顺序关停：先关 clientv3 连接，再关 embed
// （内部停止 raft、刷 WAL、释放数据目录锁）。幂等——embed 3.5.33 的
// Close 只对 stopc 做了 closeOnce 保护，errc 二次 close 会 panic，
// 因此本层用 sync.Once 整体保护（测试/装配区可安全重复调用）。
//
// 同步写穿方案的关停红利：写路径无异步缓冲区，Close 即数据完整，无需 flush 阶段。
func (et *EmbeddedEtcd) Close() error {
	var err error
	et.closeOnce.Do(func() {
		if et.client != nil {
			err = et.client.Close()
			et.client = nil
		}
		et.e.Close()
		log.Infof("[etcdstore] 嵌入式 etcd 已关闭 dir=%s", et.cfg.DataDir)
	})
	return err
}

// ClientURL 返回实际客户端监听地址（http://127.0.0.1:port）。
func (et *EmbeddedEtcd) ClientURL() string { return et.clientURL }

// Client 返回 clientv3.KV 接口实现（供需要 clientv3 原生语义的场景使用；
// 上层通常直接用 KVStore 封装）。
func (et *EmbeddedEtcd) Client() clientv3.KV { return clientv3.NewKV(et.client) }

// compactionRetentionString 把保留期换算为 embed 需要的字符串：
// periodic 模式 → Go duration（如 "1h0m0s"）；revision 模式 → 整数 revision 数（秒取整）。
func compactionRetentionString(mode string, retention time.Duration) string {
	if mode == "revision" {
		return strconv.FormatInt(int64(retention/time.Second), 10)
	}
	return retention.String()
}
