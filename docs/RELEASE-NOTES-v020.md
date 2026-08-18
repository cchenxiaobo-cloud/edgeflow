# EdgeFlow v0.2.0 发布说明

- 版本号：v0.2.0（minor：功能增量）
- 类型：功能发布
- 代码基线 commit：`d3f09fe`（fix(edged): 资源漂移检测接入真实调谐路径）
- 发布记录 HEAD：`71823bc`（release 制品），tag v0.2.0 指向
- 发布日期：2026-08-18
- 范围：v0.1.1 后延后登记的剩余计划项（WBS 6.5 资源调度、WBS 5.1 Mapper 装配开关、Modbus namespace、P3 小项收尾）

## 一、功能变更

### 1. 资源调度基础（WBS 6.5，P2 全量）

- K8s 风格资源语义：`cpuRequest/cpuLimit`（"250m"）、`memoryRequest/memoryLimit`（"64Mi"），零值 = 不限制（云边契约只增不改）
- 云端前置校验：request>limit → HTTP 400（含具体超标字段文案）；超卖拒绝 → HTTP 409 `EDGEFLOW_RESOURCE_EXHAUSTED`（不落盘、不建容器）；其余边缘拒绝 → 502
- 边缘准入：admitPodResources fail-closed，拒绝不落盘
- 超卖率：默认 CPU/内存 150%，`EDGEFLOW_EDGECORE_OVERCOMMIT_CPU_RATE/_MEMORY_RATE` 可调（非有限值回退默认）
- 节点容量：自动探测 + `EDGEFLOW_EDGECORE_NODE_CPU_MILLI/_MEMORY_BYTES` 覆盖（非法回退探测值）
- 容器落地：docker `--cpus/--memory/--memory-swap`（swap 禁用）
- 资源漂移检测：调谐 3c 分支镜像+资源双漂移检查，外部 `docker update` 改动 limit 后自动 stop+重建恢复（每轮最多重建 1 个滚动门控，不计入 RestartCount）
- 输入校验：ParseCPU 拒绝 Inf/NaN/超 int64 毫核范围/前导 '+'

### 2. Mapper 框架 EventBus 装配开关（WBS 5.1）

- `EDGEFLOW_EDGECORE_ENABLE_MAPPER`，默认 `true`（与 v0.1.1 行为一致）
- 关闭（false/0/off/no，大小写不敏感）→ 不注册 Mapper、不启动采集循环、指令仅更新 Twin.Desired（纯影子模式，行为与装配前完全一致）

### 3. ModbusMapper DeviceNamespace 接口

- 新增 `DeviceNamespace()` + 三级解析：WithNamespace 选项 > `EDGEFLOW_MODBUS_NAMESPACE` env > 默认 `default`
- 注册表按 namespace 隔离路由（同名设备分 ns 共存）；指令路径 ns 路由生效，错误 ns → 502

### 4. P3 小项收尾

- `pkg/log`：SetLevel/GetLevel/Debugf 全局级别过滤（默认 Info，与旧行为逐字节兼容）
- cloudhub：被踢标志即时清除（窗口内旧连接心跳收 not_registered）
- edgehub：退避重置两段式单测（flakyDialer 注入）
- Shutdown 撞 Start 并发防护补测

### 5. 质量与修复（本开发轮）

- 代码复核：0 blocker / 1 major（已修）/ 7 minor（修 6 项，m3 记录为已知限制）/ 5 nit（n1 已修）
- 修复内容：ParseCPU 静默收 NaN/Inf/溢出、超卖率 env 非有限值、409 响应 json.Marshal、死代码删除与舍入口径统一、手写字符串组件换 stdlib、单边畸形值 fail-closed、测试缺口补测（env 覆盖/回退、错误路径、命名空间归一）
- 冒烟 1d 暴露并修复：资源漂移检测接入真实调谐路径（装配缺口，修复前为死代码）
- release/v0.1.1/* 记录修正（images.json size 口径）：独立 commit `9cf7772`，tag v0.1.1 保持不动

## 二、验证记录

| 项 | 结果 | 证据 |
|---|---|---|
| go build/vet/gofmt | 全绿 | 主线两轮回归 |
| 关键包 -race | 全绿 | pkg/resource、edged、cmd/cloudcore、cmd/edgecore、cloudhub、edgehub、modbus、pkg/log |
| 全仓 go test ./... | 全绿 | 含 tests/contract |
| e2e 独立复跑 ×2 | 全绿 | 204.7s / 203.0s |
| 预发冒烟 8/8 | PASS | 1a limit 端到端（NanoCpus=250000000/Memory=134217728）、1b 超卖 409、1c request>limit 400、1d 漂移重建（注入 500m → ≤15s 重建回 250m，修复后复验）、2a Mapper 默认开启、2b 开关关闭影子、3 Modbus ns 路由隔离（真机模拟器）、4 log SetLevel |
| 制品自检 | 通过 | checksums 13/13；9 二进制 --version=v0.2.0+d3f09fe；helm lint 0 failed；govulncheck 9/9 clean（go1.26.6）；trivy 双镜像 0 HIGH/0 CRITICAL；4/4 平台镜像 docker run --version 实测 |

## 三、制品清单

- 9 个二进制（cloudcore/edgecore/keadm × darwin-arm64/linux-amd64/linux-arm64，GOTOOLCHAIN=go1.26.6）
- edgeflow-0.2.0.tgz（helm chart，version 0.2.0 / appVersion v0.2.0）
- sbom.json（CycloneDX 1.5）
- 双架构镜像（cloudcore/edgecore × linux/amd64+linux/arm64，manifest list；cloudcore index `sha256:a83c4ec0…`、edgecore index `sha256:813bb6a7…`）
- images.json + checksums.txt
- govulncheck.txt（9/9 No vulnerabilities found）

## 四、已知限制（新增）

1. `collectMapperReports` 周期采集硬编码 default ns：Modbus 多 ns 部署时云端出现 default/plant-a 双条目（指令路径正确，采集路径待 Mapper 采集结果扩展时解决）
2. edgehub 退避测试仍用实时阈值（m3 跳过：改造需生产代码注入点）
3. cloudcore 400 分支 err.Error() 裸拼 JSON（下批修）
4. EDGEFLOW_EDGECORE_RECONCILE/REPORT/DEVICE_REPORT_INTERVAL 传 300ms 静默回落默认（合法区间 1s~10m）
5. 镜像构建受 Docker Hub 故障影响，本轮采用离线 COPY-only 策略（base 走 gcr.io）；mediaType 为 docker manifest list v2（功能等价 OCI index）

## 五、升级注意事项

- 零破坏：云边契约只增不改（resources 字段 omitempty）；Mapper 开关默认 true 与 v0.1.1 行为一致；log 默认级别输出逐字节兼容
- 新能力均需显式配置才生效（资源字段、环境变量开关），存量部署不受影响
