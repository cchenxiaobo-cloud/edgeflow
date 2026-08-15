# EdgeFlow 镜像安全扫描记录（WBS 10.4 / B6）

> 扫描器：Trivy 0.74.0（2026-08-15 安装于本机）
> 扫描对象：`edgeflow/cloudcore:v0.1.0`、`edgeflow/edgecore:v0.1.0`（docker 本地镜像，release/v0.1.0 双架构构建同源）
> 漏洞数据库：trivy-db:2（2026-08-15 下载）
> 扫描命令：`trivy image --scanners vuln --no-progress <镜像>`
> 结论：**两镜像 0 漏洞（CRITICAL/HIGH/MEDIUM/LOW 全 0）**

---

## 1. 扫描结果

| 镜像 | 基础镜像 | 系统包漏洞 | Go 二进制漏洞 | 总计 |
|------|---------|-----------|--------------|------|
| edgeflow/cloudcore:v0.1.0 | distroless static-debian12:nonroot（debian 12.15） | 0 | 0 | **0** |
| edgeflow/edgecore:v0.1.0 | 同上 | 0 | 0 | **0** |

## 2. 修复过程（首扫发现 → 修复 → 复扫）

1. **首扫（2026-08-15）**：edgecore 镜像检出 **10 个漏洞**（5 HIGH + 5 MEDIUM），全部来自
   `golang.org/x/net v0.44.0`（indirect 依赖）：
   - HIGH：CVE-2026-25681 / CVE-2026-27136 / CVE-2026-33814 / CVE-2026-39821 / CVE-2026-46600
   - MEDIUM：CVE-2025-47911 / CVE-2025-58190 / CVE-2026-25680 / CVE-2026-42502 / CVE-2026-42506
   - cloudcore 镜像首扫即 0 漏洞。
2. **修复**：`go get golang.org/x/net@v0.56.0`（满足全部 CVE 的 FixedVersion，最高要求 0.56.0），
   `go mod tidy`；全量 `go build ./...` + 全包 `go test` 通过。
3. **重建**：`docker build -f build/docker/Dockerfile --target cloudcore|edgecore`（GIT_COMMIT=当前 HEAD）。
4. **复扫**：两镜像全部 **0 漏洞**。

## 3. 镜像基线（复扫后）

| 项 | cloudcore | edgecore |
|----|-----------|----------|
| 镜像 ID | ef7254acc790 | 264fe7dd650e |
| 大小 | 17.4MB（压缩 3.69MB） | 22.9MB（压缩 5.4MB） |
| 版本注入 | v0.1.0 + 当前 GIT_COMMIT | v0.1.0 + 当前 GIT_COMMIT |
| 漏洞 | 0 | 0 |

## 4. 后续建议

- **定期扫描**：每次发布前执行上述 trivy 命令；建议 CI 阶段加入 `trivy image --exit-code 1 --severity HIGH,CRITICAL` 门禁。
- **依赖看护**：`golang.org/x/net` 等 indirect 依赖用 `govulncheck` 或 Dependabot 持续跟踪。
- **基础镜像**：distroless static-debian12 随上游更新，重扫时自动覆盖 debian 12.x 系统包。
- **远程仓库**：本地 registry（localhost:5001）未运行；推送远程后需对远程 tag 重扫一次（环境边界，无凭据）。
