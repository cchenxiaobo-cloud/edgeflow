# CODE REVIEW M4B — mTLS 安全认证（WBS 7.1/7.4）+ 镜像构建与 Helm 完整化（WBS 8.5）

- 审查日期：2026-08-14
- 审查人：M4B 复核员（资深 Go 工程师 + 安全工程师视角）
- 审查范围提交：`0a7fcc2`（pkg/certs + CloudHub/EdgeHub TLS 接入 + 装配 + SECURITY.md）、`3bb60ce`（Dockerfile + .dockerignore + Chart 完整化 + DEPLOYMENT.md）
- 约束：只读复核，未修改任何代码（仓库 `git status` 仅新增本报告文件）
- 基线核对：主线声称的 mTLS 端到端实测、证书 0600、race/lint/覆盖率 —— 本文第 3 节独立复验

---

## 0. 结论摘要

**结论：有条件通过（CONDITIONAL PASS）**

- 代码质量：高。certs 包（幂等/半套 fail-fast/权限/回滚）、TLS 配置（双向强制、TLS1.2+）、接入方式（tls.NewListener 集成正确、r.TLS 审计、wss 归一化）、镜像（多阶段 distroless 静态非 root）、Chart（探针/env/资源/安全上下文齐全）均实现正确。
- 安全方向全部 fail-closed，未发现可利用的安全绕过（无 P0）。
- 主要问题集中在**文档与部署路径不可执行**（P1×4）：错误连接路径 `/edgehub`、引用不存在的环境变量、Chart 只读根文件系统与证书生成冲突、服务端证书 SAN 硬编码导致 mTLS 仅限回环场景。
- 一次偶发 race（未捕获栈，无法定性），列为观察项。

---

## 1. 代码阅读记录

### 1.1 pkg/certs/certs.go（新增，376 行）

- 纯标准库：crypto/x509、crypto/rsa、crypto/tls。零第三方依赖 ✓
- 单 CA 模型：自签 CA（CN=edgeflow-ca，10 年，RSA 2048，IsCA + CertSign|CRLSign|DigitalSignature）
- 叶子证书：1 年；服务端 SAN=IP:127.0.0.1 + DNS:localhost/cloudcore，EKU=ServerAuth；客户端 CN=edgeflow-<nodeID>，EKU=ClientAuth
- **幂等语义**：文件齐全→加载不重签；只存在一半→报错 fail-fast（防静默轮换）；均不存在→生成。测试 `TestEnsureCA_Idempotent`/`TestEnsureLeaf_Idempotent` 用字节级比较证明不重签
- **写盘顺序与回滚**：先私钥（0600）后证书（0644），证书写失败回滚删除私钥，避免"有 key 无 crt"不完整态
- 序列号：128 位随机正数（`rand.Int(rand.Reader, 1<<128)`），防猜测/碰撞
- 私钥 PEM：PKCS#1（加载兼容 PKCS#8）
- 损坏文件处理：PEM 解析失败/非 CERTIFICATE/非 RSA → 明确报错不降级
- `LoadTLSConfig`：server 形态 = 服务端证书 + ClientCAs + `RequireAndVerifyClientCert` + MinVersion=TLS1.2；client 形态 = 客户端证书 + RootCAs + TLS1.2
- 发现缺陷（详见 4.1 / P2-1）：`loadCA` 不校验 CA 私钥与 CA 证书公钥匹配，与代码注释及 SECURITY.md §3.2 声称的"公私钥匹配校验"不符

### 1.2 cloud/pkg/cloudhub/server.go（TLS 接入，+40 行）

- `WithTLS(tlsConfig)` Option：传 nil = 纯 WS 向后兼容；非 nil 时 `Start` 内 `srv.TLSConfig = tlsConfig; ln = tls.NewListener(ln, tlsConfig)`
- 与 net/http `ListenAndServeTLS` 内部同构：握手由 http.Server 在单连接上懒执行，失败只影响该连接（日志 `http: TLS handshake error`），`Shutdown` 语义与纯 TCP 监听一致 ✓
- 握手成功后 `r.TLS.PeerCertificates` 被填充 → `serveWS` 记录审计日志 `mTLS 连接已认证（peer CN=...）` ✓（测试日志实测出现）
- `CheckOrigin` 仍全放行，注释明确为有意保留（TLS 层已认证），与 SECURITY.md §5 一致
- 存量并发模型（trackConn/wg/closeAllConns）未受 TLS 影响

### 1.3 edge/pkg/edgehub/client.go（TLS 接入，+47 行）

- `Options.TLSConfig`：nil = 纯 ws 向后兼容（`TestNew_TLSAddressNormalization` 基线断言 ws:// 不变）
- `normalizeTLSScheme`：TLSConfig 非 nil 时 ws:// 自动升级 wss://；显式 wss:// 保持
- 拨号器：`TLSClientConfig = opts.TLSConfig` 当（TLSConfig 非 nil || 地址为 wss://）；wss 且无 TLSConfig 时走系统根证书池（自签 CA 场景会失败——fail-closed 正确行为，注释已说明）
- 无 InsecureSkipVerify ✓；服务端主机名校验由 URL host 触发（与 4.4 P1-4 相关）

### 1.4 cmd/cloudcore/main.go + cmd/edgecore/main.go（装配，各 +31 行）

- 开关：`EDGEFLOW_CLOUDCORE_TLS=on` / `EDGEFLOW_EDGECORE_TLS=on`（精确匹配 "on"）
- 目录：`EDGEFLOW_*_CERT_DIR` 默认 `data/certs`；先 EnsureCA → 再 Ensure 各自叶子 → LoadTLSConfig；任一步失败退出码 1（fail-fast）
- 客户端 CN：`edgeflow-` + nodeID；已有证书直接加载不因 nodeID 变化重签（注释明示）
- TLS off 路径：`hubTLS` 为 nil → `WithTLS(nil)` 不生效，行为与历史完全一致 ✓
- **代码中不存在 `EDGEFLOW_CLOUDCORE_TLS_CERT/KEY` 环境变量**（grep 全仓无引用）→ 文档引用为虚构（P1-2）

### 1.5 build/docker/Dockerfile + .dockerignore（新增）

- 多阶段：builder（golang:1.26-alpine，先 COPY go.mod/go.sum + go mod download 利用层缓存）→ cloudcore/edgecore 两个 distroless 运行 target
- `CGO_ENABLED=0 GOOS=linux` 静态编译（modernc sqlite 纯 Go，无需 cgo）✓；`-trimpath -s -w`
- 版本注入：`-X edgeflow/pkg/version.Version/GitCommit/BuildTime`，与 Makefile LDFLAGS 变量名一致 ✓（实测 `docker run --version` 输出 v0.1.0）
- 运行镜像 `gcr.io/distroless/static-debian12:nonroot`：无 shell/包管理器，uid 65532；ca-certificates + tzdata 随镜像分发 ✓
- edgecore：`/data` 预授权 65532（builder 阶段 chown），`VOLUME /data` + `EDGEFLOW_EDGECORE_DB_PATH=/data/edgeflow.db` ✓
- .dockerignore 排除 .git/docs/data/bin/build/charts 等，上下文最小化 ✓

### 1.6 build/charts/edgeflow（values.yaml + templates + Chart.yaml + NOTES.txt）

- values：镜像/副本/terminationGracePeriod/双端口/探针/资源/安全上下文（pod + 容器双层）/env/extraEnv/nodeSelector/tolerations/affinity/imagePullSecrets/service（hubEnabled 默认 false）完整
- cloudcore-deployment.yaml：探针 `httpGet: /healthz` 端口名 `http`（与代码 httpx.Healthz 一致）✓；securityContext `readOnlyRootFilesystem: true` + drop ALL + allowPrivilegeEscalation false ✓；env 经 map range 渲染（text/template 按 key 排序，确定性输出）✓
- cloudcore-service.yaml：selector 与 deployment matchLabels 一致 ✓
- Chart.yaml：version 0.1.0 / appVersion v0.1.0 与镜像 tag 一致 ✓
- 已知缺口：Chart 无 volumeMounts/volumes 模板、无 tls 字段（文档声明"按需追加"）；仅含 cloudcore（edgecore 以二进制/裸容器部署，文档明示）

### 1.7 hack/gen-certs.sh（新增，openssl 实现，与 Go 同布局同参数）

- 幂等跳过、半套 die、私钥 0600、序列号文件 600 + 随机播种；CA/叶子参数与 pkg/certs 一致
- 注：显示层对 `$CERT_DIR/*.key` 路径做了脱敏显示，已用 xxd 十六进制核实文件内容真实正确（`bash -n` 通过）
- SAN/EKU 与 Go 侧一致；**SAN 同样硬编码**（IP:127.0.0.1,DNS:localhost,DNS:cloudcore，见 P1-4）

### 1.8 docs/SECURITY.md（131 行）+ docs/DEPLOYMENT.md（214 行）

- SECURITY.md：架构图、开关表、证书布局表、生命周期（生成/加载/轮换）、拒绝路径日志表（明文/无证书/错 CA/不信任云证书）、已知限制清单（CA 私钥保护、无吊销、无 CSR、CN 未绑定、证书整机共享、CheckOrigin）——整体完整、诚实
- DEPLOYMENT.md：构建/推送/Helm 安装/验证/values 说明/边缘接入/升级回滚/排障/验证清单
- 发现文档错误：§2.5 连接串路径 `/edgehub`（P1-1）；§3 与 values extraEnv 示例引用虚构环境变量 `EDGEFLOW_CLOUDCORE_TLS_CERT/KEY`（P1-2）；mTLS×Chart 组合未说明只读根文件系统与证书生成的冲突（P1-3）

---

## 2. 测试断言核实（读码确认断言真实、非空转）

| 测试 | 断言 | 判定 |
|---|---|---|
| TestEnsureCA_GenerateAndLoad | CN/IsCA/10 年±1 天/0644/0600/加载往返一致 | 真实 |
| TestEnsureCA_Idempotent | 私钥字节 + mtime 不变（二次不重签） | 真实 |
| TestEnsureLeaf_GenerateAndLoad | SAN 集合、EKU 精确（client 无 ServerAuth）、0600、`cert.Verify` 链校验 | 真实 |
| TestEnsureLeaf_Idempotent | 换 CN 调用仍不重签（字节+mtime） | 真实 |
| TestEnsureCA_CorruptFile / PartialPair | 损坏报错、半套报错 | 真实 |
| TestLoadTLSConfig | ClientAuth 精确值 / ClientCAs / RootCAs / MinVersion | 真实 |
| TestTLSHandshake_{Success,NoClientCert,WrongCA} | 真实握手：成功 / 无证书被拒 / 异 CA 被拒 | 真实 |
| TestServer_WSRegisterOverTLS | mTLS 握手 + WS 升级 + 完整 Register→Ack + NodeCount | 真实 |
| TestServer_RejectPlainWSOnTLS | ws:// 拨 TLS 端口失败，且拒绝后 mTLS 仍可用 | 真实 |
| TestServer_RejectUntrustedClientCert | rogue CA 证书握手被拒 | 真实 |
| TestNew_TLSAddressNormalization | TLS off 基线 ws:// 不变；TLS on ws→wss；显式 wss 保持 | 真实 |
| TestClient_FullRegisterOverTLS | 真实 mTLS 注册 + 心跳往返（云端侧证据） | 真实 |
| TestClient_TLSRejectsUntrustedServer | 客户端拒绝不可信服务端；云端 upgrades==0（握手在升级前失败） | 真实 |

---

## 3. 命令验证输出（本机独立复验）

```
环境: macOS arm64, go 1.26.2, helm v4.2.3, golangci-lint, docker 29.4.3

go build ./...                       → OK
go vet (certs/cloudhub/edgehub/cmd)  → 0 issues
go test -race -cover ./pkg/certs/... ./cloud/... ./edge/... ./cmd/...
  → 13 包全绿（certs 71.4% / cloudhub 84.7% / edgehub 85.9% / cmd-cloudcore 80.1% / cmd-edgecore 74.6% ...）
golangci-lint run (4 目标包)          → 0 issues
helm lint build/charts/edgeflow      → 1 chart(s) linted, 0 failed
helm template test                   → image/probes/env/securityContext 渲染正确
docker run --rm edgeflow/cloudcore:v0.1.0 --version
  → version=v0.1.0 goVersion=go1.26.6（版本注入生效）
docker inspect → User=65532（nonroot）
docker images → cloudcore 16.7MB / edgecore 22.5MB（与声称一致）
```

**偶发 race 记录（观察项）**：首次全量并行 `-race -cover` 时 `cloud/pkg/cloudhub/TestShutdownDuringNewConnections` 报 `race detected during execution of test`（未捕获 race 栈）。随后：单跑 ×3、`-count=10` 加压、全量复跑 ×3（两轮全量 + 一轮 13 包）全部通过。该测试为存量 P2-1 Shutdown 并发窗口压测（本轮两提交未触碰 server_test.go），判定为高负载下偶发，无法定性为真实竞态 → P2-3 观察项。

**复核实验（overlay/临时目录方式，仓库零改动）**：将 `ca.key` 换成另一 CA 的私钥（ca.crt 不变）后 `EnsureCA` 返回成功（未检测不匹配）；随后签发叶子证书被 Go 拒绝（`x509: provided PrivateKey doesn't match parent's PublicKey`）→ 证实 P2-1：加载层缺匹配校验，但签发层 fail-closed 兜底。

---

## 4. 维度审查

### 4.1 证书管理（pkg/certs）— 通过（附 P2-1）

- RSA 2048 / 10 年 CA / 1 年叶子 / 128 位随机序列号 / SAN+EKU 精确 ✓
- 幂等：字节级验证不重签；半套文件 fail-fast 防静默轮换 ✓
- 私钥 0600（测试断言）、证书 0644；写序 + 失败回滚 ✓
- 损坏 PEM / 非 CA / 非 RSA → 明确报错 ✓
- 路径遍历：certDir 为操作者配置、文件名固定常量，无外部输入注入路径 ✓
- **P2-1**：`loadCA` 未校验 CA 私钥与 CA 证书公钥匹配（仅叶子对经 `tls.LoadX509KeyPair` 校验）。代码注释与 SECURITY.md §3.2 声称"公私钥匹配校验"名不副实。实测后果 fail-closed（签发时报 `provided PrivateKey doesn't match parent's PublicKey`）；若叶子证书已齐全而仅 ca.key 被替换，存量连接不受影响、新节点签发失败——静默不匹配态。建议 `loadCA` 比对 `x509.MarshalPKIXPublicKey`。

### 4.2 TLS 配置 — 通过

- 服务端：`ClientCAs=CA 池` + `ClientAuth=RequireAndVerifyClientCert`（mTLS 核心，测试实测无证书/异 CA 均被拒）✓
- 客户端：`RootCAs=CA 池` + 携带客户端证书；服务端证书与主机名双校验（无 InsecureSkipVerify）✓
- 两侧 `MinVersion=TLS1.2`；MaxVersion 不设（允许 1.3）✓
- 证书选择：单证书无 SNI 需求（单域名服务），无多证书场景 ✓

### 4.3 TLS 接入 — 通过

- CloudHub：`tls.NewListener` + `http.Server.Serve` 与 net/http 内部 TLS 路径同构；`r.TLS` 正确填充（测试日志 `mTLS 连接已认证（peer CN=edgeflow-tls-test）` 实证）；握手错误仅影响单连接（日志 `http: TLS handshake error ... bad certificate` / `client sent an HTTP request to an HTTPS server` 实证）；Shutdown 语义不变 ✓
- EdgeHub：wss 归一化逻辑正确（仅 TLSConfig 非 nil 时升级 ws→wss；显式 wss 保持）；TLS off 完全向后兼容（Address() 基线断言）✓
- 明文拨 TLS 端口被拒（TLS 层拦截，协议层不可达）✓

### 4.4 安全边界 — 通过（附 P1-4）

- CheckOrigin 仍放行：已文档化，TLS 层完成身份认证，浏览器 CSWSH 风险低 ✓
- CN 与 nodeID 未绑定：已文档化（SECURITY.md §5），审计日志记录 peer CN 可供后续增强 ✓
- 吊销/轮换缺失：已文档化，轮换流程写明 ✓
- CA 私钥保护：0600 + 文档化生产建议（离线签发/KMS/目录分离）✓
- 审计：握手成功记录 peer CN + 来源 IP ✓
- **P1-4（功能边界）**：服务端证书 SAN 硬编码 `IP:127.0.0.1 / DNS:localhost / DNS:cloudcore`，Go 侧与 gen-certs.sh 均无注入点。跨主机部署（edge 连云真实 IP/域名）或集群内按 Service DNS（`edgeflow-cloudcore`）连接时，主机名校验必失败且无文档化变通指引（SECURITY.md §5 已知限制未列此项）。当前 mTLS 实际仅回环/单机可用。建议：gen-certs.sh 增加 SAN 参数、Go 侧支持环境变量注入 SAN，或至少文档明确限制与 openssl 手工重签指引。

### 4.5 镜像构建 — 通过

- 多阶段正确：CGO_ENABLED=0 静态（modernc sqlite 纯 Go）、distroless nonroot（uid 65532 实测）、无 shell 攻击面最小 ✓
- 版本注入：Dockerfile ARG 与 Makefile LDFLAGS 变量名一致，实测注入生效；`docker run --version` 可验证 ✓（"版本注入缺口"未发现）
- 大小：16.7MB / 22.5MB（实测一致），合理 ✓
- ca-certificates + tzdata 随镜像分发（静态二进制不自带）✓
- edgecore 数据目录预授权 + VOLUME + DB_PATH 固定 /data ✓

### 4.6 Helm Chart — 有条件通过（P1-2/P1-3）

- values 完整、探针路径 /healthz 正确（端口按名引用）、env 透传渲染正确、helm lint/template 通过 ✓
- 安全上下文：双层 nonroot + drop ALL + 只读根文件系统 ✓
- **P1-2**：values.yaml extraEnv 示例与 DEPLOYMENT.md §3 引用 `EDGEFLOW_CLOUDCORE_TLS_CERT/KEY`——代码中不存在（实际为 `EDGEFLOW_CLOUDCORE_TLS=on` + `EDGEFLOW_CLOUDCORE_CERT_DIR`）。照文档配置 mTLS 无效。
- **P1-3**：Chart `readOnlyRootFilesystem: true` 与"首次启动自动生成证书（写入 data/certs）"冲突：Chart 内启用 mTLS 时 EnsureCA 写盘失败 → 启动即退出。唯一可行路径是 Secret 预置全套证书 + CERT_DIR 指向挂载点（Secret 卷只读，加载路径可行），但文档未说明该路径与安全权衡。建议 Chart 内置 `cloudcore.tls` 字段（certSecret 挂载 + 可写 emptyDir 供自动生成）或文档写明预置方案。

### 4.7 生产就绪度（文档与异常路径）— 有条件通过（P1-1）

- SECURITY.md：完整诚实，拒绝路径日志表与实测一致 ✓
- DEPLOYMENT.md：构建/安装/排障/回滚齐全，验证清单与实测一致 ✓
- **P1-1**：DEPLOYMENT.md §2.5 与 NOTES.txt 的边缘连接串 `ws://<节点IP>:<NodePort>/edgehub` 路径错误——CloudHub 仅注册 `/v1/edge` 路由（实测 grep 确认无 `/edgehub`），照文档操作必然 404 握手失败。应改为 `ws://<IP>:<Port>/v1/edge` 或省略路径（EdgeHub 自动补全）。

---

## 5. 结论

**有条件通过（CONDITIONAL PASS）** — 代码层面质量高、安全方向 fail-closed、无 P0；P1 全部集中在文档/部署路径的可执行性，不阻断代码验收，但阻断"照文档启用 mTLS/边缘接入"的生产路径，须在合入前或紧随其后修复文档与 Chart 缺口。

---

## 6. 问题清单

### P0（阻断发布）
- 无

### P1（应修复）
1. **文档连接串路径错误**：DEPLOYMENT.md §2.5 + NOTES.txt 使用 `ws://<IP>:<NodePort>/edgehub`，CloudHub 无此路由（仅 `/v1/edge`），照文档必失败。改为 `/v1/edge` 或省略路径。
2. **虚构环境变量**：DEPLOYMENT.md §3 + values.yaml extraEnv 示例引用 `EDGEFLOW_CLOUDCORE_TLS_CERT/KEY`（代码不存在）。修正为 `EDGEFLOW_CLOUDCORE_TLS=on` + `EDGEFLOW_CLOUDCORE_CERT_DIR`，并给出 Secret 挂载 + 预置证书的真实示例。
3. **Chart 内启用 mTLS 无可行路径**：`readOnlyRootFilesystem: true` 阻断证书自动生成；文档未说明"Secret 预置全套证书 + CERT_DIR 指向只读挂载"的可行路径。建议 Chart 增加 `cloudcore.tls`（certSecret 挂载 + 可写卷）或文档写明预置方案与安全权衡。
4. **服务端证书 SAN 硬编码，mTLS 仅限回环**：SAN（127.0.0.1/localhost/cloudcore）在 Go 与 gen-certs.sh 均无注入点，跨主机/集群 Service DNS 连接必失败；SECURITY.md §5 未列此限制。建议支持 SAN 注入（脚本参数/环境变量）并文档化。

### P2（建议改进）
1. `loadCA` 不校验 CA 公私钥匹配（注释与 SECURITY.md §3.2 声称已校验，名不副实）；实测签发层 fail-closed 兜底但错误点隐晦。建议加载时比对公钥指纹。
2. Chart 内启用 mTLS 时建议提供预置证书校验提示（Start 失败错误信息不指向只读根文件系统，排障成本高）。
3. **观察项**：`TestShutdownDuringNewConnections`（存量压测，非本轮改动）在高负载并行 `-race` 下偶发一次 race 报告，未捕获栈，10+ 次复跑全绿。若复现需抓完整 race 栈定性。
4. TLS 握手阶段无超时与并发上限（net/http 长期未实现握手 deadline，ReadHeaderTimeout 不覆盖握手阶段）：TLS 端口暴露于不可信网络时存在慢握手消耗 goroutine 的风险。建议部署层限流/防火墙，或接入层加握手 deadline。
5. gen-certs.sh 客户端证书 CN 固定 `edgeflow-edgecore`，与 Go 侧 `edgeflow-<nodeID>` 不一致（不影响认证，仅审计可读性；脚本注释已说明）。

---

*报告生成：审查人独立执行（读码 → 读测试 → 跑验证 → 实验复核），未修改仓库任何文件。*
