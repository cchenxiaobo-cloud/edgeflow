#!/usr/bin/env bash
# 重新导出 diagrams SVG → PNG（等比渲染，修复旧版非等比拉伸导致的文字裁切/变形）
# 方式：rsvg-convert（librsvg，严格按 viewBox 等比渲染，支持系统字体）
# 依赖：brew install librsvg
# 用法：bash hack/re-export-diagrams.sh   （仓库根目录执行）
set -euo pipefail
cd "$(dirname "$0")/../docs/solution-manual/diagrams"

command -v rsvg-convert >/dev/null 2>&1 || {
  echo "❌ 需要 librsvg：brew install librsvg"; exit 1; }

for svg in *.svg; do
  name="${svg%.svg}"
  # 按 viewBox 比例导出，最长边 1600px（等比，内容完整无裁切）
  rsvg-convert -w 1600 "$svg" -o "${name}.png"
  python3 -c "
from PIL import Image
im = Image.open('${name}.png')
print(f'${name}.png: {im.size[0]}x{im.size[1]}（等比）')
"
done
echo "✅ 6 张 PNG 重导出完成"
