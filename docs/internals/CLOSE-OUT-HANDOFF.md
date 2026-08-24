# EdgeFlow 部署与交接建议

> 时间：2026-08-14 · 依据：收尾核对报告 + RELEASE-CHECKLIST.md + DEPLOYMENT.md + DRILL-SCHEDULE.md

---

## 1. 当前可交付部署形态

| 形态 | 状态 | 说明 |
|------|------|------|
| 单机开发/演示部署 | ✅ 可交付 | demo.sh 一键跑通（~5 分钟）；Docker + 本地进程 |
| 生产集群部署（Helm） | 🟨 可交付（未在真实集群验证） | Chart 完整（lint/dry-run 通过），真实集群执行待 A7 |
| 边缘节点接入 | 🟨 可交付（keadm 产物化） | init/join/upgrade/rollback 命令实测；真实节点执行待验证 |
| 镜像 | 🟨 单架构（arm64） | 多架构待 B5 |

## 2. 部署计划（建议顺序）

### 阶段 1：单机演示（已可执行）
```bash
./examples/demo.sh          # 一键端到端（cloudcore+edgecore+Pod+设备）
```

### 阶段 2：测试集群验证（A7，3 周内）
1. kind 起集群（或测试集群）
2. `keadm init` → `kubectl apply -f keadm-out/cloudcore.yaml` → 确认 Pod Running
3. `keadm join`（边缘节点）→ `kubectl get nodes` 确认 Ready
4. CRD apply（需先补 manifest yaml，B4）
5. 计时验证 15min 部署目标
6. 产出验收记录 → 归档到 RELEASE-LEDGER.md

### 阶段 3：生产部署（演练通过后）
1. 前置：RBAC（A4）→ 可观测性（A3）→ 安全扫描（B6）→ 多架构镜像（B5）
2. Helm install（带 values）或 keadm init --tls
3. mTLS 强制开启（TLS=on + SAN 注入）
4. 观察期 7 天（告警/工单跟踪）

## 3. 回滚方案（含暂停、备份、恢复三环节）

### 3.1 镜像/Chart 回退
| 环节 | 动作 |
|------|------|
| 暂停 | `helm upgrade --recreate-pods` 前先停止新流量；或 `kubectl rollout pause deployment/cloudcore` |
| 备份 | 记录当前版本与 digest；备份 values（`helm get values`） |
| 恢复 | `helm rollback cloudcore <previous-revision>`；或镜像 tag 回退（MULTIARCH.md §6） |

### 3.2 keadm 产物回滚
| 环节 | 动作 |
|------|------|
| 暂停 | 停止 edgecore 服务（systemctl stop edgecore） |
| 备份 | 升级自动备份已在 backups/（keadm upgrade 内置） |
| 恢复 | `keadm rollback --latest`（事务化恢复，实测 diff 一致） |

### 3.3 数据兼容与影响
- 升级/回滚**不触碰数据目录**（SQLite 独立）；已实测数据 hash 一致
- 版本兼容：CloudCore/EdgeCore 建议同版本部署（协议 Version=v1）
- 升级期间新数据：保留（SQLite 不被覆盖）

### 3.4 执行前提与审批
- 生产回滚需：变更审批单 + 值班确认 + 备份验证完成
- 演练先行（DRILL-SCHEDULE.md）通过后才可生产操作

## 4. 交接清单

### 4.1 仓库与版本
| 项 | 位置 |
|----|------|
| 代码 | /Users/mac/Documents/edgeflow/（108 commits，HEAD eacd35a） |
| 版本 | v0.1.0（tag 未打——建议发布时打 tag） |
| 制品 | release/v0.1.0/（10 文件） |
| 文档 | docs/（47 份） |

### 4.2 交接检查单（接手人逐项确认）
- [ ] setup-env.sh 可幂等复现环境
- [ ] make build && make test && make lint 全过
- [ ] examples/demo.sh DEMO PASS
- [ ] docs/CLOSE-OUT-REPORT.md 已读（缺口 G1-G17 已知）
- [ ] docs/CLOSE-OUT-ACTIONS.md 责任人与排期已确认（W1-W4）
- [ ] GitHub 远程关联完成（C7，用户操作）
- [ ] 生产部署前置（A3/A4/B5/B6）完成后再上线

### 4.3 已知问题提示（接手时必读）
- 云端 API 当前**无认证**（7.2 未做）——暴露前必须完成 RBAC
- 无真实集群验证记录——生产部署前必须 A7
- 发布镜像 arm64 单架构——异构节点需 B5
- ROADMAP/PROGRESS 状态列滞后——以 CLOSE-OUT 报告为准

## 5. 后续交接动作

1. 项目负责人确认 CLOSE-OUT-ACTIONS.md 责任人（W1）
2. 打 v0.1.0 git tag（当前 HEAD 即发布内容）
3. 用户完成 GitHub 关联（C7）→ CI 首跑确认
4. 按 A1-A7 优先级推进整改
5. 生产部署前完成 B 类前置项
