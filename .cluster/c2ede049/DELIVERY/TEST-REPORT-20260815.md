# EdgeFlow 后续开发轮 — 测试与质量检查报告

- 轮次：2026-08-15 后续开发轮（goal `2bcbad93`，工作台 `.cluster/c2ede049/`）
- 仓库：`/Users/mac/Documents/edgeflow`，main 分支
- 基线：`f6b4898`（收尾轮 head）→ 本轮 head：`09e4707`（10 个提交）
- 范围：ROADMAP 2.7 配置热重载 / 4.4 gzip 压缩 / 7.1 证书轮换 / 10.2 灰度发布 / 9.1 架构文档回写 / 四份审查记录（M1B/M1C/M1/M3A）遗留 P2/P3 修复

---

## 1. 测试执行总览

| 检查项 | 命令 | 结果 |
|--------|------|------|
| 全量单元测试（race） | `go test -race -count=1 ./...` | ✅ 全部通过（EXIT 0，9 个关键包全 ok） |
| 关键包专项（第一轮后） | `go test -race ./pkg/config/... ./cmd/cloudcore/... ./cmd/edgecore/...` | ✅ 2.070s / 4.017s / 3.885s |
| 关键包专项（第二轮后） | `go test -race ./cmd/keadm/... ./pkg/certs/...` | ✅ 4.396s / 7.398s |
| 代码格式 | `gofmt -l` | ✅ 无输出（全部干净） |
| 静态检查 | golangci-lint（worker 侧执行） | ✅ 0 issues（gzip/keadm worker 各自验证） |
| 构建 | `go build` 三个二进制（cloudcore/edgecore/keadm） | ✅ BUILD-OK |

## 2. 各任务测试证据

| 任务 | 关键测试（均 `-race` 通过） | 数量 |
|------|------------------------------|------|
| N1 配置热重载（2.7） | `TestReloader*`（快照/串行化/错误回退/Stop）、`TestApplyConfigReload`（端口热切换/回滚/非法配置保持旧值）、`TestApplyEdgeCoreReload`（上报周期透传/cloudAddr/nodeID/reconcileInterval 回写/no-op）5 子用例 | 3 组 |
| N2 gzip 压缩（4.4） | `TestCompressionRoundTrip*`、四象限互操作、`TestDecompressionBomb*`（1MiB 明文上限）、小消息回落、损坏数据报错、`TestCompressionNegotiatedUplink`（含 Register 声明断言）、`TestCompressionNotAdvertisedUplinkPlaintext`、`TestCompressionRenegotiateAfterReconnect` | 5 包全绿 |
| N3 存储/注册表（审查遗留） | `TestListPrefixRangeSemantics`（通配符字面/大小写敏感/0xff 进位/非法 UTF-8/空前缀全表）、`TestOfflineTTLGC`（>= 语义边界）、`TestLazyGCOnWrite`、`TestConcurrentAccessWithGC`、`TestMarkOfflineTwiceKeepsClock`（复核 M1 新增） | 5+ |
| N4 通道/API（审查遗留） | `TestWaitConnectionsTimeout`、`TestShutdownDuringActiveDial`（10 轮拨号×Shutdown）、`TestSyncPodBodyTooLarge/AtLimit`、`TestSyncConfigBodyTooLarge`、`TestDeviceCommandBodyTooLarge`、`TestNewIDFallbackOnRandError/TestNewIDRandomUnique`、`TestRegisterMemoryAsUint64`、`TestRegisterTokenAuth`（NodeCount==0） | 10+ |
| N5 证书轮换（7.1） | rotate 备份/重签/幂等/错误路径/CN 校验/事务化落盘（pkg/certs 19 个新增） | 19 |
| N6 灰度升级（10.2） | 分批中止/暂停/参数校验（fake executor/sleeper） | 5+ |
| 基线核查确认 | pending 交叉清理、ErrAckFailed→502、SetReadLimit、Memory uint64（抽查测试存在） | ✅ |

## 3. 代码评审处置

| 评审 | 结论 | 处置 |
|------|------|------|
| 代码风险复核（review_code.md） | 需修复后交付 | **B1 阻断**（gzip 协商断裂）：已修复 `5ac07f8`（Register 声明 + 测试断言）；**M1**（MarkOffline 时钟重置）：已修复 `8f1bc80` + 专项测试；**M2**（接收侧容忍）：补安全注释 `8f1bc80`；**L1-L7** 低风险：登记入档，不阻塞 |
| 文档一致性复核（review_docs.md） | 需修正后交付 | **F1-F10 全部修正** + S1-S9 随附项，`09e4707`；无误项 12 组抽查全部通过 |

## 4. 已知残留（登记不阻塞）

- L1 等待连接 goroutine 超时后短暂存活（进程随即退出，无实际影响）
- L2 SIGHUP watcher stop 不等待 goroutine（库级复用语义瑕疵）
- L3 cert rotate 双 Rename 非原子窗口（备份先行 + 恢复提示，已文档化）
- L4 rotate 并发备份 id 冲突（单进程操作无影响）
- L5 registry GC O(n)（当前规模无影响，演进留待 apiserver 迁移）
- L6 swapPort Serve 错误静默（正常路径不触发）
- L7 上报周期热重载延迟一个旧周期（≤30s，已文档化）
- M3A byDevice namespace/多实例 Mapper/LastReportedAt/502 Desired：记录结论不改行为
- C7 GitHub 远程关联 + CI 首跑：需用户操作（环境边界）

## 5. 结论

✅ 全部 9 条验收标准证据链齐备（plan-and-backlog / progress-tracking / code-implementation / test-and-quality / review-and-exception / staged-verification / rollback-ready / production-launch / handoff-and-docs）；无已知阻断问题，可交付。
