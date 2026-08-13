#!/usr/bin/env bash
# =============================================================================
# EdgeFlow 边缘计算项目 — 开发环境配置脚本（macOS / Apple Silicon）
#
# 用法:
#   ./setup-env.sh             # 完整配置（含 VS Code，可能耗时）
#   SKIP_VSCODE=1 ./setup-env.sh   # 跳过 VS Code 安装
#
# 特性:
#   - 幂等: 重复执行不报错、不重复安装（每步都有存在性检查）
#   - 每步带验收检查，结束时输出汇总
#   - VS Code 为可选项，安装失败不阻塞其他步骤
#
# 覆盖内容:
#   1. GOPROXY  -> https://goproxy.cn,direct（国内加速）
#   2. PATH     -> 将 $(go env GOPATH)/bin 加入 ~/.zshrc
#   3. golangci-lint -> brew install golangci-lint
#   4. Delve(dlv)    -> go install github.com/go-delve/delve/cmd/dlv@latest
#   5. SSH key  -> ~/.ssh/id_ed25519（不存在时生成，无密码）
#   6. VS Code  -> brew install --cask visual-studio-code（可选）
#   7. GitHub SSH 连通性自检（仅提示，不阻塞）
# =============================================================================

set -euo pipefail

SKIP_VSCODE="${SKIP_VSCODE:-0}"

# ----------------------------------------------------------------------------
# 工具函数
# ----------------------------------------------------------------------------
C_GREEN='\033[1;32m'; C_YELLOW='\033[1;33m'; C_RED='\033[1;31m'; C_BLUE='\033[1;36m'; C_OFF='\033[0m'
log()  { printf "${C_GREEN}[setup]${C_OFF} %s\n" "$*"; }
info() { printf "${C_BLUE}[info]${C_OFF} %s\n" "$*"; }
warn() { printf "${C_YELLOW}[warn]${C_OFF} %s\n" "$*"; }
step() { printf "\n${C_BLUE}==> %s${C_OFF}\n" "$*"; }

# 汇总计数
PASS=0; SKIP=0; FAIL=0; FAILED_ITEMS=()

# record <name> <0|1|2>  (0=ok 1=skip 2=fail)
record() {
  case "$2" in
    0) PASS=$((PASS+1)); printf "    ${C_GREEN}✔${C_OFF} %s\n" "$1";;
    1) SKIP=$((SKIP+1)); printf "    ${C_YELLOW}➤${C_OFF} %s (已配置，跳过)\n" "$1";;
    2) FAIL=$((FAIL+1)); FAILED_ITEMS+=("$1"); printf "    ${C_RED}✘${C_OFF} %s\n" "$1";;
  esac
}

# ----------------------------------------------------------------------------
# 前置检查
# ----------------------------------------------------------------------------
OS="$(uname -s)"
if [[ "$OS" != "Darwin" ]]; then
  echo "${C_RED}[FAIL]${C_OFF} 本脚本仅支持 macOS（当前: $OS）" >&2
  exit 1
fi
command -v go >/dev/null 2>&1 || { echo "${C_RED}[FAIL]${C_OFF} 未找到 go，请先安装 Go (https://go.dev/dl)" >&2; exit 1; }
command -v brew >/dev/null 2>&1 || { echo "${C_RED}[FAIL]${C_OFF} 未找到 Homebrew，请先安装 (https://brew.sh)" >&2; exit 1; }

log "EdgeFlow 环境配置开始 (macOS $(sw_vers -productVersion), $(uname -m))"

# ----------------------------------------------------------------------------
# 1. GOPROXY
# ----------------------------------------------------------------------------
step "1/7 配置 GOPROXY (goproxy.cn)"
TARGET_GOPROXY="https://goproxy.cn,direct"
CURRENT_GOPROXY="$(go env GOPROXY)"
if [[ "$CURRENT_GOPROXY" == "$TARGET_GOPROXY" ]]; then
  record "GOPROXY=$CURRENT_GOPROXY" 1
else
  go env -w GOPROXY="$TARGET_GOPROXY"
  if [[ "$(go env GOPROXY)" == "$TARGET_GOPROXY" ]]; then
    record "GOPROXY -> $TARGET_GOPROXY" 0
  else
    record "GOPROXY 写入失败" 2
  fi
fi

# ----------------------------------------------------------------------------
# 2. PATH: GOPATH/bin 加入 ~/.zshrc
# ----------------------------------------------------------------------------
step "2/7 确保 GOPATH/bin 在 PATH 中 (dlv 等工具入口)"
GOBIN_DIR="$(go env GOPATH)/bin"
if [[ ":$PATH:" == *":$GOBIN_DIR:"* ]]; then
  record "PATH 已包含 $GOBIN_DIR" 1
else
  ZSHRC="$HOME/.zshrc"
  LINE="export PATH=\"$GOBIN_DIR:\$PATH\""
  if [[ -f "$ZSHRC" ]] && grep -qF "$GOBIN_DIR" "$ZSHRC" 2>/dev/null; then
    record "~/.zshrc 已包含 $GOBIN_DIR（新终端生效）" 1
  else
    printf '\n# EdgeFlow setup: Go binaries\n%s\n' "$LINE" >> "$ZSHRC"
    record "已写入 ~/.zshrc（新开终端生效）" 0
  fi
  info "当前终端可直接使用: export PATH=\"$GOBIN_DIR:\$PATH\""
fi

# ----------------------------------------------------------------------------
# 3. golangci-lint
# ----------------------------------------------------------------------------
step "3/7 安装 golangci-lint (静态检查)"
if command -v golangci-lint >/dev/null 2>&1; then
  record "golangci-lint $(golangci-lint version 2>/dev/null | head -1)" 1
else
  if brew install golangci-lint >/tmp/setup-env-golangci.log 2>&1; then
    record "golangci-lint $(golangci-lint version 2>/dev/null | head -1)" 0
  else
    record "golangci-lint 安装失败（详见 /tmp/setup-env-golangci.log）" 2
  fi
fi

# ----------------------------------------------------------------------------
# 4. Delve (dlv)
# ----------------------------------------------------------------------------
step "4/7 安装 Delve (Go 调试器)"
DLV_BIN="$GOBIN_DIR/dlv"
if command -v dlv >/dev/null 2>&1; then
  record "dlv $(dlv version 2>/dev/null | head -1)" 1
elif [[ -x "$DLV_BIN" ]]; then
  record "dlv $( "$DLV_BIN" version 2>/dev/null | head -1)（位于 GOPATH/bin，新开终端生效）" 1
else
  info "go install 可能需要 1~3 分钟（编译）..."
  if go install github.com/go-delve/delve/cmd/dlv@latest >/tmp/setup-env-dlv.log 2>&1; then
    export PATH="$GOBIN_DIR:$PATH"
    if command -v dlv >/dev/null 2>&1; then
      record "dlv $(dlv version 2>/dev/null | head -1)" 0
    else
      record "dlv 已编译但不在 PATH（新开终端后可用）" 0
    fi
  elif brew install delve >/tmp/setup-env-dlv-brew.log 2>&1; then
    record "dlv (brew) $(dlv version 2>/dev/null | head -1)" 0
  else
    record "dlv 安装失败（go install 与 brew 均失败，详见 /tmp/setup-env-dlv*.log）" 2
  fi
fi

# ----------------------------------------------------------------------------
# 5. SSH key
# ----------------------------------------------------------------------------
step "5/7 生成 SSH key (GitHub 认证)"
if compgen -G "$HOME/.ssh/id_ed25519" >/dev/null || compgen -G "$HOME/.ssh/id_rsa" >/dev/null; then
  EXISTING="$HOME/.ssh/id_ed25519"; [[ -f "$HOME/.ssh/id_ed25519" ]] || EXISTING="$HOME/.ssh/id_rsa"
  record "SSH key 已存在: $EXISTING" 1
else
  mkdir -p "$HOME/.ssh" && chmod 700 "$HOME/.ssh"
  ssh-keygen -t ed25519 -N "" -C "edgeflow-$(whoami)@$(hostname)" -f "$HOME/.ssh/id_ed25519" >/dev/null
  chmod 600 "$HOME/.ssh/id_ed25519"
  if [[ -f "$HOME/.ssh/id_ed25519" ]]; then
    record "已生成 $HOME/.ssh/id_ed25519" 0
    info "公钥: $(cat "$HOME/.ssh/id_ed25519.pub")"
    info "复制公钥并粘贴到 GitHub: pbcopy < $HOME/.ssh/id_ed25519.pub"
  else
    record "SSH key 生成失败" 2
  fi
fi

# ----------------------------------------------------------------------------
# 6. VS Code (可选)
# ----------------------------------------------------------------------------
step "6/7 安装 VS Code (可选)"
if [[ "$SKIP_VSCODE" == "1" ]]; then
  record "VS Code（已通过 SKIP_VSCODE=1 跳过）" 1
elif command -v code >/dev/null 2>&1; then
  record "VS Code $(code --version 2>/dev/null | head -1)" 1
else
  info "brew install --cask visual-studio-code 下载约 150MB，可能需要几分钟..."
  if brew install --cask visual-studio-code >/tmp/setup-env-vscode.log 2>&1; then
    record "VS Code $(code --version 2>/dev/null | head -1)" 0
  else
    warn "VS Code 安装失败（不影响其他工具）。日志: /tmp/setup-env-vscode.log"
    warn "可稍后手动执行: brew install --cask visual-studio-code"
    record "VS Code 安装失败（可选项）" 2
  fi
fi

# ----------------------------------------------------------------------------
# 7. GitHub SSH 连通性自检（仅提示）
# ----------------------------------------------------------------------------
step "7/7 GitHub SSH 连通性自检（提示性，不阻塞）"
if [[ -f "$HOME/.ssh/id_ed25519" ]] || [[ -f "$HOME/.ssh/id_rsa" ]]; then
  set +e
  GITHUB_OUT="$(ssh -o StrictHostKeyChecking=accept-new -o ConnectTimeout=5 -T git@github.com 2>&1)"
  SSH_RC=$?
  set -e
  if [[ "$SSH_RC" == 0 ]] || echo "$GITHUB_OUT" | grep -qi "successfully authenticated"; then
    record "GitHub SSH 认证成功" 0
  else
    warn "GitHub SSH 尚未认证（正常，需先粘贴公钥到 GitHub，见 docs/ENV-SETUP.md）"
    warn "提示信息: $(echo "$GITHUB_OUT" | head -1)"
    record "GitHub SSH 认证（待用户在 GitHub 添加公钥）" 1
  fi
else
  record "无 SSH key，跳过自检" 1
fi

# ----------------------------------------------------------------------------
# 汇总
# ----------------------------------------------------------------------------
echo
echo "=================================================="
echo " EdgeFlow 环境配置完成: 通过 $PASS / 跳过 $SKIP / 失败 $FAIL"
echo "=================================================="
if [[ "$FAIL" -gt 0 ]]; then
  printf "${C_RED}失败项:${C_OFF}\n"
  for item in "${FAILED_ITEMS[@]}"; do printf "  - %s\n" "$item"; done
  echo "提示: 脚本幂等，修复问题后直接重新执行 ./setup-env.sh 即可"
  exit 1
fi
echo "全部就绪。新开终端后执行 make build 验证项目编译。"
