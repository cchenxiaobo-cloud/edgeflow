# EdgeFlow 后续开发轮 — 版本回滚方案

- 适用发布：2026-08-15 后续开发轮（commit `f6b4898` → `09e4707`）
- 原则：每项功能按 commit 粒度可回退；既有 ops-ledger / 备份模型全部沿用，无新增不可逆操作。

---

## 1. 回滚点

| 级别 | 回滚点 | 说明 |
|------|--------|------|
| 代码库 | `git revert` 或 `git reset --hard f6b4898` | 本轮全部 10 个提交的父提交 = 收尾轮 head（上线前基线） |
| 逐项回退 | 各 commit 可独立 revert | 见 §2 功能级回退表 |
| 证书 | `data/certs/backups/<ts>/` | keadm cert rotate 每次轮换前自动备份（旧证书+私钥+manifest+sha256），覆盖原路径即回退 |
| 升级 | `keadm rollback`（既有能力） | 升级/回滚已有备份模型 `backups/<ts>/` + 事务化 restore + manifest 白名单，本轮灰度参数不改变该语义 |

## 2. 功能级回退表

| 功能 | commit | 回退方式 | 特殊注意 |
|------|--------|----------|----------|
| 配置热重载（2.7） | `2d0a903` | revert 或改回重启生效策略 | SIGHUP/轮询行为由代码控制，revert 后回到仅启动加载 |
| gzip 压缩（4.4） | `37f34f9` + `5ac07f8` | ① 配置开关：cloudcore 配置 `compress: false`（无需回退代码，重启生效）；② revert 代码 | 协商式兼容：仅回退一端（云或边）自动降级明文，无互操作风险 |
| keadm 证书轮换（7.1） | `2c877d4` | ① 不再使用 `cert rotate` 命令即可；② 已轮换节点用备份目录覆盖回退；③ revert 代码 | 轮换不自动分发不自动重启，操作员可控 |
| 灰度分批（10.2） | `2c877d4` | 不用 `--batch-size/--pause-between`（默认 batch-size=1）即回到逐节点行为 | 无 |
| 审查遗留修复（M1B/M1C/M1） | `37fbaf4`、`d9bc9ec` | revert 对应 commit | 范围扫描/GC/竞态修复为行为修正，revert 即回到旧语义 |
| 复核处置（M1/M2） | `8f1bc80` | revert | 离线时钟语义/注释 |
| 文档回写（9.1/N8/复核修正） | `1ae2d81`、`09e4707` | revert（不影响运行） | 无 |

## 3. 回滚操作步骤（代码库级）

```bash
# 方案 A：整轮回退（保留历史，追加反向提交）
cd /Users/mac/Documents/edgeflow
git revert --no-commit f6b4898..09e4707   # 按提交顺序反向应用
git commit -m "revert: 2026-08-15 后续开发轮（回滚演练）"

# 方案 B：强制回到基线（仅紧急/未共享远程时）
git reset --hard f6b4898

# 方案 C：单功能回退（例：关闭 gzip 且回退代码）
git revert 5ac07f8 37f34f9
```

## 4. 数据与配置回滚

| 项 | 备份位置 | 恢复方式 |
|----|----------|----------|
| 节点证书 | `data/certs/backups/<ts>/`（rotate 自动） | 备份文件覆盖 `edgecore.crt/key` 后重启 |
| 升级备份 | `backups/<ts>/`（upgrade 自动） | `keadm rollback`（既有命令） |
| 审计台账 | `audit-ledger.jsonl` / `ops-ledger.jsonl` | 追加式文件，回滚不触碰（可追溯） |
| 配置 | env/flag（无持久化配置库） | 按部署文档恢复 env |

## 5. 回滚验证（已完成部分）

- 冒烟验证：cert rotate 备份目录生成 + 恢复路径提示（PRE-RELEASE-VERIFICATION §2.1）
- keadm rollback 语义：既有单测覆盖（batch_test/upgrade 测试，5 项）——本轮未改动 rollback 行为
- 版本回退编译：基线 `f6b4898` 为已验证 head（收尾轮交付物），`git reset` 后可重建

## 6. 风险与建议

- 本轮无数据库 schema 变更、无不可逆数据操作，回滚面为零数据风险。
- gzip 开关回退优先级最高（配置级，无需发版）；证书轮换已轮换节点优先用备份覆盖（无需代码回退）。
- 建议生产环境在发布后保留上一发布窗口制品（release/v0.1.0 快照）至下一下次验证通过。
