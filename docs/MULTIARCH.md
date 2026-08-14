# EdgeFlow 多架构镜像构建文档（M4）

> 目标：cloudcore / edgecore 产出 **linux/amd64 + linux/arm64 双架构镜像**（OCI
> manifest 单 tag 双平台），满足 M4 验收：manifest 可构建 / 可拉取 / 可运行，
> 且两架构 `--version` 输出一致（版本在构建时注入）。

---

## 1. 架构总览

```
源码（Go，CGO_ENABLED=0 静态编译，SQLite 用 modernc.org 纯 Go 实现）
        │
        ▼
buildx（docker-container driver，QEMU 模拟异构架构）
        │  多阶段构建：builder（golang:1.26-alpine）→ 运行镜像（distroless static）
        ▼
OCI manifest（linux/amd64 + linux/arm64，单 tag 双架构）
        │
        ├─ 本地验证：registry:2 容器（127.0.0.1:5001）
        └─ 发布：CI release.yml → ghcr.io（或 docker.io）
```

- 静态二进制 + distroless：无 libc/glibc 依赖，跨架构行为一致，天然适合多架构分发。
- 版本注入：`--build-arg VERSION` → Dockerfile `ARG VERSION` → `-ldflags -X edgeflow/pkg/version.Version`，镜像内 `--version` 可验证。

---

## 2. 本地验证流程（无远端仓库时的闭环）

前置：Docker（含 buildx）、QEMU/binfmt 支持（Docker Desktop 自带；Linux 需
`docker run --privileged --rm tonistiigi/binfmt --install all`）。

> 注：本机 5000 端口被系统进程占用，本地 registry 使用 **5001** 端口；
> 其余环境若 5000 空闲可直接 `-p 5000:5000`。

```bash
# 1. 本地 registry
docker run -d -p 127.0.0.1:5001:5000 --name local-reg registry:2

# 2. buildx builder（docker-container 驱动，多架构构建必需）
#    本地 registry 为明文 HTTP，需给 BuildKit 声明 insecure registry：
cat > buildkitd.toml <<'EOF'
[registry."host.docker.internal:5001"]
  http = true
EOF
docker buildx create --name edgeflow-builder --driver docker-container \
  --config buildkitd.toml --use

# 3. 双架构构建 + 推送本地 registry（首次拉基础镜像较慢，耐心等待）
#    注意：推送地址用 host.docker.internal:5001（BuildKit 容器内 localhost ≠ 宿主机）
docker buildx build --platform linux/amd64,linux/arm64 \
  -f build/docker/Dockerfile \
  -t host.docker.internal:5001/edgeflow/cloudcore:v0.1.0 --target cloudcore --push .
docker buildx build --platform linux/amd64,linux/arm64 \
  -f build/docker/Dockerfile \
  -t host.docker.internal:5001/edgeflow/edgecore:v0.1.0  --target edgecore  --push .

# 4. manifest 校验：确认双架构齐全（--insecure：本地明文 HTTP registry）
docker manifest inspect --insecure localhost:5001/edgeflow/cloudcore:v0.1.0
#   → manifests 下应有 linux/amd64 与 linux/arm64 两项，且 digest 互不相同

# 5. 运行验证（arm64 本机原生 + amd64 QEMU 模拟）
docker run --rm localhost:5001/edgeflow/cloudcore:v0.1.0 --version
docker run --platform linux/amd64 --rm localhost:5001/edgeflow/cloudcore:v0.1.0 --version
#   → 两者输出一致（version=v0.1.0 ...）
docker run --rm localhost:5001/edgeflow/edgecore:v0.1.0 --version
docker run --platform linux/amd64 --rm localhost:5001/edgeflow/edgecore:v0.1.0 --version

# 6. 清理
docker rm -f local-reg
docker buildx rm edgeflow-builder
rm buildkitd.toml
```

### 2.1 registry API 检查

```bash
curl http://127.0.0.1:5001/v2/_catalog            # 仓库列表
curl http://127.0.0.1:5001/v2/edgeflow/cloudcore/tags/list
```

---

## 3. CI 发布流程（.github/workflows/release.yml）

触发：

| 方式 | 命令 | 版本来源 |
|---|---|---|
| tag 推送 | `git tag v0.1.0 && git push origin v0.1.0` | tag 名 |
| 手动 | GitHub Actions → Run workflow → 填 version | 输入框 |

步骤链：`checkout → setup-qemu → setup-buildx → login → 计算版本 → build-push（矩阵 cloudcore/edgecore）→ manifest 自检`。

- 平台矩阵：`linux/amd64,linux/arm64`（env.PLATFORMS 可调）。
- 版本注入：`--build-arg VERSION=<tag>` + `GIT_COMMIT=<sha>`。
- 推送目标：`ghcr.io/edgeflow/<service>:<tag>`（默认，GITHUB_TOKEN 免配置）；
  切 docker.io 需配置 `DOCKERHUB_USERNAME` / `DOCKERHUB_TOKEN` 两个 secrets
  （workflow 内已有条件登录逻辑与注释）。
- 发布后自检：`docker buildx imagetools inspect` 断言 amd64/arm64 均存在，缺失即失败。

---

## 4. 常用检查命令（排障/验收用）

```bash
# 看 manifest 结构（平台 + digest + 大小）
docker manifest inspect --verbose localhost:5001/edgeflow/cloudcore:v0.1.0

# 看远程 registry 的 manifest（CI 发布后验收）
docker buildx imagetools inspect ghcr.io/edgeflow/cloudcore:v0.1.0

# 显式按平台拉取并运行
docker run --platform linux/arm64 --rm <img> --version
docker run --platform linux/amd64 --rm <img> --version

# 构建缓存诊断
docker buildx du
```

---

## 5. 跨平台风险与注意事项

| 风险 | 说明 | 对策 |
|---|---|---|
| QEMU 模拟性能 | amd64 在 arm64 主机（或反之）上经 QEMU 模拟编译，速度约为原生的 1/5~1/10 | 仅 CI/本地验证使用；二进制是静态编译，运行时性能无损失 |
| cgo/动态链接 | 含 cgo 的二进制在 distroless 无 libc 环境会崩 | Dockerfile 已 `CGO_ENABLED=0`；SQLite 用 modernc.org 纯 Go 实现，无此风险；新引入依赖需确认纯 Go |
| distroless 无 shell | 容器内无法 exec 调试 | 日志走 stdout/文件挂载；需要时临时换 alpine 基镜像（Dockerfile 注释有说明） |
| registry 端口冲突 | 本机 5000 可能被占用 | 换端口（本地验证用 5001） |
| BuildKit 推本地 HTTP registry | docker-container 驱动的 BuildKit 在容器内，`localhost` 指 BuildKit 自身；非 localhost 地址默认拒绝 HTTP | 用 `host.docker.internal:5001` + buildkitd.toml `[registry."host.docker.internal:5001"] http=true`；宿主侧 inspect 用 `--insecure` |
| 偶发编译失败 | edgecore（modernc sqlite 编译吃内存）在双平台并行 + QEMU 下偶发 OOM/exit 1 | 重试即可（构建缓存复用，代价小）；CI 上 amd64 原生 + arm64 QEMU 并行压力更小 |
| buildx 首次拉镜像 | golang/distroless 双平台镜像 + go mod 下载，单次可达数 GB | CI 有缓存（build-push-action 自动）；本地耐心等待 |
| 版本注入遗漏 | `--version` 输出 dev，两架构不一致 | 构建必须传 `--build-arg VERSION`；CI 已固定 |
| registry 存储格式 | 旧 registry 不支持 manifest list | 用 registry:2（OCI 兼容） |
| :latest 可被覆盖（非 immutable）；无镜像签名/SBOM | 每次发布（含手动 dispatch）都覆盖 :latest tag，无不可变保护；未生成 cosign 签名与 SBOM | 按版本 tag 可追溯（M4C P2 记录）；签名/SBOM 依赖镜像仓库 + cosign 基础设施，**延后至 M5 发布阶段**（台账见 docs/PROGRESS.md） |

---

## 6. 回滚预案（发布失败）

分级响应：

1. **构建/推送失败（CI 红）**：修复后重跑 workflow（手动 dispatch 同一版本号，
   tag 已存在时覆盖推送）→ 不动已发布的 tag。
2. **镜像损坏/版本不对（已推送但不可用）**：优先**回退到单架构**——
   ```bash
   # 以 arm64 单架构重建并覆盖 tag（边缘设备主架构，最快止血）
   docker buildx build --platform linux/arm64 \
     -f build/docker/Dockerfile \
     -t ghcr.io/edgeflow/cloudcore:v0.1.0 --target cloudcore --push .
   # 或退回上一个可用版本
   docker buildx imagetools create -t ghcr.io/edgeflow/cloudcore:v0.1.0 \
     ghcr.io/edgeflow/cloudcore:v0.0.9
   ```
3. **tag 无法覆盖（远端拒绝）**：删除远端 tag
   `git push origin :refs/tags/v0.1.0`，修正后重新打 tag 推送。
4. **彻底回退到旧发布**：把 `v0.1.0` 指向上一版 digest：
   ```bash
   docker buildx imagetools create -t <reg>/<img>:v0.1.0 <reg>/<img>@sha256:<旧 digest>
   ```

原则：**先恢复可用，再排查根因**。单架构 tag 始终是可用的兜底产物。

---

## 7. 本地验证证据（2026-08-14，Apple Silicon + Docker Desktop）

```text
$ docker manifest inspect --insecure localhost:5001/edgeflow/cloudcore:v0.1.0
{
   "schemaVersion": 2,
   "mediaType": "application/vnd.oci.image.index.v1+json",
   "manifests": [
      { "architecture": "amd64", "os": "linux", "digest": "sha256:307c75fa..." },
      { "architecture": "arm64", "os": "linux", "digest": "sha256:81df9cdb..." }
   ]
}

$ docker manifest inspect --insecure localhost:5001/edgeflow/edgecore:v0.1.0
   linux/amd64  digest=sha256:28b44e44...
   linux/arm64  digest=sha256:fd8b79a7...

$ docker run --rm localhost:5001/edgeflow/cloudcore:v0.1.0 --version        # arm64 原生
version=v0.1.0 gitCommit=f71684e buildTime=unknown goVersion=go1.26.6

$ docker run --platform linux/amd64 --rm localhost:5001/edgeflow/cloudcore:v0.1.0 --version  # QEMU
version=v0.1.0 gitCommit=f71684e buildTime=unknown goVersion=go1.26.6

$ docker run --rm localhost:5001/edgeflow/edgecore:v0.1.0 --version        # arm64 原生
version=v0.1.0 gitCommit=f71684e buildTime=unknown goVersion=go1.26.6

$ docker run --platform linux/amd64 --rm localhost:5001/edgeflow/edgecore:v0.1.0 --version
version=v0.1.0 gitCommit=f71684e buildTime=unknown goVersion=go1.26.6

$ curl -s http://127.0.0.1:5001/v2/_catalog
{"repositories":["edgeflow/cloudcore","edgeflow/edgecore"]}
```

注：本地验证未传 BUILD_TIME（默认 unknown）；CI 已注入构建时间。

---

## 8. 验收清单（M4）

- [x] 本地 registry 闭环：双架构 manifest 构建 + 推送 + inspect 通过
- [x] amd64 / arm64 均可 `docker run ... --version`（QEMU 模拟验证 amd64）
- [x] 双架构版本输出一致（v0.1.0）
- [x] CI workflow：tag/手动触发、QEMU + buildx、版本注入、manifest 自检
- [x] 本机 Dockerfile 无需改动（ARG VERSION 已存在，零 Go 代码改动）
- [ ] 远端仓库推送（需仓库授权 + secrets，见 §3）——本地已验证，CI 落地即可发布
