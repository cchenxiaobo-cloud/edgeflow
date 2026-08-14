# EdgeFlow 解决方案手册 v1.0.0 — LaTeX 编译说明与交接

## 1. 目录结构

```
latex/
├── main.tex                      # 主文件：文档类、样式、封面、目录、\input 各章
├── compile.sh                    # 一键编译脚本（两遍 xelatex + 产物改名 + 清理中间文件）
├── chapters/
│   ├── ch01-overview.tex         # 第 1 章 方案总览
│   ├── ch02-collect.tex          # 第 2 章 物联网数据采集场景
│   ├── ch03-weaknet.tex          # 第 3 章 弱网自治场景
│   ├── ch04-model-mgmt.tex       # 第 4 章 模型管理场景
│   ├── ch05-model-app.tex        # 第 5 章 模型应用场景
│   ├── ch06-dataflow.tex         # 第 6 章 数据全链路与台账管理
│   └── appendix.tex              # 附录 A FAQ / B 术语表 / C 映射表 / D 版本管理
└── EdgeFlow-解决方案手册-v1.0.0.pdf   # 编译产物（37 页）
```

## 2. 编译方法

```bash
cd latex/
bash compile.sh          # 或手动：xelatex -interaction=nonstopmode main.tex × 2 遍
```

**依赖**：TeX Live 2025+（xelatex 引擎），宏包：ctexbook、xeCJK（ctex 自带）、
fontspec、booktabs、longtable、tabularx、array、tcolorbox、listings、seqsplit、
fancyhdr、titlesec、enumitem、hyperref、geometry、xcolor、caption。

**字体**：中文 PingFang SC、西文 Helvetica Neue（macOS 系统字体）。
其他平台（Linux/Windows）请在 `main.tex` 中替换字体设置，例如：

```latex
\setCJKmainfont{Noto Sans CJK SC}
\setsansfont{Noto Sans}
```

**图片**：`../diagrams/*.png`（相对 latex/ 目录），6 张，见第 3 节清单。

## 3. 图片资源清单（6 张，均已嵌入 PDF）

| 图号 | 文件名（PNG / SVG 源） | 插入章节 | PDF 页 | tex 引用位置 |
|---|---|---|---|---|
| 图 1-1 | arch-overview.png / arch-overview.svg | 1.2 整体架构 | 6 | ch01-overview.tex |
| 图 2-1 | scenario-collect.png / scenario-collect.svg | 2.4 部署方式 | 11 | ch02-collect.tex |
| 图 3-1 | scenario-weaknet.png / scenario-weaknet.svg | 3.4 部署方式 | 15 | ch03-weaknet.tex |
| 图 4-1 | scenario-model-mgmt.png / scenario-model-mgmt.svg | 4.4 部署方式 | 21 | ch04-model-mgmt.tex |
| 图 5-1 | scenario-model-app.png / scenario-model-app.svg | 5.4 部署方式 | 25 | ch05-model-app.tex |
| 图 6-1 | dataflow.png / dataflow.svg | 6.1 数据流转全景 | 27 | ch06-dataflow.tex |

- PNG 为 1600×1600 渲染图（qlmanage 从 SVG 渲染），插入宽度 0.94\textwidth，比例未拉伸；
- SVG 为可编辑源文件（Inkscape/Figma 可直接打开），改图后重新渲染 PNG 再编译即可。

## 4. 修改指南

| 想改什么 | 改哪里 |
|---|---|
| 正文/表格内容 | 对应章节 tex 文件（如第 3 章改 ch03-weaknet.tex） |
| 封面信息/声明 | main.tex 的 titlepage 环境 |
| 修订记录 | main.tex 修订记录表 |
| 字体/颜色/页面边距 | main.tex 的 preamble（fontspec / xcolor / geometry） |
| 图片 | diagrams/ 下 SVG 改后渲染 PNG，或直接替换 PNG（保持同名） |
| 新增章节 | 复制章节 tex 模板，在 main.tex 的 \input 处添加 |

## 5. 版本管理与回滚

- **原稿备份**：内容基线为 `docs/solution-manual/EdgeFlow-解决方案手册-v1.0.0.md`（Git 提交 65aff1a 起已入库），本次 LaTeX 版为同版本衍生制品；
- **本次提交**：LaTeX 源 + PDF 以新 Git 提交入库，可随时 `git checkout` 回退；
- **回滚口径**：若新 PDF 不被认可，回退到上一 Git 提交即可恢复 Markdown+HTML 交付状态，无需从零找回素材；
- **升级流程**：产品新版本发布后，先对照附录 C 特性清单核验状态，再升版（v1.1.0…）并同步更新修订记录表。

## 6. 已知说明（编译警告）

- 编译日志无 error；PDF 全部文本均在页面安全边距内（bbox 校验 0 越界）。
- 存在两类无害警告：
  1. xeCJK 重定义 CJKfamily（ctex 默认字体族声明，不影响输出）、fontspec 字体族提示；
  2. 少量 Overfull hbox 警告（行盒略超列宽，最大约 50pt，均发生在表格内长环境变量名/API 路径断行处；
     已通过固定列宽 + seqsplit 断词处理，视觉上无越界无截断，仅日志提示）。
- 表格排版说明：全部表格采用固定 p 列宽 + 3pt 列间距 + booktabs 三线表；长 token（环境变量名、API 路径）
  通过 \env 命令（ttfamily + seqsplit）按字符断行，避免英文连字符断词（如 de-vices）。
