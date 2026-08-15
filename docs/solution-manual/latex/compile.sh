#!/bin/bash
# =====================================================================
# EdgeFlow 解决方案手册 LaTeX 编译脚本
# 用法：bash compile.sh        （在 latex/ 目录下执行）
# 依赖：TeX Live 2025+（xelatex + ctexbook 宏集）
# 字体：PingFang SC（macOS）；其他平台请修改 main.tex 中的字体设置
# 图片：../diagrams/*.png（6 张）
# 产物：main.pdf（重命名为 EdgeFlow-解决方案手册-v1.0.0.pdf）
# 编译：循环至多 4 遍，直到目录/交叉引用稳定（无 Label(s) may have changed）
# =====================================================================
set -e
cd "$(dirname "$0")"

compile_once() {
  xelatex -interaction=nonstopmode -halt-on-error main.tex >/dev/null 2>&1 || {
    echo "编译失败，查看 main.log 定位错误"; exit 1; }
}

for i in 1 2 3 4; do
  echo "[${i}/4] xelatex 编译..."
  compile_once
  if ! grep -q "Label(s) may have changed" main.log; then
    break
  fi
  if [ "$i" = 4 ]; then
    echo "⚠️ 4 遍后仍有 Label 变化警告，继续使用当前产物（目录页码可能偏差）"
  fi
done

if [ -f main.pdf ]; then
  cp main.pdf EdgeFlow-解决方案手册-v1.0.0.pdf
  echo "✅ 编译成功：EdgeFlow-解决方案手册-v1.0.0.pdf（$(python3 -c "import fitz;print(fitz.open('main.pdf').page_count)" 2>/dev/null || echo '?') 页）"
  # 清理中间文件（保留 .tex/.log 便于排查）
  rm -f main.aux main.toc main.out main.lof main.lot
else
  echo "❌ 未生成 PDF，请检查 main.log"; exit 1
fi
