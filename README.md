# EdgeFlow

EdgeFlow 是一个类 KubeEdge 的边缘计算项目，采用云边两级架构：

- **CloudCore（云端）**：运行在云端，未来负责云边通信（WebSocket）、消息路由（NATS）、设备管理（CRD）等。
- **EdgeCore（边缘端）**：运行在边缘节点，未来负责与云端建立连接、收发消息、上报设备数据等。

> 当前进度：M0 核心模块已完成 —— 基础共享库（version / log / httpx）+ cloudcore 最小可运行服务（提供 `/healthz` 健康检查）；edgecore 仍为占位程序，业务逻辑在后续迭代中实现。

## 目录结构

参考 [golang-standards/project-layout](https://github.com/golang-standards/project-layout)，保持最小：

```
edgeflow/
├── cmd/
│   ├── cloudcore/        # 云端组件入口（常驻服务，提供 /healthz）
│   └── edgecore/         # 边缘端组件入口（占位程序）
├── pkg/
│   ├── version/          # 版本信息（编译时通过 -ldflags 注入）
│   ├── log/              # 轻量日志封装（Info/Warn/Error，基于标准库）
│   └── httpx/            # HTTP 公共处理器（/healthz 健康检查）
├── apis/                 # API 定义（如设备 CRD，后续填充）
├── build/
│   └── charts/edgeflow/  # Helm Chart（部署清单，M4 前为骨架）
├── docs/                 # 文档
├── hack/                 # 开发脚本（后续填充）
├── .github/workflows/    # CI 流水线
├── go.mod
├── Makefile
└── README.md
```

## 环境要求

- Go 1.26+
- Make
- golangci-lint（可选，用于静态检查）

## 快速开始

```bash
# 1. 编译全部二进制（输出到 bin/，自动注入版本号）
make build

# 2. 启动云端组件（默认监听 8080 端口）
./bin/cloudcore
# 开发时也可直接运行：go run ./cmd/cloudcore

# 3. 验证健康检查（另开一个终端执行）
curl http://127.0.0.1:8080/healthz
# 期望返回 HTTP 200 和 JSON，例如：
# {"status":"ok","version":{"version":"v0.1.0","gitCommit":"...","buildTime":"...","goVersion":"go1.26.2"}}

# 4. 查看版本信息
./bin/cloudcore --version

# 5. 运行单元测试（含竞态检测与覆盖率）
make test

# 6. 静态检查
make lint
```

### 配置监听端口

`cloudcore` 的监听端口按以下优先级确定：

1. 命令行参数：`./bin/cloudcore --port 9090`
2. 环境变量：`EDGEFLOW_CLOUDCORE_PORT=9090 ./bin/cloudcore`
3. 默认值：`8080`

### 优雅退出

启动后按 `Ctrl+C`（SIGINT）或执行 `kill -TERM <进程号>`（SIGTERM），cloudcore 会停止接收新请求，等待正在处理的请求完成（最多 5 秒）后退出。

手动运行边缘端占位程序：

```bash
go run ./cmd/edgecore
```

## 部署（M4 前为骨架）

> ⚠️ 当前 Chart 为**骨架**：引用的镜像是占位地址 `edgeflow/cloudcore:v0.1.0`，**镜像尚未构建**。镜像构建与推送是后续任务（M4 之后），构建完成后替换 `build/charts/edgeflow/values.yaml` 中的 `cloudcore.image.repository` 即可。

### Chart 结构

```
build/charts/edgeflow/
├── Chart.yaml                  # Chart 元信息（name/version/appVersion）
├── values.yaml                 # 可配置项：镜像、副本数、端口、探针、资源（含中文注释）
├── .helmignore                 # 打包忽略清单
└── templates/
    ├── _helpers.tpl            # 标签辅助模板（name/fullname/labels）
    ├── cloudcore-deployment.yaml  # Deployment：/healthz 健康检查、资源限制
    ├── cloudcore-service.yaml     # Service：ClusterIP，端口 8080
    └── NOTES.txt               # 部署后的使用提示
```

### 使用方法

```bash
# 1. 校验 Chart（需要 helm，安装：brew install helm）
make helm-lint
# 或：helm lint build/charts/edgeflow/

# 2. 本地渲染预览（不部署，仅查看生成清单）
helm template edgeflow build/charts/edgeflow/

# 3. 部署到集群（镜像构建并推送后执行）
helm install edgeflow build/charts/edgeflow/ \
  --set cloudcore.image.repository=<镜像仓库地址>

# 4. 验证
kubectl get deploy,svc -l app.kubernetes.io/instance=edgeflow
kubectl port-forward svc/edgeflow-cloudcore 8080:8080
curl http://127.0.0.1:8080/healthz
```

### 交叉编译（为边缘节点产出生效二进制）

```bash
make cross-build
# 产物：dist/cloudcore-linux-amd64 / cloudcore-linux-arm64 / edgecore-linux-amd64 / edgecore-linux-arm64
```

## License

TODO: 待补充开源协议。
