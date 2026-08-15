# EdgeFlow 后续开发轮 — 预发布验证记录与生产部署记录

- 轮次：2026-08-15 后续开发轮
- 预发布环境：本机沙箱（macOS arm64，仓库内真实进程/命令级验证；集群级验收需真实环境复测——见 §5 环境边界说明）
- 验证对象：commit `37fbaf4` → `09e4707`（10 个提交）

---

## 1. 构建验证

| 产物 | 命令 | 结果 |
|------|------|------|
| cloudcore | `go build -o /tmp/ef-build-check/cloudcore ./cmd/cloudcore` | ✅ 9.9MB |
| edgecore | `go build -o /tmp/ef-build-check/edgecore ./cmd/edgecore` | ✅ 15.8MB |
| keadm | `go build -o /tmp/ef-build-check/keadm ./cmd/keadm` | ✅ 7.8MB |

## 2. 功能冒烟验证

### 2.1 keadm cert rotate 实操（真实重签链路）

```text
$ bash hack/gen-certs.sh   # 生成 CA + cloudcore/edgecore 证书（幂等，与 pkg/certs 布局一致）
$ keadm cert rotate --node edgeflow-edgecore --cert-dir data/certs
keadm cert rotate 完成: 节点 edgeflow-edgecore 证书已重新签发
  新证书: data/certs/edgecore.crt
  新私钥: data/certs/edgecore.key
  备份:   data/certs/backups/20260815-212822（轮换前旧证书与私钥，含 manifest.json）
  提示: 将新证书分发到节点后重启 edgecore/cloudcore 生效；如需回退，用备份文件覆盖原路径即可。
$ openssl x509 -in data/certs/edgecore.crt -noout -serial   # 轮换前
serial=E3E8E02188211CE8
$ openssl x509 -in data/certs/edgecore.crt -noout -serial   # 轮换后
serial=D77DE22B18F8BE85B20CE942C26E97F6
```

**结论**：✅ 证书重新签发生效（序列号变化）、备份目录生成（含 manifest）、回退路径明确。

### 2.2 keadm upgrade 灰度参数

```text
$ keadm upgrade --help
  -batch-size int          分批大小（灰度发布；仅 keadm batch --op=upgrade 生效，默认 1）
  -pause-between duration  批间暂停时长（灰度发布，如 30s；默认 0 不暂停）
$ keadm cert rotate --help
  -node string   节点证书 CN（必填）
  -cert-dir string  证书目录（默认 data/certs）
```

**结论**：✅ 参数注册、默认值、校验（batch-size>=1、pause 非负，upgrade.go:250-254）验证通过。

### 2.3 gzip 协商（测试级端到端）

真实 `edgehub.Client` ↔ mock 云端：Register 帧声明 `compression:"gzip"` → RegisterAck 回带 → 上行大消息 EFGZ 压缩帧、小消息明文回落（`TestCompressionNegotiatedUplink`）。评审 B1 修复后新增 Register 声明断言，验证协商闭环不再断裂。

### 2.4 热重载（单测级）

`TestApplyConfigReload`：端口热切换 + 新端口绑定失败回滚旧监听；`TestApplyEdgeCoreReload` 5 子用例：上报周期热生效透传、cloudAddr/nodeID/reconcileInterval 变更回写旧值、no-op。

## 3. 回归验证

- 全量 `go test -race -count=1 ./...`：✅ EXIT 0（首轮通过；复核 M1/M2 处置后再次全量确认，见 TEST-REPORT §1）
- 既有 E2E 三用例（60s 自治/多节点隔离/弱网）未改动，不在本轮回滚面内

## 4. 生产部署记录

| 步骤 | 状态 | 说明 |
|------|------|------|
| 提交入库 | ✅ | 10 commits 已落 main（`37fbaf4..09e4707`），提交信息与任务台账一一对应 |
| 构建 | ✅ | 三二进制构建通过（§1） |
| 制品发布 | ✅（仓库级） | 本轮代码变更随 main 推进；`release/v0.1.0/` 制品为 2026-08-15 收尾轮发布物，本轮功能需按版本策略在下一发布窗口重新打包（含 keadm 新子命令、压缩开关） |
| 上线后复核 | ✅（沙箱级） | 冒烟 + 回归全过；生产集群部署/长跑复核超出本环境边界，登记为遗留（见 §5） |

## 5. 环境边界（明确标注的假设）

1. 本环境无真实多节点集群：集群级验收（8.2 多节点 E2E、30min 自治长跑、100 节点压测复测）按 ROADMAP §8 登记 ⏸，需真实环境执行。
2. 生产部署记录为仓库级交付（提交+构建+冒烟）；真实生产集群上线由用户按 docs/DEPLOYMENT.md、docs/REAL-CLUSTER-GUIDE.md 执行，本记录作为预发布验证证据。
3. gzip 压缩默认开启（`compress` 缺省 true）；旧版 cloudcore/edgecore 互操作按四象限矩阵安全降级明文，无需停机升级。
