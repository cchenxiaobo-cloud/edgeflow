# EdgeFlow v0.20.0 发布说明（发布生命周期收口：失败节点重试 + 终态发布归档删除 + 发布元数据）

- **发布日期**：2026-08-27
- **版本基线**：v0.19.0 → v0.20.0
- **主题**：失败不整单重来、终态不留垃圾、发布带得上变更注记
- **兼容性**：HTTP 端点 40→**42** 只增不改；全部新参数 opt-in 缺省行为与 v0.19.0 逐字节一致；升级零迁移；老边缘零动作；**零新依赖**

## 一、核心能力

### 1. 失败节点重试（LC-A）
- `POST /api/v1/models/{m}/releases/{id}/retry`：仅**终态**发布可 retry；克隆新发布（新 ID + RetryOf 回指原发布），正常走 guard/批次全流程
- TargetNodes = 原发布 failed 节点子集（`nodeIDs` 可选缩围；不在集合内 → 400 带 failedNodes）；成功节点不被二次打扰
- 目标版本仍须 active（执行期被归档/删除 → 422 阻断"补发已删版本"）；无 failed 节点 422；空 body 合法（缺省全部 failed）
- guard 冲突照常 409（同模型在途互斥不变）；时间线 created + retried 双事件落账

### 2. 终态发布归档删除（LC-B）
- `DELETE /api/v1/models/{m}/releases/{id}`：手动点删单条终态发布（succeeded/failed/canceled/rolled_back）
- 非终态一律 409——与 GC 同源「在途绝不删」语义；双存储同步删头键 + releases/<id>/ 子键（对齐 GCReleases 删除路径）
- 200 响应被删快照（id/status/retryOf）供审计落地；**删除不可逆**（无回收站）

### 3. releaseNotes 发布元数据（LC-C）
- 创建请求体可选 `releaseNotes`（≤1024 字节 opt-in）：变更单号/发起人/窗口等短注记
- 头内嵌持久化（Memory/Etcd 双实现），list/get/snapshot/global list **全读取路径透出**
- PATCH 白名单不含该字段——元数据创建期定死不可变；retry 克隆自动继承原发布 notes

## 二、验证摘要（实测）

- 全量 `go test -race ./...` 两轮回归 + 终轮 **37 包 0 FAIL**（首轮 registry 翻面用例 ×3 复跑证清白；文档一致性守卫当场拦下 §1.1 漏行并修复复绿）
- 新增测试：V200 API ×3（retry 校验链链序/子集核对/422 族 + delete 终态守卫/摘要形态 + releaseNotes 创建/全路径透出/PATCH 不可变/超限 400）+ **TestV200LifecycleOpsE2E 云端全链路（20.7s）**：真实 cloudcore + 双边缘节点，首发跑完 → retry 语义分支 → 归档删除 → 404 链序 → 在途守卫
- 契约守卫三处联动绿：契约表 40→42、路由计数 26 条 want 数组 +2 且发布族 11→13、API-SPEC §1.1 补行 ×2 + 新增 §7.10、API-COMPATIBILITY §1 第 41/42 行

## 三、升级注意

- **零迁移**：DeleteRelease 为既有键空间的点删除；无 schema 变化
- 行为差异面仅显式调用两个新端点或携带 releaseNotes 创建时存在；缺省行为逐字节不变
- 回滚 = 回退二进制即可；L7 网关若有路径策略需放行一条新 POST 与一条新 DELETE（均挂 Bearer Token 认证）
- 升级后老版本二进制不知道这两个端点——多副本混布时先升全部云端副本再放行调用方

## 四、遗留（非阻断，见 KNOWN-ISSUES §20）

- retry 链式无深度限制（可再 failed 再 retry，审计经 RetryOf 回溯口径登记）
- 归档删除不可逆、无回收站（export 备份口径不变）
- releaseNotes 为自由文本不做结构化校验（1024 上限外直接 400 无截断）
