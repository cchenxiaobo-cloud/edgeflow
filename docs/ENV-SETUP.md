# EdgeFlow 开发环境配置指南

> 适用平台：macOS（Apple Silicon, arm64）
> 项目：EdgeFlow 边缘计算（Go 语言）
> 最后更新：2026-08-13

## 1. 环境要求

| 依赖 | 版本要求 | 验收命令 |
|---|---|---|
| macOS | 26.x (arm64) | `sw_vers -productVersion && uname -m` |
| Go | ≥ 1.26 | `go version` |
| Homebrew | ≥ 5.x | `brew --version` |
| Git | ≥ 2.50 | `git --version` |
| Docker | ≥ 29.x（运行容器化组件时） | `docker --version` |
| Make | 3.81 | `make --version` |

本机已具备：Go 1.26.2 / Git 2.50.1 / Docker 29.4.3 / Make 3.81 / Homebrew 5.1.11 / Node v24.12.0。
缺失并在本指南中补齐：**golangci-lint、Delve(dlv)、VS Code（可选）、SSH key、GOPROXY 国内加速**。

## 2. 快速开始（推荐）

```bash
cd /Users/mac/Documents/edgeflow
chmod +x setup-env.sh
./setup-env.sh                 # 完整安装（含 VS Code，耗时较长）
# 或跳过 VS Code：
SKIP_VSCODE=1 ./setup-env.sh
```

脚本**幂等**：重复执行不会重复安装、不会报错；某步失败修复后可直接重跑。

### 一键验收

```bash
go env GOPROXY                    # 期望: https://goproxy.cn,direct
command -v golangci-lint && golangci-lint version
command -v dlv && dlv version
ls -l ~/.ssh/id_ed25519.pub       # SSH 公钥存在
command -v code && code --version # 装了 VS Code 才有输出
```

## 3. 分步说明（脚本做的事 + 手动等价命令 + 验收）

### 3.1 GOPROXY 国内加速

```bash
go env -w GOPROXY=https://goproxy.cn,direct
```

- 原因：官方源 `proxy.golang.org` 在国内访问不稳定，`goproxy.cn`（七牛）为国内镜像。
- 验收：`go env GOPROXY` 输出 `https://goproxy.cn,direct`。
- 影响范围：仅 Go 模块下载；持久化在 `$(go env GOENV)` 配置文件中，不改动系统文件。

### 3.2 GOPATH/bin 加入 PATH

```bash
echo 'export PATH="$(go env GOPATH)/bin:$PATH"' >> ~/.zshrc
```

- 原因：`go install` 安装的工具（如 dlv）默认落在 `$(go env GOPATH)/bin`（本机为 `/Users/mac/go/bin`），该目录不在 PATH 中。
- 验收：**新开终端**后执行 `command -v dlv` 能输出路径；当前终端可用 `export PATH="$(go env GOPATH)/bin:$PATH"` 临时生效。

### 3.3 golangci-lint（静态检查）

```bash
brew install golangci-lint
```

- 验收：`golangci-lint version`（或 `golangci-lint version --format short`）。
- 使用：在项目根目录执行 `golangci-lint run ./...`。

### 3.4 Delve（Go 调试器）

```bash
go install github.com/go-delve/delve/cmd/dlv@latest
# 若 go install 失败（网络等），可退回:
# brew install delve
```

- 验收：`dlv version`。
- 使用：VS Code 调试（F5）会自动调用 dlv；命令行可用 `dlv debug ./cmd/...`。
- 注意：dlv 与 Go 版本强相关，Go 升级后建议重装 `go install ...@latest`。

### 3.5 SSH key（GitHub 认证）

```bash
ssh-keygen -t ed25519 -N "" -f ~/.ssh/id_ed25519 -C "edgeflow-$(whoami)@$(hostname)"
```

- 验收：`ls -l ~/.ssh/id_ed25519.pub` 存在；`ssh-keygen -lf ~/.ssh/id_ed25519` 能显示指纹。
- 公钥粘贴到 GitHub 后验证：`ssh -T git@github.com`，看到 `Hi <username>! You've successfully authenticated` 即成功。

### 3.6 VS Code（可选，但强烈建议）

```bash
brew install --cask visual-studio-code
```

- 验收：`code --version`；或打开 "Visual Studio Code.app"。
- 建议安装扩展：`Go`（golang.go，含 dlv 调试集成）、`golangci-lint`（golangci.golangci-lint）。
- 调试配置：项目 `.vscode/launch.json` 使用 `"type": "go"`，`"request": "launch"`，`"program": "${workspaceFolder}/cmd/..."`。

## 4. 常见问题（FAQ）

### 4.1 GOPROXY 相关：`go get` 超时 / dial tcp: i/o timeout

1. 确认代理：`go env GOPROXY` 应为 `https://goproxy.cn,direct`。
2. 手动设置：`go env -w GOPROXY=https://goproxy.cn,direct`。
3. 仍超时：检查网络（`curl -I https://goproxy.cn`）；公司网络可能需要走代理：
   `go env -w GOPROXY=https://goproxy.cn,direct HTTPS_PROXY=http://127.0.0.1:7890`（示例，按实际代理端口改）。

### 4.2 SSH 认证失败：`Permission denied (publickey)` / `ssh: connect to host github.com port 22: Operation timed out`

1. 公钥未添加：复制 `pbcopy < ~/.ssh/id_ed25519.pub`，粘贴到 GitHub → Settings → SSH and GPG keys → New SSH key → 保存。
2. 端口 22 被墙：改用 443 端口（在 `~/.ssh/config` 添加）：
   ```
   Host github.com
     HostName ssh.github.com
     Port 443
     User git
   ```
3. 验证：`ssh -T git@github.com` → `Hi <username>! You've successfully authenticated...`。
4. 多 key 时指定：`ssh -i ~/.ssh/id_ed25519 -T git@github.com`。
5. 确认 git 用户信息（提交需要）：
   ```bash
   git config --global user.name "Your Name"
   git config --global user.email "you@example.com"
   ```

### 4.3 Delve 断点不生效 / `could not launch process: exec: "lldb-server"` / 调试时崩溃

1. **版本不匹配**：dlv 必须与 Go 版本兼容。升级 Go 后执行 `go install github.com/go-delve/delve/cmd/dlv@latest` 重装。
2. **PATH 问题**：VS Code 提示找不到 dlv → 确认新开终端后 `command -v dlv` 有输出；`"dlvPath"` 未配置时 VS Code 按 PATH 查找。
3. **断点不命中（被优化/内联）**：编译优化导致断点失效：
   - VS Code `launch.json` 中加 `"buildFlags": "-gcflags=\"all=-N -l\""`（禁用优化与内联，仅调试时使用）；
   - 或命令行：`dlv debug -- -gcflags="all=-N -l"`。
4. **macOS 权限**：首次调试如提示开发者工具，执行 `xcode-select --install` 或到 系统设置 → 隐私与安全性 → 开发者工具 中授权终端/VS Code。
5. **仍失败**：`dlv version` 确认版本，查看 VS Code 输出面板 (View → Output → Go) 中的真实错误。

### 4.4 golangci-lint 运行缓慢 / 报错

- 首次运行需下载/缓存 linters，属正常现象。
- 只检查变更文件可加快：`golangci-lint run --new-from-rev=HEAD~1`。
- 版本问题：`brew upgrade golangci-lint`。

### 4.5 VS Code 安装失败（cask 下载超时）

```bash
# 重试（brew 断点续传）：
brew install --cask visual-studio-code
# 或从官网手动下载安装：https://code.visualstudio.com
```

## 5. 完整验收清单

```bash
echo "== Go ==";                 go version
echo "== GOPROXY ==";            go env GOPROXY
echo "== golangci-lint ==";      golangci-lint version --format short 2>/dev/null || golangci-lint version
echo "== dlv ==";                dlv version
echo "== SSH key ==";            ls -l ~/.ssh/id_ed25519.pub
echo "== VS Code ==";            code --version 2>/dev/null | head -1 || echo "(未安装，可选)"
echo "== GitHub SSH ==";         ssh -T -o ConnectTimeout=5 git@github.com 2>&1 | head -1
echo "== 项目编译 ==";           cd /Users/mac/Documents/edgeflow && make build
```

全部通过即环境就绪。若有失败项，重新执行 `./setup-env.sh`（幂等，安全）。
