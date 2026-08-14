#!/bin/bash
# =====================================================================
# EdgeFlow 解决方案手册 LaTeX 编译脚本
# 用法：bash compile.sh        （在 latex/ 目录下执行）
# 依赖：TeX Live 2025+（xelatex + ctexbook 宏集）
# 字体：PingFang SC（macOS）；其他平台请修改 main.tex 中的字体设置
# 图片：../diagrams/*.png（6 张）
# 产物：main.pdf（重命名为 EdgeFlow-解决方案手册-v1.0.0.pdf）
# =====================================================================
set -e
cd "$(dirname "$0")"

echo "[1/2] 第一遍编译（生成目录）..."
xelatex -interaction=nonstopmode -halt-on-error main.tex >/dev/null 2>&1 || {
  echo "编译失败，查看 main.log 定位错误"; exit 1; }

echo "[2/2] 第二遍编译（解析目录/引用）..."
xelatex -interaction=nonstopmode -halt-on-error main.tex

if [ -f main.pdf ]; then
  cp main.pdf EdgeFlow-解决方案手册-v1.0.0.pdf
  echo "✅ 编译成功：EdgeFlow-解决方案手册-v1.0.0.pdf"
  # 清理中间文件（保留 .tex/.log 便于排查）
  rm -f main.aux main.toc main.out main.lof main.lot
else
  echo "❌ 未生成 PDF，请检查 main.log"; exit 1
fi
