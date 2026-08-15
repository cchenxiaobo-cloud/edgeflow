# EdgeFlow 未完成开发项收尾交付说明（2026-08-15）

> 依据：docs/收尾审计报告.html（P1×7 已完成）+ docs/CLOSE-OUT-ACTIONS.md 剩余项
> 范围：CLOSE-OUT-ACTIONS.md 中未完成项（B1/B3/B4/B6/B7/B8、C3/C6、C7）
> 交接对象：项目负责人 / 后续接手人
> 台账：docs/CLOSE-OUT-ACTIONS.md（状态列已更新）

---

## 1. 本轮完成项总览

| # | 任务 | 状态 | 验证证据 | 提交 |
|---|------|------|---------|------|
| B1 | 7.3 设备认证（Token 消费） | ✅ 完成 | 单测 6 项 PASS + `hack/token-auth-check.sh` 真实进程验证（正确 token 注册成功；错误/缺失 token 被拒且注册表无污染；已注册节点无回归） | 本轮 commit |
| B3 | keadm 批量操作 | ✅ 完成 | 单测 5 项 PASS + 真实运行验证（2 节点批量 join，产物+IP 覆盖正确） | 本轮 commit |
| B4 | CRD manifest yaml | ✅ 确认已完成（b809e30） | config/crd/ 3 个 CRD 与 Go 类型字段一致（脚本对照验证），A7 kind 集群已 apply | b809e30 |
| B6 | 10.4 镜像安全扫描 | ✅ 完成 | Trivy 0.74.0：修复前 edgecore 10 漏洞（5H+5M，golang.org/x/net v0.44.0）→ 升级 v0.56.0 → 重建 → **两镜像 0 漏洞**；记录 docs/SECURITY-SCAN.md | 本轮 commit |
| B7 | 10.3 API 兼容矩阵 | ✅ 完成 | docs/API-COMPATIBILITY.md（11 REST + /healthz + /metrics + 9 消息 + 3 CRD + 变更登记） | 本轮 commit |
| B8 | 跨主机 CA 分发自动化 | ✅ 完成 | gen-certs.sh CERT_DIST_DIR 分发包实测：结构完整、私钥 0600、openssl verify 链验证 OK | 本轮 commit |
| C3 | 待办清单清理 | ✅ 完成 | PROGRESS.md §5 六项闭环回写 | 本轮 commit |
| C6 | 里程碑归属偏移回写 | ✅ 确认已完成（b809e30） | ROADMAP §3 注已标注 2.4/4.5/5.2/8.6 → M4 | b809e30 |
| — | P2 附加项：linux-arm64 发布二进制 | ✅ 完成 | release/v0.1.0/ 新增 cloudcore/edgecore/keadm-linux-arm64（ELF aarch64 静态链接，版本注入验证），checksums 已更新 | 本轮 commit |
| C7 | GitHub 远程关联 + CI 首跑 | ⏸ 需用户操作 | 无 git remote；SSH 公钥已存在 | — |

## 2. 回归检查结果（无新增破坏）

| 检查 | 结果 |
|------|------|
| `make lint` | ✅ 0 issues |
| `make test` 全量 | ✅ 全部包 PASS（含 tests/e2e 204.6s 三用例；keadm 76.9%、cloudhub 全过） |
| `examples/demo.sh` 端到端 | ✅ DEMO PASS（注册→Pod 下发/运行→设备数据→设备指令→MQTT 数据面→清理） |
| `hack/token-auth-check.sh` | ✅ 3 场景全过（B1 新增验证脚本） |

## 3. 部署/运行方式

### 3.1 设备认证（B1）启用
```bash
# 云端（cloudcore）：设置节点接入令牌
export EDGEFLOW_CLOUDCORE_NODE_TOKEN=<共享令牌>
bin/cloudcore --port 8080

# 边缘（edgecore）：keadm join 已自动写入，或手动设置
export EDGEFLOW_EDGECORE_TOKEN=<同一令牌>
bin/edgecore
```
未设置 `EDGEFLOW_CLOUDCORE_NODE_TOKEN` 时行为与 M1-M3 一致（不校验，向后兼容）。

### 3.2 keadm 批量操作（B3）
```bash
# 批量 join（清单每行：node-id 或 node-id,ip）
printf 'node-a\nnode-b,192.168.1.20\n' > nodes.txt
keadm batch --op=join --file=nodes.txt --cloudcore-ip=192.168.1.10 --token=<t>

# 批量 upgrade / rollback（清单每行：节点产物目录）
keadm batch --op=upgrade --file=dirs.txt --version=v0.2.0
keadm batch --op=rollback --file=dirs.txt --latest
```

### 3.3 镜像安全扫描（B6）
```bash
brew install trivy   # 已装 0.74.0
trivy image --scanners vuln --severity CRITICAL,HIGH,MEDIUM edgeflow/cloudcore:v0.1.0
trivy image --scanners vuln --severity CRITICAL,HIGH,MEDIUM edgeflow/edgecore:v0.1.0
```

### 3.4 跨主机 CA 分发（B8）
```bash
CERT_DIR=/etc/edgeflow/certs CERT_DIST_DIR=./certs-dist CLIENT_CN=edge-node-x bash hack/gen-certs.sh
scp -r certs-dist/cloud user@cloud-host:/etc/edgeflow/
scp -r certs-dist/edge/edge-node-x user@edge-host:/etc/edgeflow/
```

### 3.5 CRD（B4）
```bash
kubectl apply -f config/crd/edgenodes.edgeflow.io.yaml
kubectl apply -f config/crd/devicemodels.edgeflow.io.yaml
kubectl apply -f config/crd/devices.edgeflow.io.yaml
```

## 4. 每个改动的回滚方案

| 改动 | 回滚方式 |
|------|---------|
| B1 设备认证 | ① 云端：`unset EDGEFLOW_CLOUDCORE_NODE_TOKEN` 重启 cloudcore → 恢复不校验；② 代码回滚：`git revert` 本轮 commit（RegisterPayload.token 为可选字段，老版本互不兼容风险低；云边建议同版本部署） |
| B3 keadm batch | 新子命令，不影响既有命令；删除清单与产物目录即可，无状态残留（batch 复用 join/upgrade/rollback 的事务语义） |
| B6 x/net 升级 + 镜像重建 | 旧镜像仍在本地（localhost:5001 双架构 tag 未覆盖前）；`docker tag` 回退旧 digest；依赖回退：`go get golang.org/x/net@v0.44.0` |
| B7 API 兼容矩阵 | 纯文档，删除即可 |
| B8 CA 分发 | 纯脚本扩展（env 开关，默认不输出分发包）；旧调用方式不变 |
| C3/C6 文档回写 | git revert 对应文档提交 |
| arm64 二进制 | 发布目录新增文件，删除即回退（checksums.txt 同步回退） |

## 5. 剩余事项与风险

### 5.1 需要用户决策/操作
1. **C7 GitHub 关联**（唯一阻塞项）：`~/.ssh/id_ed25519.pub` 已就绪，需用户：GitHub 添加公钥 → 提供仓库地址 → `git remote add origin <url> && git push -u origin main` → 确认 CI lint+test 绿勾。
2. **v0.1.0 git tag**：建议按 RELEASE-CHECKLIST 打 tag（当前 HEAD 即发布内容）。
3. **本地 registry 推送**：镜像已重建（0 漏洞），推送远程 registry 需凭据；推送后对远程 tag 重扫一次。

### 5.2 剩余风险（登记于 CLOSE-OUT-RISKS.md，无新增）
- R1 真实多节点集群验证（kind 单节点已跑通，多节点网络/存储/证书差异待演练）
- R7 证书轮换人工编排（B8 已自动化分发，轮换 SOP 见分发包 README）
- 镜像远程推送后需重扫（本地已 0 漏洞）
- 设备认证为共享令牌（非每节点独立），生产可升级为每节点证书/令牌（架构预留 token 字段）

## 6. 后续建议
1. 完成 C7 后跑 CI 首轮（M0 验收"CI PR 反馈 ≤10min"实证）。
2. 生产部署前置项已全部就绪（RBAC/可观测性/安全扫描/多架构镜像/真实集群路径），按 CLOSE-OUT-HANDOFF.md 阶段 3 执行。
3. 建议将 trivy 扫描纳入发布清单门禁（SECURITY-SCAN.md §4）。
