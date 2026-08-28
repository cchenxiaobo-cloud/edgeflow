// cloudcore 是 EdgeFlow 的云端组件入口。
//
// 对标 KubeEdge 的 CloudCore：未来将在这里承载云边通信
// （WebSocket）、消息路由（NATS）、设备管理（CRD）等云端逻辑。
// 当前版本提供：加载配置、启动 HTTP 服务（/healthz 健康检查）、
// 启动 CloudHub（云边 WebSocket 通道，默认端口 10000）。
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	v1alpha1 "edgeflow/apis/edge/v1alpha1"
	"edgeflow/cloud/pkg/audit"
	"edgeflow/cloud/pkg/auth"
	"edgeflow/cloud/pkg/cloudhub"
	"edgeflow/cloud/pkg/devicestatus"
	"edgeflow/cloud/pkg/etcdstore"
	"edgeflow/cloud/pkg/metrics"
	"edgeflow/cloud/pkg/modelrelease"
	"edgeflow/cloud/pkg/modelrepo"
	"edgeflow/cloud/pkg/nodecontroller"
	"edgeflow/cloud/pkg/podstatus"
	"edgeflow/cloud/pkg/registry"
	"edgeflow/pkg/certs"
	"edgeflow/pkg/config"
	"edgeflow/pkg/httpx"
	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"
	"edgeflow/pkg/resource"
	"edgeflow/pkg/version"
)

// sendQuotaBytes 解析单连接发送缓冲字节配额（v0.22.0，CHN-02）：
// EDGEFLOW_CLOUDHUB_SEND_QUOTA_BYTES 覆盖（字节数）；未设置/非法 → 默认 64MiB；
// 显式 <=0 → 关闭字节计量（行为与 v0.21.0 一致，仅消息数上限）。
func sendQuotaBytes() int64 {
	if v := os.Getenv("EDGEFLOW_CLOUDHUB_SEND_QUOTA_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
		log.Warnf("EDGEFLOW_CLOUDHUB_SEND_QUOTA_BYTES=%q 非法（需十进制字节数），使用默认配额", v)
	}
	return cloudhub.SendQuotaBytesDefault
}

// allowedOriginsFromEnv 解析 WebSocket Origin 白名单（v0.23.0，SEC-05）：
// EDGEFLOW_CLOUDCORE_ALLOWED_ORIGINS 逗号分隔（自动去空白、丢空段）；
// 未设置/空 → nil = 全放行（默认行为不变）。无有效性校验（URL 形态由
// 部署者负责，精确匹配语义下错误配置只会导致对应 Origin 被拒，可观测）。
func allowedOriginsFromEnv() []string {
	v := os.Getenv("EDGEFLOW_CLOUDCORE_ALLOWED_ORIGINS")
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	origins := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			origins = append(origins, t)
		}
	}
	if len(origins) == 0 {
		return nil
	}
	return origins
}

func main() {
	// run 返回进程退出码：非 0 表示启动/运行失败
	if code := run(os.Args[1:], os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

// registerNodeAPIRoutes 注册既有节点/设备/应用管理 API 路由（11 条：
// /api/v1/nodes×2 + /edgenodes×2 + podsync + config-sync + pods×2 +
// devices×2 + device-command）。v0.7.0 自 run() 机械抽出（注册行原样迁
// 移，行为零变化——既有端点测试全量通过 = 回归锚点），目的是让契约测试
// 经 routeRegistrar 计数既有 11 条（D1 口径：14 = 11 apiMux + /healthz
// + /metrics + /ocsp；+17 模型端点 = 31）。
func registerNodeAPIRoutes(mux routeRegistrar, api *nodeAPI) {
	mux.HandleFunc("GET /api/v1/nodes", api.listNodes)
	mux.HandleFunc("GET /api/v1/nodes/{nodeID}", api.getNode)
	mux.HandleFunc("GET /api/v1/edgenodes", api.listEdgeNodes)
	mux.HandleFunc("GET /api/v1/edgenodes/{nodeID}", api.getEdgeNode)
	mux.HandleFunc("POST /api/v1/nodes/{nodeID}/podsync", api.syncPod)
	mux.HandleFunc("POST /api/v1/nodes/{nodeID}/config-sync", api.syncConfig)
	mux.HandleFunc("GET /api/v1/pods", api.listPods)
	mux.HandleFunc("GET /api/v1/nodes/{nodeID}/pods", api.listNodePods)
	mux.HandleFunc("GET /api/v1/devices", api.listDevices)
	mux.HandleFunc("GET /api/v1/nodes/{nodeID}/devices", api.listNodeDevices)
	mux.HandleFunc("POST /api/v1/nodes/{nodeID}/device-command", api.sendDeviceCommand)
}

// run 是 main 的可测试入口：解析命令行 → 加载配置 → 启动服务，
// 返回进程退出码（0 成功，1 失败）。
func run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseFlags(args, stderr)
	if err != nil {
		// 写入 stderr 失败（如管道关闭）时无需额外处理，忽略即可
		_, _ = fmt.Fprintf(stderr, "参数解析失败: %v\n", err)
		return 1
	}

	// --version 打印版本信息后退出
	if opts.version {
		// 写入 stdout 失败时无需额外处理，忽略即可
		_, _ = fmt.Fprintln(stdout, version.Get().String())
		return 0
	}

	// 加载配置：--port > 环境变量 EDGEFLOW_CLOUDCORE_PORT > 配置文件 > 默认值
	// （CloudHub 端口同构：EDGEFLOW_CLOUDCORE_HUB_PORT > 配置文件 hubPort > 默认 10000）
	cfg, err := config.Load(opts.config, opts.port, opts.portSet)
	if err != nil {
		log.Errorf("配置加载失败: %v", err)
		return 1
	}

	// 打印启动信息与生效配置（含端口来源，便于排查）
	info := version.Get()
	log.Infof("cloudcore starting, %s", info.String())
	log.Infof("生效配置: HTTP 端口 %d（来源: %s）, CloudHub 端口 %d（来源: %s）, 通道压缩 %v",
		cfg.Port, cfg.PortSource, cfg.HubPort, cfg.HubPortSource, cfg.Compress)

	// 证书目录（EDGEFLOW_CLOUDCORE_CERT_DIR，默认 data/certs/）：
	// 云边通道 mTLS 与 OCSP responder（/ocsp）共用同一目录，
	// 吊销状态（crl.json）天然同源。
	certDir := os.Getenv("EDGEFLOW_CLOUDCORE_CERT_DIR")
	if certDir == "" {
		certDir = certs.DefaultCertDir
	}

	// 云边通道 mTLS（WBS 7.1 证书管理 + 7.4 云边认证）：
	// EDGEFLOW_CLOUDCORE_TLS=on 时启用 TLS 监听，并要求边缘侧携带
	// 本 CA 签发的客户端证书（双向认证）。
	//   - 首次运行自动生成 CA 与 cloudcore 服务端证书；已存在则加载（幂等）
	//   - 未开启时行为与之前完全一致（纯 ws://，向后兼容）
	var hubTLS *tls.Config
	if os.Getenv("EDGEFLOW_CLOUDCORE_TLS") == "on" {
		if _, err := certs.EnsureCA(certDir); err != nil {
			log.Errorf("CA 初始化失败（certDir=%s）: %v", certDir, err)
			return 1
		}
		// SAN 注入（M4B P1-4）：EDGEFLOW_CLOUDCORE_TLS_SAN 支持
		// "IP:1.2.3.4,DNS:cloudcore.svc" 逗号分隔列表；未设置时
		// 回退默认（127.0.0.1/localhost/cloudcore，仅本机可用）。
		// M4C P2-① 修复：非法条目从"Warn 跳过"改为"报错退出"（fail-fast）——
		// 配错的 SAN 若被静默跳过，证书只覆盖默认 SAN（仅回环），边缘节点
		// mTLS 会全部握手失败且难以定位，必须在启动阶段就暴露配置错误。
		var ips []net.IP
		var dnsNames []string
		if san := os.Getenv("EDGEFLOW_CLOUDCORE_TLS_SAN"); san != "" {
			ips, dnsNames, err = parseSANList(san)
			if err != nil {
				log.Errorf("EDGEFLOW_CLOUDCORE_TLS_SAN 配置无效: %v", err)
				return 1
			}
		}
		if _, err := certs.EnsureServerCertWithSANs(certDir, "cloudcore", ips, dnsNames); err != nil {
			log.Errorf("cloudcore 服务端证书初始化失败: %v", err)
			return 1
		}
		// v0.22.0（SEC-04 吊销链收紧开关，默认全关 = v0.21.0 行为逐字节一致）：
		// EDGEFLOW_CLOUDCORE_CRL_STRICT=on → CRL 缺失时 mTLS 握手 fail-closed；
		// EDGEFLOW_CLOUDCORE_OCSP_FRESH=on → OCSP 查询启用 nextUpdate 新鲜度校验。
		revOpts := revocationOptionsFromEnv()
		if hubTLS, err = certs.LoadTLSConfigWithOptions(certDir, true, revOpts); err != nil {
			log.Errorf("加载 TLS 配置失败: %v", err)
			return 1
		}
		log.Infof("CloudHub TLS 已启用（certDir=%s, mTLS: 强制要求并验证客户端证书, CRL 严格=%v, OCSP 新鲜度=%v）", certDir, revOpts.CRLStrict, revOpts.OCSPFreshCheck)
	}
	hub := cloudhub.New(fmt.Sprintf(":%d", cfg.HubPort), cloudhub.WithTLS(hubTLS),
		// WBS 7.3 设备认证：EDGEFLOW_CLOUDCORE_NODE_TOKEN 非空时启用节点接入
		// 令牌校验（edgecore 注册必须携带相同 token）；未设置保持向后兼容。
		// v0.21.0（SEC-02）：EDGEFLOW_CLOUDHUB_REQUIRE_NODE_TOKEN=on时，
		// 即使服务端未配置令牌也拒绝携带令牌的注册（防伪造令牌抢占探测），
		// 默认关闭（行为与 v0.20.0 逐字节一致）。
		cloudhub.WithNodeToken(os.Getenv("EDGEFLOW_CLOUDCORE_NODE_TOKEN")),
		cloudhub.WithRequireNodeToken(os.Getenv("EDGEFLOW_CLOUDHUB_REQUIRE_NODE_TOKEN") == "on"),
		// WBS 4.4 云边通道 gzip 压缩：配置 compress（缺省 true 默认开启）。
		// 协商式兼容：旧边缘不声明能力 → 云端对其保持明文下发，互操作不受影响。
		cloudhub.WithCompress(cfg.Compress),
		// v0.22.0（CHN-02）：单连接发送缓冲字节配额，默认 64MiB；
		// EDGEFLOW_CLOUDHUB_SEND_QUOTA_BYTES 覆盖（字节数，<=0 关闭计量）。
		cloudhub.WithSendQuotaBytes(sendQuotaBytes()),
		// v0.23.0（SEC-05）：WebSocket Origin 白名单，EDGEFLOW_CLOUDCORE_ALLOWED_ORIGINS
		// 逗号分隔；未设置/空 = 全放行（默认行为不变），非空 = 携带 Origin 的
		// 握手必须精确命中（缺失 Origin 放行，边缘主路径不受影响）。
		cloudhub.WithAllowedOrigins(allowedOriginsFromEnv()))

	// ── v0.4.0 存储装配：嵌入式 etcd 写穿 vs v0.3.x 纯内存 ────────────────
	// 配置 fail-fast（与 nodecontroller.DurationsFromEnv 同约定）：
	//   EDGEFLOW_CLOUDCORE_ETCD_ENABLED（默认 true）→ etcd 路径；
	//   EDGEFLOW_CLOUDCORE_NODE_RETENTION（默认 24h）喂给 registry 的
	//   OfflineTTL（内存 TTL 与 etcd 键生命周期同口径；纯内存模式默认值
	//   与 v0.3.x 的 registry.New() 完全一致，行为不变）。
	etcdCfg, err := etcdstore.ConfigFromEnv()
	if err != nil {
		log.Errorf("etcd 配置无效: %v", err)
		return 1
	}
	retention, err := nodeRetentionFromEnv()
	if err != nil {
		log.Errorf("节点保留时长配置无效: %v", err)
		return 1
	}
	// v0.6.0：NODE_SCAN_INTERVAL / NODE_TIMEOUT 提前到存储装配前解析——
	// 外部模式的扫描周期（NODE_SCAN_INTERVAL）要喂给 lease 注册表（重扫/
	// GC 周期，设计 §12.5）；embed/纯内存用法不变（解析结果继续喂
	// NodeController）。非法值 fail-fast（与 v0.5.0 同约定）。
	scanInterval, nodeTimeout, err := nodecontroller.DurationsFromEnv()
	if err != nil {
		log.Errorf("NodeController/外部模式扫描周期配置无效: %v", err)
		return 1
	}
	// 清理循环/GC 级联 goroutine 的取消信号（serve 内另有同源的优雅关停，
	// 二者并行不冲突）。
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	var (
		nodeReg     registry.Store     // 节点注册表（纯内存或 EtcdRegistry 写穿包装）
		deviceStore devicestatus.Store // 设备状态存储（纯内存或 EtcdDeviceStore 写穿包装）
		// v0.9.0（③）：Pod 状态存储（纯内存或 EtcdPodStore 写穿包装——
		// 写穿后 cloudcore 重启 Pod 列表直接可见，不再短暂清空）。
		podStore  podstatus.Store
		closeEtcd func() // etcd 关停闭包（§6.1：注册表循环 → kv → embed，最后执行）
		// v0.6.0 多副本参数（外部模式解析；其余形态零值/默认，路由区按 externalMode 消费）
		leaseTTL     time.Duration // 判活租约 TTL（EDGEFLOW_CLOUDCORE_NODE_LEASE_TTL，默认 300s）
		multiReplica bool          // 多副本标识（EDGEFLOW_CLOUDCORE_MULTI_REPLICA="1"/"true"）
		// v0.7.0：模型仓库存储装配捕获（embed 成功路径的 kv / 外部模式的
		// ext；纯内存与 embed 降级路径保持 nil → 模型仓库退化为纯内存）
		modelKV etcdstore.KVStore
	)
	// 最先注册 → 最后执行：保证在 ledger.Close()（审计）之后、进程退出前才关
	// etcd（write-through 无待刷缓冲，Close 即数据完整；EmbeddedEtcd.Close 幂等）。
	defer func() {
		if closeEtcd != nil {
			closeEtcd()
		}
	}()

	downgrade := func(reason string) {
		log.Errorf("[etcdstore] %s，降级纯内存模式——数据未持久化，重启将丢失", reason)
		nodeReg = registry.New(registry.WithOfflineTTL(retention))
		deviceStore = devicestatus.NewStore()
		podStore = podstatus.NewStore()
	}

	if !etcdCfg.Enabled {
		// v0.3.x 纯内存路径：不建目录、不占端口、不写盘，行为与旧版完全一致。
		// 总开关优先（设计 §9）：ETCD_ENABLED=false + ENDPOINTS 非空 → 仍走纯内存。
		log.Infof("存储: EDGEFLOW_CLOUDCORE_ETCD_ENABLED=false，纯内存模式（重启后节点/设备数据丢失）")
		warnLeaseTTLIgnored() // M1/M15：纯内存不解析新 env 死路径，显式设置 → Warn 忽略
		nodeReg = registry.New(registry.WithOfflineTTL(retention))
		deviceStore = devicestatus.NewStore()
		podStore = podstatus.NewStore()
	} else if len(etcdCfg.Endpoints) > 0 {
		// ── v0.5.0 外部 etcd 模式（方案④，配置级切换）────────────────────
		// 跳过 embed：不建目录/不占端口/忽略 DATA_DIR，clientv3 直连
		// EDGEFLOW_CLOUDCORE_ETCD_ENDPOINTS 指向的集群。
		// v0.6.0「真多活」：判活改为 etcd 租约视角（心跳键存在性）+ 删除守卫
		// + watch 缓存同步，多副本安全（单写者铁律解除；设计定稿 §0.3/§1）。
		// 故障语义与 embed 不同：连接/TLS 配置失败 = fail-fast 拒绝启动——
		// 外部集群是主动选择的依赖，静默降级违背部署意图。
		tlsCfg, err := etcdCfg.BuildTLS()
		if err != nil {
			log.Errorf("[etcdstore] 外部 etcd TLS 配置无效，拒绝启动: %v", err)
			return 1
		}
		kv, err := etcdstore.NewKVStoreWithAuth(etcdCfg.Endpoints, tlsCfg, etcdCfg.Username, etcdCfg.Password)
		if err != nil {
			log.Errorf("[etcdstore] 外部 etcd 连接失败（endpoints=%v），拒绝启动（fail-fast，不降级）: %v",
				etcdCfg.Endpoints, err)
			return 1
		}
		log.Infof("[etcdstore] 外部 etcd 模式 endpoints=%v tls=%v auth=%v", etcdCfg.Endpoints, tlsCfg != nil, etcdCfg.Username != "")
		// R3 启动探活：clientv3.New 是懒连接，必须显式线性一致 Get 验证集群
		// 可读可写（quorum），失败 = fail-fast（至多 3 次尝试/预算 ≤20s）。
		if err := etcdstore.ProbeAlive(kv); err != nil {
			log.Errorf("[etcdstore] 外部 etcd 启动探活失败（endpoints=%v），拒绝启动: %v",
				etcdCfg.Endpoints, err)
			_ = kv.Close()
			return 1
		}
		// M10：明文护栏由 ConfigFromEnv 裁决（非回环+无 TLS 默认拒绝）；能
		// 走到这里说明全回环明文或已开逃生门——后一种情况每次启动都大告警。
		if etcdCfg.AllowInsecure && !etcdCfg.TLSEnabled() {
			for _, ep := range etcdCfg.Endpoints {
				if !etcdstore.HostIsLoopback(etcdstore.EndpointHost(ep)) {
					log.Warnf("[etcdstore] ⚠️ 明文暴露模式：通过 %s=1 绕过明文护栏连接非回环端点 %q（无 TLS、无鉴权——任何能触达的客户端可读写全部键空间，风险自担）",
						etcdstore.EnvAllowInsecure, ep)
					break
				}
			}
		}
		// 前置条件 #4：显式设置的 embed 字段在外部模式下被忽略 → 明示告警。
		warnEmbedFieldsIgnored()

		// ── v0.6.0 多副本参数（主线裁决 D2/D3，仅外部模式解析）────────────
		// 判活租约 TTL：EDGEFLOW_CLOUDCORE_NODE_LEASE_TTL，默认 300s（独立于
		// NODE_TIMEOUT——D2：3m 是检测延迟不是故障免疫，解耦后互不牺牲）。
		leaseTTL, err = etcdstore.LeaseTTLFromEnv()
		if err != nil {
			log.Errorf("[etcdstore] 判活租约 TTL 配置无效，拒绝启动: %v", err)
			_ = kv.Close()
			return 1
		}
		// 多副本标识：EDGEFLOW_CLOUDCORE_MULTI_REPLICA（"1"/"true" 生效）→
		// /healthz 反映 etcd 连接（失联 >TTL → 503 → K8s liveness 重启自愈）。
		multiReplica, err = multiReplicaFromEnv()
		if err != nil {
			log.Errorf("[etcdstore] 多副本标识配置无效，拒绝启动: %v", err)
			_ = kv.Close()
			return 1
		}

		// ExtendedKV 断言（防御）：kv 恒由 etcdstore 自身实现满足，理论不可达。
		ext, ok := etcdstore.AsExtended(kv)
		if !ok {
			log.Errorf("[etcdstore] 外部模式要求 ExtendedKV（CAS/watch/lease 扩展面），当前 KV 不满足，拒绝启动")
			_ = kv.Close()
			return 1
		}
		modelKV = ext // v0.7.0：模型仓库存储复用同一 ExtendedKV（CAS+watch）
		externalFailFast := func(reason string) {
			log.Errorf("[etcdstore] 外部模式 %s，拒绝启动（不降级——外部 etcd 是显式部署依赖）", reason)
		}
		// v0.9.0（③）：Pod 状态写穿持久化（外部模式，锚定加载 + watch 同步）。
		// 失败语义：探活已通过、kv 可用，本步失败仅告警降级内存（Pod 状态
		// 是瞬态上报，重启后仍可自愈——写穿是增强非硬依赖）。
		ps, perr := podstatus.NewEtcdPodStore(ext)
		if perr != nil {
			log.Warnf("[podstatus] 外部模式写穿装配失败，降级纯内存: %v", perr)
			podStore = podstatus.NewStore()
		} else {
			if _, lerr := ps.LoadAnchored(sigCtx); lerr != nil {
				log.Warnf("[podstatus] 外部模式锚定加载失败（以空库继续，watch/重扫会收敛）: %v", lerr)
			}
			ps.StartWatch(sigCtx)
			podStore = ps
		}
		nodeReg, deviceStore, closeEtcd, err = assembleExternalEtcdStores(
			ext, func() {
				if err := kv.Close(); err != nil {
					log.Errorf("[etcdstore] 外部 etcd 关停失败: %v", err)
				}
			}, "外部 etcd", retention, leaseTTL, scanInterval, sigCtx)
		if err != nil {
			externalFailFast(fmt.Sprintf("装配失败: %v", err))
			return 1
		}
		log.Infof("[etcdstore] 外部模式判活 = etcd 租约机制（心跳键 /edgeflow/registry/heartbeats/ 存在性，TTL=%v，grant-per-heartbeat）；NodeController 内存扫描停用；NODE_TIMEOUT=%v 不再参与判活（D2 解耦）；NODE_SCAN_INTERVAL=%v 语义迁移为 etcd 重扫/GC 周期",
			leaseTTL, nodeTimeout, scanInterval)
	} else {
		// etcd 路径：safeStart 兜住坏 WAL 的 panic（基础层实测：坏库在 raft
		// 恢复阶段是 panic 不是 error，必须 recover 才能按 Strict 决策）。
		log.Infof("[etcdstore] 嵌入式 etcd 模式 dataDir=%s", etcdCfg.DataDir)
		warnLeaseTTLIgnored() // M2/M15：embed 模式下 LEASE_TTL 显式设置 → Warn 忽略
		et, kv, err := safeStartEmbeddedKV(etcdCfg)
		if err != nil {
			if etcdCfg.Strict {
				log.Errorf("[etcdstore] 嵌入式 etcd 启动失败且 STRICT=1，拒绝启动: %v", err)
				return 1
			}
			downgrade(fmt.Sprintf("嵌入式 etcd 启动失败（dataDir=%s）", etcdCfg.DataDir))
		} else {
			nodeReg, deviceStore, closeEtcd, _ = assembleEtcdStores(
				kv, func() {
					if err := et.Close(); err != nil {
						log.Errorf("[etcdstore] 嵌入式 etcd 关停失败: %v", err)
					}
				}, "嵌入式 etcd", retention, sigCtx, downgrade)
			// v0.9.0（③）：Pod 状态写穿持久化（embed 模式，全量加载）。
			ps, perr := podstatus.NewEtcdPodStore(kv)
			if perr != nil {
				log.Warnf("[podstatus] 写穿装配失败，降级纯内存: %v", perr)
				podStore = podstatus.NewStore()
			} else {
				if lerr := ps.Load(sigCtx); lerr != nil {
					log.Warnf("[podstatus] 全量加载失败（以空库继续，上报会收敛）: %v", lerr)
				}
				podStore = ps
			}
			modelKV = kv // v0.7.0：模型仓库存储复用 embed kvStore（CAS 恒成功，D4 口径）
		}
	}

	// 节点注册表与 CloudHub 事件桥接（依赖注入，CloudHub 不感知实现）：
	// 节点注册/心跳/断开时实时维护节点元数据，供查询 API 使用。
	hub.SetNodeEvents(registry.NewCloudHubAdapter(nodeReg))

	// ── v0.7.0 模型仓库与灰度发布装配（WBS-8）───────────────────────────
	// 新 env（设计 §9.3）：发布控制器扫描周期 EDGEFLOW_CLOUDCORE_RELEASE_
	// SCAN_INTERVAL（默认 5s，>0）/ 领跑锁 TTL EDGEFLOW_CLOUDCORE_RELEASE_
	// LOCK_TTL（默认 60s，>=15s 下限，D5：TTL ≥ 3×refresh）。非法值
	// fail-fast（对齐 duration env 风格）；embed/纯内存对 LOCK_TTL 显式
	// 设置 → Warn 忽略（并入 warnEmbedFieldsIgnored 同族）。
	releaseScan, err := releaseScanIntervalFromEnv()
	if err != nil {
		log.Errorf("[modelrelease] %v", err)
		return 1
	}
	releaseLockTTL, err := releaseLockTTLFromEnv()
	if err != nil {
		log.Errorf("[modelrelease] %v", err)
		return 1
	}
	releaseBatchParallel, err := releaseBatchParallelFromEnv()
	if err != nil {
		log.Errorf("[modelrelease] %v", err)
		return 1
	}
	releaseGCEnabled, releaseGCKeep, err := releaseGCFromEnv()
	if err != nil {
		log.Errorf("[modelrelease] %v", err)
		return 1
	}
	// v0.9.0（R-1）：发布前镜像探活配置（默认 off；warn/fail 见 env 注释）
	mirrorCheckCfg, err := mirrorCheckFromEnv()
	if err != nil {
		log.Errorf("[modelrelease] %v", err)
		return 1
	}
	var (
		modelStore modelrepo.ModelStore     // 模型仓库存储（三模式）
		relCtrl    *modelrelease.Controller // 灰度发布控制器
		closeModel func()                   // 模型存储关停（etcd 停 watch；内存空操作）
	)
	externalEtcd := etcdCfg.Enabled && len(etcdCfg.Endpoints) > 0
	if modelKV == nil {
		// 纯内存（ETCD_ENABLED=false，总开关优先）/ embed 启动失败降级路径：
		// MemoryModelStore + NoopLockKV（单实例天然领跑者，锁逻辑空转无害，
		// 设计 §3.4/§5.4）；重启后模型/版本/发布/部署影子丢失为明示行为（L22）。
		warnReleaseLockTTLIgnored()
		modelStore = modelrepo.NewMemoryModelStore(modelrepo.WithReleaseGC(releaseGCEnabled, releaseGCKeep))
		deploy, derr := modelrelease.NewDeployer(modelStore, hub.ReliableSendContext)
		if derr != nil {
			log.Errorf("[modelrelease] 部署执行器装配失败: %v", derr)
			return 1
		}
		relCtrl, err = modelrelease.NewController(modelStore, deploy, &modelrelease.NoopLockKV{}, modelrelease.Options{
			ScanInterval:  releaseScan,
			LockTTL:       releaseLockTTL,
			GCEnabled:     releaseGCEnabled,
			GCKeep:        releaseGCKeep,
			BatchParallel: releaseBatchParallel,
			DigestLookup:  podstatus.NodeDigestOf(podStore),
		})
		if err != nil {
			log.Errorf("[modelrelease] 发布控制器装配失败: %v", err)
			return 1
		}
		closeModel = func() {}
		log.Infof("[modelrepo] 模型仓库装配完成（纯内存）：重启后模型/版本/发布/部署影子丢失（KNOWN-ISSUES L22 明示行为）")
	} else {
		// embed / 外部 etcd：EtcdModelStore（写穿 + CAS + guard 自愈 D3）
		// + 部署执行器 + 领跑锁（外部=租约锁、embed=Noop 恒成功）+
		// 加载（embed Load / 外部 LoadAnchored+StartWatch）
		if !externalEtcd {
			warnReleaseLockTTLIgnored()
		}
		modeLabel := "嵌入式 etcd"
		if externalEtcd {
			modeLabel = "外部 etcd"
		}
		modelStore, relCtrl, closeModel, err = assembleModelStores(
			modelKV, hub.ReliableSendContext, externalEtcd, modeLabel, sigCtx, releaseScan, releaseLockTTL,
			releaseGCEnabled, releaseGCKeep, releaseBatchParallel, podStore)
		if err != nil {
			if externalEtcd {
				log.Errorf("[modelrelease] 外部模式模型仓库装配失败，拒绝启动（不降级——外部 etcd 是显式部署依赖）: %v", err)
				return 1
			}
			// embed：与注册表装配同口径降级（理论不可达——embed kvStore 恒
			// 满足 AtomicKV；防御性兜底，模型功能退化但进程继续）
			log.Errorf("[modelrelease] 嵌入式模型仓库装配失败，降级纯内存: %v", err)
			modelStore = modelrepo.NewMemoryModelStore(modelrepo.WithReleaseGC(releaseGCEnabled, releaseGCKeep))
		}
	}
	if relCtrl != nil {
		// 控制器 goroutine：sigCtx 取消即退出（优雅关停随主流程）
		go relCtrl.Run(sigCtx)
	}
	// 模型存储关停追加进 closeEtcd 链（最早注册 → 最晚执行语义：在注册表
	// 循环关停后停 watch，不关底层 kv——kv 生命周期归 closeEtcd）
	if closeModel != nil && closeEtcd != nil {
		prevCloseEtcd := closeEtcd
		closeEtcd = func() {
			closeModel()
			prevCloseEtcd()
		}
	}

	// 节点心跳静默超时管理（WBS 2.4）：定时扫描注册表，把心跳停滞
	// （LastHeartbeatAt 超过 timeout 未更新）的节点标记 Offline。
	// 与 CloudHub 断开事件互补：断开事件覆盖"连接断了"的常规场景，
	// 本控制器兜底"连接活着但心跳停滞 / 断开事件丢失"的场景
	// （对标 KubeEdge NodeController）。
	// 周期/阈值环境变量覆盖：EDGEFLOW_CLOUDCORE_NODE_SCAN_INTERVAL /
	// EDGEFLOW_CLOUDCORE_NODE_TIMEOUT（默认 30s / 180s，支持 "15s" 或秒数）。
	// v0.6.0（设计定稿 §1.3b）：外部模式停用 NodeController 内存扫描——
	// 判活 = etcd 租约视角（心跳键存在性，grant-per-heartbeat）；内存扫描的
	// LastHeartbeatAt 判定是多副本误判离线的病灶，被 watch + 重扫替代。
	externalMode := etcdCfg.Enabled && len(etcdCfg.Endpoints) > 0
	nc := nodecontroller.New(nodeReg,
		nodecontroller.WithInterval(scanInterval),
		nodecontroller.WithTimeout(nodeTimeout),
	)
	if externalMode {
		log.Infof("NodeController 停用（外部模式判活 = etcd 租约机制；NODE_TIMEOUT=%v 不再参与判活，D2 解耦）", nodeTimeout)
	} else {
		nc.Start()
		log.Infof("NodeController 装配完成: 扫描周期 %v, 心跳超时 %v", scanInterval, nodeTimeout)
	}

	// Pod 状态存储与 CloudHub 上报回调桥接（依赖注入，CloudHub 不感知实现）：
	// 边侧上报的 PodStatus 消息经 CloudHub 校验后注入存储，供查询 API 使用。
	// v0.9.0（③）起 write-through：纯内存/降级路径在上方各分支已赋值；
	// 此处兜底防漏（任何分支未赋值时保持内存语义）。
	if podStore == nil {
		podStore = podstatus.NewStore()
	}
	hub.SetPodStatusHandler(func(nodeID string, ps cloudhub.PodStatusPayload) {
		// 回调运行在 CloudHub 读循环 goroutine 内：recover 兜底，
		// 防止单条异常数据导致整个连接处理崩溃（M2B 审查 P2-4）。
		defer func() {
			if r := recover(); r != nil {
				log.Errorf("PodStatus handler panic（nodeID=%s）: %v", nodeID, r)
			}
		}()
		// v0.23.0（SEC-06）：PodName/Phase 等边缘可控字段先消毒再进日志
		// （store 内字段不消毒，保留原始值供 API 消费方自行裁决展示层转义）。
		safePhase, safePod := sanitizeEdgeField(ps.Phase), sanitizeEdgeField(ps.PodName)
		// phase 白名单校验（M2B 审查 P2-3）：未知阶段视为脏数据，丢弃并告警
		switch ps.Phase {
		case "Running", "Stopped", "Absent", "Error", "Unknown":
		default:
			log.Warnf("收到未知 PodStatus phase %q（node=%s pod=%s），丢弃", safePhase, nodeID, safePod)
			return
		}
		// Absent = 边缘确认 Pod 已从期望集合删除：清除云端记录，
		// 列表不再显示已删除 Pod（与 K8s 语义一致；P1 修复配套）。
		if ps.Phase == "Absent" {
			podStore.Delete(nodeID, ps.Namespace, ps.PodName)
			return
		}
		podStore.Upsert(nodeID, podstatus.PodStatus{
			NodeID:          ps.NodeID,
			PodName:         ps.PodName,
			Namespace:       ps.Namespace,
			Phase:           ps.Phase,
			Message:         ps.Message,
			LastReconcileAt: ps.LastReconcileAt,
			ImageDigest:     ps.ImageDigest,
		})
	})

	// 设备状态存储与 CloudHub 上报回调桥接（WBS 5.3）：
	// 边侧上报的 DeviceReport 消息经 CloudHub 校验后注入存储（内存或 etcd
	// 写穿实现已在上方装配），供查询 API 使用。依赖注入（SetDeviceReportHandler），
	// CloudHub 不感知存储实现。
	hub.SetDeviceReportHandler(func(nodeID string, dr cloudhub.DeviceReportPayload) {
		// 回调运行在 CloudHub 读循环 goroutine 内：recover 兜底，
		// 防止单条异常数据导致整个连接处理崩溃（与 PodStatus 回调同约定）。
		defer func() {
			if r := recover(); r != nil {
				log.Errorf("DeviceReport handler panic（nodeID=%s）: %v", nodeID, r)
			}
		}()
		deviceStore.Upsert(nodeID, devicestatus.DeviceStatus{
			NodeID:         nodeID,
			DeviceName:     dr.DeviceName,
			Namespace:      dr.Namespace,
			Properties:     dr.Properties,
			LastReportedAt: dr.ReportedAt,
		})
	})

	// 审计台账（WBS 7.5）：JSONL 追加写，记录每次管理 API 调用。
	// 路径可用环境变量 EDGEFLOW_CLOUDCORE_AUDIT_PATH 覆盖（默认 data/audit-ledger.jsonl）；
	// 初始化失败直接拒绝启动（审计是安全控制，静默降级会掩盖问题）。
	auditPath := os.Getenv(audit.EnvPath)
	if auditPath == "" {
		auditPath = audit.DefaultPath
	}
	ledger, err := audit.NewLedger(auditPath)
	if err != nil {
		log.Errorf("审计台账初始化失败: %v", err)
		return 1
	}
	defer func() { _ = ledger.Close() }()
	log.Infof("审计台账就绪（%s，JSONL 追加写）", auditPath)

	// API Token 认证（WBS 7.2）：默认关闭（向后兼容），
	// EDGEFLOW_CLOUDCORE_AUTH=on 时启用，令牌来自 EDGEFLOW_CLOUDCORE_API_TOKEN。
	// 开启但未配置令牌 → 拒绝启动（fail-fast，与 TLS SAN 校验同约定）。
	authEnabled := auth.EnabledFromEnv()
	var apiToken string
	if authEnabled {
		apiToken = auth.TokenFromEnv()
		if apiToken == "" {
			log.Errorf("%s=on 但 %s 未设置，拒绝启动（认证开启必须配置令牌）", auth.EnvAuth, auth.EnvToken)
			return 1
		}
		log.Infof("API 认证已启用：/api/v1/* 需携带 Authorization: Bearer <token>")
	} else {
		log.Infof("API 认证未启用（默认关闭，向后兼容；%s=on 开启）", auth.EnvAuth)
	}

	// ── v0.21.0 安全默认值告警（SEC-01/SEC-02/CHN-06）：只提升可见性，不改行为 ──
	// 三条告警全部不阻断启动、不改变任何默认行为：
	//   ① 管理 API 认证关闭（SEC-01）：auth.WarnEnabledFromEnv 可显式关闭；
	//   ② 云边接入未配令牌（SEC-02）：nodeToken 为空即向任意主机开放注册
	//     （EDGEFLOW_CLOUDHUB_REQUIRE_NODE_TOKEN=on 时上方 WithRequireNodeToken
	//      已开启携带令牌注册拒绝，此处提示生效中）；
	//   ③ 裸奔组合（CHN-06）：无 mTLS + 无 nodeToken 同时成立 → 聚合大告警。
	if !authEnabled && auth.WarnEnabledFromEnv() {
		log.Warnf("[安全默认值] 管理 API 认证未启用：所有 /api/v1/* 端点（节点台账/设备指令/模型发布）对可达者无差别开放；"+
			"生产环境建议设置 %s=on 且配置 %s=<token>；本告警可用 %s=off 关闭",
			auth.EnvAuth, auth.EnvToken, auth.EnvAuthWarn)
	}
	if os.Getenv("EDGEFLOW_CLOUDCORE_NODE_TOKEN") == "" {
		if os.Getenv("EDGEFLOW_CLOUDHUB_REQUIRE_NODE_TOKEN") == "on" {
			log.Infof("[安全默认值] 云边接入强制校验已开启（%s=on）：携带令牌的注册将被拒绝（服务端未配置令牌）", "EDGEFLOW_CLOUDHUB_REQUIRE_NODE_TOKEN")
		} else {
			log.Warnf("[安全默认值] 云边接入未配置节点令牌（%s 为空）：任意可达 %s:%d 的主机均可注册冒充节点并抢占同 ID 真节点；"+
				"生产环境建议设置 EDGEFLOW_CLOUDCORE_NODE_TOKEN=<token>（与 edgecore 同值），或开启 mTLS（%s=on）",
				"EDGEFLOW_CLOUDCORE_NODE_TOKEN", "<hub-addr>", cfg.HubPort, "EDGEFLOW_CLOUDCORE_TLS")
		}
	}
	if hubTLS == nil && os.Getenv("EDGEFLOW_CLOUDCORE_NODE_TOKEN") == "" {
		log.Warnf("[安全默认值] 裸奔组合：云边通道无 mTLS 且未配置节点令牌——注册面与下发面均明文且无认证，仅限可信隔离网络使用；" +
			"任一维度收紧即可消除本告警（mTLS：EDGEFLOW_CLOUDCORE_TLS=on；令牌：EDGEFLOW_CLOUDCORE_NODE_TOKEN）")
	}

	// 注册路由：/healthz 健康检查 + 节点查询 API（两个视角并存）：
	//   /api/v1/nodes      → NodeInfo 视图（运行视角：CPU/内存/IP 等）
	//   /api/v1/edgenodes  → EdgeNode 视图（CRD 对象视角，对标 K8s Node）
	mux := http.NewServeMux()
	// /healthz：进程存活语义（默认）或 etcd 连接健康（外部多副本模式，D3）——
	// External() && MULTI_REPLICA → 失联 >TTL 返回 503（K8s liveness 重启自愈）；
	// 其余形态（embed / 单副本外部）保持 v0.5.0 语义（恒 200 进程存活检查）。
	var healthzHandler http.HandlerFunc = httpx.Healthz()
	if externalMode && multiReplica {
		if checker, ok := nodeReg.(etcdHealthChecker); ok {
			healthzHandler = etcdAwareHealthz(checker, leaseTTL)
			log.Infof("/healthz 已绑定 etcd 连接健康（多副本模式，失联 >%v → 503 触发 K8s liveness 重启自愈）", leaseTTL)
		} else {
			log.Warnf("/healthz 多副本绑定跳过：nodeReg 未实现 etcdHealthChecker（不应发生，外部模式装配异常）")
		}
	}
	mux.HandleFunc("/healthz", healthzHandler)
	// OCSP 在线吊销（WBS 7.1）：标准 OCSP responder 端点。
	// 数据源与 mTLS 同一证书目录，状态直接读 crl.json（与 CRL 同源）；
	// 不挂 API Token 认证（协议端点，响应自带 CA 签名，见 ocsp.go 说明）。
	mux.HandleFunc("POST /ocsp", (&ocspHandler{certDir: certDir}).ServeHTTP)
	api := &nodeAPI{
		reg:          nodeReg,
		pods:         podStore,
		devices:      deviceStore,
		reliableSend: hub.ReliableSendContext,
	}
	// 管理 API 全部注册在独立 apiMux 上，统一挂认证 + 审计中间件链
	// （WBS 7.5/7.2）：
	//   - 审计在外层：请求 context 挂身份槽，未认证请求（401）同样留痕，
	//     安全审计不丢未授权访问尝试；
	//   - 认证在内层：校验通过后把身份写入身份槽（audit.SetIdentity），
	//     审计落盘 operator=token。
	apiMux := http.NewServeMux()
	// 既有 11 条节点/设备/应用管理路由（v0.1-v0.6.0 形态；v0.7.0 机械抽出
	// 为 registerNodeAPIRoutes，行为零变化——契约测试经 routeRegistrar
	// 计数既有 11 条，与 17 条模型路由合计 28 个 /api/v1/* 端点）
	registerNodeAPIRoutes(apiMux, api)
	// v0.7.0 模型仓库 API（17 条，设计定稿 §4.1 表逐一落地：模型 5 +
	// 版本 6 + 发布 5 + 部署影子 1；注册在既有 apiMux → auth/audit 链
	// 自动覆盖，零新中间件代码）
	(&modelAPI{store: modelStore, reg: nodeReg, mirrorCheck: mirrorCheckCfg, digestOf: podstatus.NodeDigestOf(podStore)}).Register(apiMux)

	var apiHandler http.Handler = apiMux
	if authEnabled {
		apiHandler = auth.Middleware(apiToken)(apiHandler)
	}
	apiHandler = ledger.Middleware(apiHandler)
	mux.Handle("/api/v1/", apiHandler)

	// 可观测性（WBS 10.1）：/metrics 端点输出 Prometheus 文本格式指标。
	// gauge 取值函数由装配层注入（依赖倒置，metrics 包不感知各存储实现）。
	// v0.8.0（L12）：续约失败计数仅外部模式（LeaseEtcdRegistry）注入，
	// 其余形态 Provider 为 nil → 指标行不输出（保持 5 项基线输出不变）。
	var leaseRenewalFailures func() uint64
	var leaseHBRebuilds func() uint64
	// CHN-19（v0.23.0）：续约队列水位与丢弃计数，仅外部模式注入
	var leaseRenewQueueDepth func() int
	var leaseRenewQueueDropped func() uint64
	if lr, ok := nodeReg.(*registry.LeaseEtcdRegistry); ok {
		leaseRenewalFailures = lr.RenewalFailures
		leaseHBRebuilds = lr.HBRebuildsCount
		leaseRenewQueueDepth = lr.RenewQueueDepth
		leaseRenewQueueDropped = lr.RenewQueueDropped
	}
	m := metrics.New(metrics.Providers{
		Nodes:                  nodeReg.Count,
		Pods:                   podStore.Count,
		Devices:                deviceStore.Count,
		ActiveConnections:      hub.ConnCount,
		LeaseRenewalFailures:   leaseRenewalFailures,
		LeaseHBRebuilds:        leaseHBRebuilds,
		LeaseRenewQueueDepth:   leaseRenewQueueDepth,
		LeaseRenewQueueDropped: leaseRenewQueueDropped,
		HubSendBufferBytes:     func() int64 { return hub.BroadcastBytesInView() },
	})
	mux.HandleFunc("GET /metrics", m.Handler())

	addr := fmt.Sprintf(":%d", cfg.Port)
	// 显式创建监听（而非 ListenAndServe）：热重载（WBS 2.7）需要持有
	// listener 才能热切换端口（见 httpReloader.swapPort）。绑定失败在
	// 启动阶段 fail-fast（避免静默跑错端口）。
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Errorf("HTTP 监听 %s 失败: %v", addr, err)
		return 1
	}
	srv := newHTTPServer(addr, m.Middleware(mux)) // 最外层：统计全部 HTTP 请求（含 /healthz、/metrics）

	// 配置热重载（WBS 2.7）：SIGHUP 强制重载 + 每 60s 检查文件 mtime 自动重载。
	// 热生效范围与策略见 applyConfigReload：HTTP 端口热切换监听；
	// CloudHub 端口变更需重启（记录警告并保持旧值）。
	// 重载失败（JSON 错误/端口绑定失败）保持旧配置继续运行（fail-safe）。
	hr := &httpReloader{srv: srv, ln: ln}
	rel := config.NewReloader(opts.config, cfg,
		func() (*config.Config, error) { return config.LoadReload(opts.config, opts.port, opts.portSet) },
		func(old, next *config.Config) error { return applyConfigReload(old, next, hr) })
	rel.Start(config.DefaultWatchInterval)
	defer rel.Stop()
	stopHUP := config.WatchSIGHUP(func() error { return rel.Reload() })
	defer stopHUP()
	log.Infof("配置热重载已启用: 修改 %s 后发送 SIGHUP 立即生效，或等待 %v 自动检查",
		opts.config, config.DefaultWatchInterval)

	return serve(srv, ln, hub, nc)
}

// newHTTPServer 构造 cloudcore 的 HTTP 服务（超时配置集中于此，便于
// 单测与审查，见 TestNewHTTPServerTimeouts）。
//
// 超时配置（M1B P2-5）：
//   - ReadHeaderTimeout 5s：防止慢速建立连接长时间占用连接；
//   - ReadTimeout 10s：防止慢速读取长时间占用连接；
//   - WriteTimeout 15s：防止慢客户端（读响应缓慢/停滞）无限占用写路径——
//     本服务全部响应均为短 JSON（API/healthz/metrics），15s 远超正常编码
//     耗时；CloudHub 的 WebSocket 长连接是独立 Server，不受此超时影响。
func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
	}
}

// sanitizeEdgeField 剥离边缘上报字段中的 CR/LF 与控制字符（含 DEL），
// 供日志打印前消毒（v0.23.0 SEC-06）：被入侵/恶意边缘节点可在
// PodName/Phase/DeviceName 等字段携带 \r\n 伪造日志行、污染安全审计线索。
// 策略：控制字符（<0x20 含 \t 与 CRLF，及 0x7f DEL）逐一剥离；可打印
// 字符（含多字节 UTF-8）原样保留——日志可读性优先，不转义。
// 取值来源为边缘上报，长度已由 CloudHub 报文配额约束，此处不做截断。
func sanitizeEdgeField(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// logEncodeError 记录响应编码失败（M1B P2-6）：典型场景是客户端已断开
// （响应已不可送达，重试无意义），因此只记 Warn 日志便于排查连接异常，
// 不影响正常请求流程（状态码已在编码前写入，无法也无须改写）。
func logEncodeError(handler string, err error) {
	log.Warnf("%s 响应编码失败（客户端可能已断开）: %v", handler, err)
}

// options 是命令行参数解析结果。
type options struct {
	// port 是 --port 的值（仅当 portSet 为 true 时生效）。
	port int
	// portSet 表示 --port 是否被显式指定。
	portSet bool
	// config 是 --config 指定的配置文件路径。
	config string
	// version 表示是否只打印版本信息后退出。
	version bool
}

// parseFlags 解析命令行参数（使用独立的 FlagSet，便于单元测试）。
func parseFlags(args []string, out io.Writer) (*options, error) {
	fs := flag.NewFlagSet("cloudcore", flag.ContinueOnError)
	fs.SetOutput(out)
	opts := &options{}
	fs.IntVar(&opts.port, "port", 0, "HTTP 服务监听端口（优先级高于环境变量与配置文件；不指定时回落到环境变量/配置文件/默认 8080）")
	fs.StringVar(&opts.config, "config", config.DefaultPath, "配置文件路径（JSON 格式，默认 config/cloudcore.json）")
	fs.BoolVar(&opts.version, "version", false, "打印版本信息后退出")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	// 记录 --port 是否被显式设置（flag.Visit 只遍历被设置的 flag）
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "port" {
			opts.portSet = true
		}
	})
	return opts, nil
}

// parseSANList 解析 EDGEFLOW_CLOUDCORE_TLS_SAN 逗号分隔列表
// （如 "IP:1.2.3.4,DNS:cloudcore.svc"），返回 IP 与 DNS 名列表。
// 语法校验（M4C P2-①）：无法识别的前缀、IP 解析失败、空 DNS 名、
// 空条目（多余逗号）一律返回错误——配置错误必须在启动阶段暴露，
// 而不是静默跳过导致证书 SAN 不完整。
func parseSANList(san string) ([]net.IP, []string, error) {
	var ips []net.IP
	var dnsNames []string
	for _, item := range strings.Split(san, ",") {
		item = strings.TrimSpace(item)
		switch {
		case item == "":
			return nil, nil, fmt.Errorf("含空条目（检查是否有多余逗号）")
		case strings.HasPrefix(item, "IP:"):
			ip := net.ParseIP(strings.TrimPrefix(item, "IP:"))
			if ip == nil {
				return nil, nil, fmt.Errorf("条目 %q 不是合法 IP（格式: IP:1.2.3.4）", item)
			}
			ips = append(ips, ip)
		case strings.HasPrefix(item, "DNS:"):
			name := strings.TrimPrefix(item, "DNS:")
			if name == "" {
				return nil, nil, fmt.Errorf("条目 %q 的 DNS 名为空（格式: DNS:host.example.com）", item)
			}
			dnsNames = append(dnsNames, name)
		default:
			return nil, nil, fmt.Errorf("条目 %q 无法识别（仅支持 IP: 与 DNS: 前缀）", item)
		}
	}
	return ips, dnsNames, nil
}

// serve 启动 HTTP 服务与 CloudHub，等待退出信号后一并优雅关闭，返回进程退出码。
// maxWriteBodyBytes 是 Cloud API 写操作（podsync/config-sync/device-command）
// 请求体的大小上限（1 MiB，与云边通道单条消息上限 maxMessageBytes 对齐，
// M1C P2-5）。超限请求立即 413 拒绝，防止超大请求体拖垮解码与后续下发。
const maxWriteBodyBytes = 1 << 20

// decodeWriteBody 解码写操作请求体：先套 http.MaxBytesReader 施加大小限制
// （超限时解码返回 *http.MaxBytesError），再按需返回错误。返回的错误由
// 调用方映射：*http.MaxBytesError → 413（请求体过大），其余 → 400（非法 JSON）。
func decodeWriteBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxWriteBodyBytes)
	return json.NewDecoder(r.Body).Decode(dst)
}

// podSyncRequest 是云端下发 Pod 配置的请求体（M2 应用管理雏形）。
type podSyncRequest struct {
	Operation string  `json:"operation"` // add / update / delete
	Pod       podSpec `json:"pod"`
}

// podSpec 是 Pod 的最小规格描述（后续 M2 扩展为完整 PodSpec）。
type podSpec struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Image     string `json:"image"`
	Replicas  int    `json:"replicas"`
	// Resources 是资源诉求（WBS 6.5 资源调度，新增可选字段，向后兼容：
	// 旧客户端不传时为零值 = 不限制）。格式沿用 K8s 习惯：
	// cpu "250m"、memory "64Mi"。
	Resources podResources `json:"resources,omitempty"`
}

// podResources 是 pod 资源诉求描述（与边缘 metamanager.ResourceRequirements
// 字段一一对应，云边契约只增不改；零值 = 不限制）。
type podResources struct {
	CPURequest    string `json:"cpuRequest,omitempty"`
	CPULimit      string `json:"cpuLimit,omitempty"`
	MemoryRequest string `json:"memoryRequest,omitempty"`
	MemoryLimit   string `json:"memoryLimit,omitempty"`
}

// validateResources 云端前置校验资源字段（fail-fast，省一轮可靠投递往返）：
//   - request ≤ limit（CPU/内存分别校验）；
//   - 资源量格式合法（解析失败视为非法请求）。
//
// 返回错误时调用方应回 400。
func (r podResources) validateResources() error {
	// CPU request ≤ limit
	if r.CPURequest != "" && r.CPULimit != "" {
		req, err := resource.ParseCPU(r.CPURequest)
		if err != nil {
			return fmt.Errorf("resources.cpuRequest 无效: %w", err)
		}
		lim, err := resource.ParseCPU(r.CPULimit)
		if err != nil {
			return fmt.Errorf("resources.cpuLimit 无效: %w", err)
		}
		if req > lim {
			return fmt.Errorf("CPU request (%s) 不能超过 CPU limit (%s)", r.CPURequest, r.CPULimit)
		}
	} else {
		// 只传单边时也校验格式（非法格式在边缘也会失败，云端早失败体验更好）
		if _, err := resource.ParseCPU(r.CPURequest); err != nil {
			return fmt.Errorf("resources.cpuRequest 无效: %w", err)
		}
		if _, err := resource.ParseCPU(r.CPULimit); err != nil {
			return fmt.Errorf("resources.cpuLimit 无效: %w", err)
		}
	}
	// Memory request ≤ limit
	if r.MemoryRequest != "" && r.MemoryLimit != "" {
		req, err := resource.ParseMemory(r.MemoryRequest)
		if err != nil {
			return fmt.Errorf("resources.memoryRequest 无效: %w", err)
		}
		lim, err := resource.ParseMemory(r.MemoryLimit)
		if err != nil {
			return fmt.Errorf("resources.memoryLimit 无效: %w", err)
		}
		if req > lim {
			return fmt.Errorf("Memory request (%s) 不能超过 Memory limit (%s)", r.MemoryRequest, r.MemoryLimit)
		}
	} else {
		if _, err := resource.ParseMemory(r.MemoryRequest); err != nil {
			return fmt.Errorf("resources.memoryRequest 无效: %w", err)
		}
		if _, err := resource.ParseMemory(r.MemoryLimit); err != nil {
			return fmt.Errorf("resources.memoryLimit 无效: %w", err)
		}
	}
	return nil
}

// syncPod 通过可靠投递向指定边缘节点下发 PodSync 消息（WBS 4.6 端到端入口）。
// 响应语义：200=边缘已确认（Ack ok）；400=请求非法（JSON 解析失败/缺字段/
// operation 不在 {add,update,delete} 内）；404=节点未注册/离线；
// 502=边缘回 error Ack（消息已送达但被拒绝）；504=确认超时重试耗尽。
func (api *nodeAPI) syncPod(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("nodeID")

	var req podSyncRequest
	if err := decodeWriteBody(w, r, &req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			http.Error(w, `{"error":"request body too large (limit 1MiB)"}`, http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, `{"error":"invalid json body"}`, http.StatusBadRequest)
		return
	}
	if req.Operation == "" || req.Pod.Name == "" {
		http.Error(w, `{"error":"operation and pod.name are required"}`, http.StatusBadRequest)
		return
	}
	// operation 白名单校验（M1C P2-3）：云端前置校验，非法值直接 400，
	// 避免下发后等一轮可靠投递往返（最长 ~15s）才以 502 暴露。
	switch req.Operation {
	case "add", "update", "delete":
	default:
		http.Error(w, `{"error":"invalid operation: must be add/update/delete"}`, http.StatusBadRequest)
		return
	}
	// 镜像校验：add/update 必须有 image（delete 不需要，按 name 删除）
	if req.Operation != "delete" && req.Pod.Image == "" {
		http.Error(w, `{"error":"pod.image is required for add/update"}`, http.StatusBadRequest)
		return
	}
	// 资源字段前置校验（WBS 6.5）：request≤limit + 格式合法性；
	// delete 操作不携带资源字段，跳过校验
	if req.Operation != "delete" {
		if err := req.Pod.Resources.validateResources(); err != nil {
			// 错误文案可能包含引号/反斜杠等 JSON 敏感字符（如畸形资源量
			// 经 %q 回显）：用 json.Marshal 构造响应体，避免裸拼字符串破坏
			// JSON 结构（与 409 分支同构，KNOWN-ISSUES §1 ③ 修复）。
			body, merr := json.Marshal(map[string]string{
				"error": "invalid resources: " + err.Error(),
			})
			if merr != nil {
				http.Error(w, `{"error":"marshal error response failed"}`, http.StatusInternalServerError)
				return
			}
			http.Error(w, string(body), http.StatusBadRequest)
			return
		}
	}

	msg, err := protocol.NewMessage(protocol.TypePodSync, "cloud", nodeID, req)
	if err != nil {
		http.Error(w, `{"error":"build message failed"}`, http.StatusInternalServerError)
		return
	}

	if err := api.reliableSend(r.Context(), nodeID, msg, cloudhub.ReliableOptions{}); err != nil {
		if errors.Is(err, cloudhub.ErrNodeOffline) {
			http.Error(w, `{"error":"node offline or not registered"}`, http.StatusNotFound)
			return
		}
		if errors.Is(err, cloudhub.ErrAckTimeout) {
			http.Error(w, `{"error":"ack timeout after retries"}`, http.StatusGatewayTimeout)
			return
		}
		if errors.Is(err, cloudhub.ErrAckFailed) {
			// 边缘明确回 error Ack（P2-2）：消息已送达但被拒绝。
			// WBS 6.5：错误带资源耗尽标记（resource.ErrResourceExhausted）
			// 时映射 409（超出节点资源），语义区别于通用边缘拒绝（502）。
			if strings.Contains(err.Error(), resource.ErrResourceExhausted) {
				// 错误文案可能包含引号/反斜杠等 JSON 敏感字符：用 json.Marshal
				// 构造响应体，避免裸拼字符串破坏 JSON 结构（P1-m1）。
				body, merr := json.Marshal(map[string]string{
					"error": resource.ErrResourceExhausted + ": exceeds node resources (" + err.Error() + ")",
				})
				if merr != nil {
					http.Error(w, `{"error":"marshal error response failed"}`, http.StatusInternalServerError)
					return
				}
				http.Error(w, string(body), http.StatusConflict)
				return
			}
			http.Error(w, `{"error":"edge rejected ack"}`, http.StatusBadGateway)
			return
		}
		http.Error(w, `{"error":"send failed"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write([]byte(`{"status":"ok","acked":true}`)); err != nil {
		logEncodeError("syncPod", err)
	}
}

// configSyncRequest 是云端下发 ConfigMap/Secret 配置的请求体（M2：WBS 6.2）。
// 与 PodSync 的 podSyncRequest 同构：operation + 配置对象。
// config 字段与边缘 ConfigSyncPayload.config 契约一致，原样下发。
type configSyncRequest struct {
	Operation string     `json:"operation"` // add / update / delete
	Config    configSpec `json:"config"`    // 配置对象（含 name/namespace/kind/data）
}

// configSpec 是配置的最小规格描述。
// Data 是 map[string]string：ConfigMap 的 value 为原始字符串；Secret 的
// value 按 base64 编码语义（云端负责编码，边缘原样存储）。
type configSpec struct {
	Name      string            `json:"name"`      // 配置名称（必填）
	Namespace string            `json:"namespace"` // 命名空间（缺省 "default"）
	Kind      string            `json:"kind"`      // 类型：ConfigMap / Secret（必填）
	Data      map[string]string `json:"data"`      // 键值数据（add/update 必填）
}

// syncConfig 通过可靠投递向指定边缘节点下发 ConfigSync 消息（WBS 6.2 端到端入口）。
// 校验规则：operation 白名单 {add,update,delete}、config.name 必填、
// kind 白名单 {ConfigMap,Secret}、add/update 时 data 非空。
// 响应语义与 syncPod 五态一致：200=边缘已确认（Ack ok）；400=请求非法；
// 404=节点未注册/离线；502=边缘回 error Ack（消息已送达但被拒绝）；
// 504=确认超时重试耗尽；500=其他发送失败。
func (api *nodeAPI) syncConfig(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("nodeID")

	var req configSyncRequest
	if err := decodeWriteBody(w, r, &req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			http.Error(w, `{"error":"request body too large (limit 1MiB)"}`, http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, `{"error":"invalid json body"}`, http.StatusBadRequest)
		return
	}
	if req.Operation == "" || req.Config.Name == "" {
		http.Error(w, `{"error":"operation and config.name are required"}`, http.StatusBadRequest)
		return
	}
	// operation 白名单校验（与 syncPod 同约定：云端前置校验，
	// 避免非法值下发到边缘，省一轮可靠投递往返）
	switch req.Operation {
	case "add", "update", "delete":
	default:
		http.Error(w, `{"error":"invalid operation: must be add/update/delete"}`, http.StatusBadRequest)
		return
	}
	// kind 白名单校验：ConfigMap/Secret 之外的类型直接 400 拒绝
	// （契约约束，防止未知类型数据落到边缘）
	switch req.Config.Kind {
	case "ConfigMap", "Secret":
	default:
		http.Error(w, `{"error":"invalid kind: must be ConfigMap/Secret"}`, http.StatusBadRequest)
		return
	}
	// data 校验：add/update 必须有 data（delete 不需要，按 name 删除）
	if req.Operation != "delete" && len(req.Config.Data) == 0 {
		http.Error(w, `{"error":"config.data is required for add/update"}`, http.StatusBadRequest)
		return
	}

	msg, err := protocol.NewMessage(protocol.TypeConfigSync, "cloud", nodeID, req)
	if err != nil {
		http.Error(w, `{"error":"build message failed"}`, http.StatusInternalServerError)
		return
	}

	if err := api.reliableSend(r.Context(), nodeID, msg, cloudhub.ReliableOptions{}); err != nil {
		if errors.Is(err, cloudhub.ErrNodeOffline) {
			http.Error(w, `{"error":"node offline or not registered"}`, http.StatusNotFound)
			return
		}
		if errors.Is(err, cloudhub.ErrAckTimeout) {
			http.Error(w, `{"error":"ack timeout after retries"}`, http.StatusGatewayTimeout)
			return
		}
		if errors.Is(err, cloudhub.ErrAckFailed) {
			// 边缘明确回 error Ack：消息已送达但被拒绝，
			// 与「没送达」（404/504）语义不同，映射 502 Bad Gateway。
			http.Error(w, `{"error":"edge rejected ack"}`, http.StatusBadGateway)
			return
		}
		http.Error(w, `{"error":"send failed"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write([]byte(`{"status":"ok","acked":true}`)); err != nil {
		logEncodeError("syncConfig", err)
	}
}

func serve(srv *http.Server, ln net.Listener, hub *cloudhub.Server, nc *nodecontroller.NodeController) int {
	// 在独立 goroutine 中启动 HTTP 服务与 CloudHub，错误通过 channel 上报主流程。
	// 热重载端口切换（WBS 2.7）会关闭旧监听并另起 Serve：监听被主动关闭
	// （net.ErrClosed）与优雅退出（ErrServerClosed）都不是致命错误。
	errCh := make(chan error, 2)
	go func() {
		log.Infof("HTTP server listening on %s", ln.Addr())
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			errCh <- err
		}
	}()
	go func() {
		if err := hub.Start(); err != nil {
			errCh <- err
		}
	}()

	// 监听 SIGINT（Ctrl+C）/ SIGTERM（kill）信号，用于优雅退出
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		// 任一服务启动/运行失败：尽力关闭全部后返回失败
		log.Errorf("服务异常退出: %v", err)
		shutdownAll(srv, hub, nc)
		return 1
	case sig := <-quit:
		log.Infof("收到信号 %s，开始优雅退出...", sig)
	}

	// 给正在处理的请求最多 5 秒完成时间
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ok := true
	if err := srv.Shutdown(ctx); err != nil {
		log.Errorf("HTTP 服务优雅退出失败: %v", err)
		ok = false
	}
	if err := hub.Shutdown(ctx); err != nil {
		log.Errorf("CloudHub 优雅退出失败: %v", err)
		ok = false
	}
	// NodeController 扫描循环随之停止（优雅退出的一部分）
	nc.Stop()
	if !ok {
		return 1
	}
	log.Infof("cloudcore exited")
	return 0
}

// shutdownAll 在服务异常退出时尽力关闭 HTTP 服务与 CloudHub（结果只记日志）。
func shutdownAll(srv *http.Server, hub *cloudhub.Server, nc *nodecontroller.NodeController) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Errorf("HTTP 服务关闭失败: %v", err)
	}
	if err := hub.Shutdown(ctx); err != nil {
		log.Errorf("CloudHub 关闭失败: %v", err)
	}
	nc.Stop()
}

// httpReloader 持有 HTTP 服务的当前监听，支持热切换监听端口（WBS 2.7）。
//
// 热切换语义：先在新端口建立监听并开始 Serve（新连接走新端口），再关闭
// 旧监听——旧监听上已建立的连接继续处理完（关闭 listener 不影响活动连接），
// 但不再接受新连接。新端口绑定失败时返回错误，旧监听保持不变（fail-safe，
// 重载被整体拒绝）。
type httpReloader struct {
	mu  sync.Mutex
	srv *http.Server
	ln  net.Listener
}

// swapPort 把 HTTP 服务切换到新端口（幂等：端口未变时直接返回 nil）。
func (h *httpReloader) swapPort(port int) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ta, ok := h.ln.Addr().(*net.TCPAddr); ok && ta.Port == port {
		return nil
	}
	newLn, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("新端口 %d 绑定失败: %w", port, err)
	}
	oldLn := h.ln
	h.ln = newLn
	h.srv.Addr = newLn.Addr().String()
	// 新监听先开始 Serve；随后关闭旧监听（其 Serve 返回 net.ErrClosed，
	// 主 serve goroutine 已将其视为非致命错误）
	go func() { _ = h.srv.Serve(newLn) }()
	if err := oldLn.Close(); err != nil {
		log.Warnf("旧监听 %s 关闭失败: %v", oldLn.Addr(), err)
	}
	log.Infof("HTTP 监听端口热切换: %s → %s", oldLn.Addr(), newLn.Addr())
	return nil
}

// applyConfigReload 是 cloudcore 的热重载策略（Reloader 的提交前钩子）：
//
//   - port（HTTP/healthz/API 监听端口）：热切换监听（swapPort）——这是
//     本实现评估后认为可安全热生效的端口类配置：新端口绑定失败即拒绝
//     重载、旧监听保持，切换过程不丢活动连接；
//   - hubPort（CloudHub WS 监听端口）：需重启生效——CloudHub 服务端不支持
//     运行期重建监听（cloud/pkg/cloudhub 未提供该能力，改动其内部超出
//     WBS 2.7 范围），因此记录警告并把旧值回写进 next（快照始终反映
//     运行中真实监听端口，不撒谎）；
//   - compress（云边通道压缩开关）：需重启生效——压缩协商发生在连接
//     注册时，运行期切换会让新旧连接协商状态不一致（已协商连接保持
//     压缩、新连接回落明文），因此与 hubPort 同策略：警告并回写旧值。
//
// 返回错误时本次重载被整体拒绝（快照保持旧配置）。
func applyConfigReload(old, next *config.Config, hr *httpReloader) error {
	if next.HubPort != old.HubPort {
		log.Warnf("hubPort 变更（%d → %d）需重启 cloudcore 生效，本次重载保持 %d（CloudHub 不支持运行期重建监听）",
			old.HubPort, next.HubPort, old.HubPort)
		next.HubPort = old.HubPort
		next.HubPortSource = old.HubPortSource
	}
	if next.Compress != old.Compress {
		log.Warnf("compress 变更（%v → %v）需重启 cloudcore 生效，本次重载保持 %v（压缩协商在连接注册时确定）",
			old.Compress, next.Compress, old.Compress)
		next.Compress = old.Compress
	}
	if next.Port != old.Port {
		if err := hr.swapPort(next.Port); err != nil {
			return fmt.Errorf("HTTP 端口 %d 热切换失败（保持 %d）: %w", next.Port, old.Port, err)
		}
	}
	return nil
}

// nodeAPI 是节点查询 API（/api/v1/nodes）的处理器集合。
// 通过结构体字段注入注册表、Pod 状态存储与设备状态存储
// （依赖注入，避免全局变量）。
type nodeAPI struct {
	// reg 是节点注册表存储（纯内存或 etcd 写穿实现；与 CloudHub 事件桥接共享同一实例）。
	reg registry.Store
	// pods 是 Pod 状态存储（纯内存或 v0.9.0 写穿实现；与 CloudHub 上报回调共享同一实例）。
	pods podstatus.Store
	// devices 是设备状态存储（纯内存或 etcd 写穿实现；与 CloudHub 上报回调
	// 共享同一实例，WBS 5.3）。
	devices devicestatus.Store
	// reliableSend 是可靠投递函数，默认指向 hub.ReliableSend（run 装配时注入）。
	// 独立成字段是为了让 syncPod 可测：测试注入 fake 即可覆盖各错误路径
	// （离线/超时/失败），无需真实 WebSocket 节点与 Ack 往返。
	reliableSend func(ctx context.Context, nodeID string, msg *protocol.Message, opts cloudhub.ReliableOptions) error
}

// listNodes 处理 GET /api/v1/nodes：返回全部节点（JSON 数组，按 NodeID 排序）。
func (a *nodeAPI) listNodes(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(a.reg.List()); err != nil {
		logEncodeError("listNodes", err)
	}
}

// getNode 处理 GET /api/v1/nodes/{nodeID}：返回单节点详情；节点不存在时 404。
func (a *nodeAPI) getNode(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("nodeID")
	info, ok := a.reg.Get(nodeID)
	if !ok {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		if err := json.NewEncoder(w).Encode(map[string]string{"error": "node not found", "nodeID": nodeID}); err != nil {
			logEncodeError("getNode", err)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(info); err != nil {
		logEncodeError("getNode", err)
	}
}

// edgeNodeList 是 GET /api/v1/edgenodes 的响应形态。
//
// 选择 K8s List 风格（kind/apiVersion + items 数组）而非裸数组：
// 与 apiserver 的列表响应结构一致，后续接入真实 apiserver 时
// 客户端解析逻辑无需改动；items 里的元素就是完整 EdgeNode 对象
// （含 kind/apiVersion，可直接当作 CRD 对象消费）。
type edgeNodeList struct {
	Kind       string              `json:"kind"`
	APIVersion string              `json:"apiVersion"`
	Items      []v1alpha1.EdgeNode `json:"items"`
}

// listEdgeNodes 处理 GET /api/v1/edgenodes：返回全部节点的 EdgeNode
// 对象列表（按 Name 排序）。
func (a *nodeAPI) listEdgeNodes(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// 编码失败（如客户端已断开）时无需额外处理，忽略即可
	if err := json.NewEncoder(w).Encode(edgeNodeList{
		Kind:       "EdgeNodeList",
		APIVersion: v1alpha1.SchemeGroupVersion.String(),
		Items:      a.reg.ListEdgeNodes(),
	}); err != nil {
		logEncodeError("listEdgeNodes", err)
	}
}

// podStatusList 是 Pod 状态查询 API 的响应形态。
//
// 选择 K8s List 风格（kind/apiVersion + items 数组）而非裸数组：
// 与 edgenodes 接口的 edgeNodeList 形态一致，也与 apiserver 的列表响应
// 结构一致，后续接入真实 apiserver 时客户端解析逻辑无需改动。
// items 恒为非 nil（空数据编码为 [] 而非 null）。
type podStatusList struct {
	Kind       string                `json:"kind"`
	APIVersion string                `json:"apiVersion"`
	Items      []podstatus.PodStatus `json:"items"`
}

// listPods 处理 GET /api/v1/pods：返回全部节点的 Pod 状态
// （按 nodeID/namespace/podName 排序）；无数据时返回空数组而非 null。
func (a *nodeAPI) listPods(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// 存储未注入（测试等场景）时按空列表处理，避免 nil 编码为 null
	items := make([]podstatus.PodStatus, 0)
	if a.pods != nil {
		items = a.pods.ListAll()
	}
	// 编码失败（如客户端已断开）时无需额外处理，忽略即可
	if err := json.NewEncoder(w).Encode(podStatusList{
		Kind:       "PodStatusList",
		APIVersion: "v1",
		Items:      items,
	}); err != nil {
		logEncodeError("listPods", err)
	}
}

// listNodePods 处理 GET /api/v1/nodes/{nodeID}/pods：返回单节点的 Pod 状态。
//
// 语义约定：
//   - 节点不存在（从未注册）→ 404（与 /api/v1/nodes/{nodeID} 的 404 语义一致）
//   - 节点存在但无 Pod → 200 + 空数组（不是 404："节点健康、只是还没 Pod"
//     与 "节点未知" 是两种语义，空数组让客户端可以无分支地遍历）
func (a *nodeAPI) listNodePods(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("nodeID")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if a.reg == nil {
		w.WriteHeader(http.StatusNotFound)
		if err := json.NewEncoder(w).Encode(map[string]string{"error": "node not found", "nodeID": nodeID}); err != nil {
			logEncodeError("listNodePods", err)
		}
		return
	}
	if _, ok := a.reg.Get(nodeID); !ok {
		w.WriteHeader(http.StatusNotFound)
		if err := json.NewEncoder(w).Encode(map[string]string{"error": "node not found", "nodeID": nodeID}); err != nil {
			logEncodeError("listNodePods", err)
		}
		return
	}
	items := make([]podstatus.PodStatus, 0)
	if a.pods != nil {
		items = a.pods.ListByNode(nodeID)
	}
	if err := json.NewEncoder(w).Encode(podStatusList{
		Kind:       "PodStatusList",
		APIVersion: "v1",
		Items:      items,
	}); err != nil {
		logEncodeError("listNodePods", err)
	}
}

// getEdgeNode 处理 GET /api/v1/edgenodes/{nodeID}：返回单个 EdgeNode
// 对象；节点不存在时 404。
func (a *nodeAPI) getEdgeNode(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("nodeID")
	info, ok := a.reg.Get(nodeID)
	if !ok {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		if err := json.NewEncoder(w).Encode(map[string]string{"error": "node not found", "nodeID": nodeID}); err != nil {
			logEncodeError("getEdgeNode", err)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Get 返回的是拷贝，取地址安全
	if err := json.NewEncoder(w).Encode(a.reg.ToEdgeNode(&info)); err != nil {
		logEncodeError("getEdgeNode", err)
	}
}
