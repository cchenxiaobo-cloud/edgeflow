# EdgeFlow 用户手册（LaTeX 工程）

本目录是《EdgeFlow 用户手册 v0.1.0》的 LaTeX 源码工程，纳入 Git 版本管理，
随产品迭代更新。

## 目录结构

```
docs/manual/
├── main.tex          # 主文件（文档类、样式、封面、目录、引入章节）
├── chapters/
│   ├── chapter1.tex  # 第1章 产品概述
│   ├── chapter2.tex  # 第2章 环境要求与安装部署
│   ├── chapter3.tex  # 第3章 快速入门
│   ├── chapter4.tex  # 第4章 云端操作指南
│   ├── chapter5.tex  # 第5章 边缘节点与自治运行
│   ├── chapter6.tex  # 第6章 设备接入
│   ├── chapter7.tex  # 第7章 升级与回滚
│   ├── chapter8.tex  # 第8章 安全与认证
│   ├── chapter9.tex  # 第9章 常见问题与故障排查
│   └── appendix.tex  # 附录 A-E
└── README.md         # 本文件
```

## 编译方式

依赖：XeLaTeX + ctex 宏包（TeX Live 2023+ 或 MacTeX 完整安装即可）。

```bash
cd docs/manual
xelatex main.tex        # 第 1 遍
xelatex main.tex        # 第 2 遍（生成目录/交叉引用）
# 或使用 latexmk 自动多遍：
latexmk -xelatex main.tex
```

产物：`main.pdf`（重命名为 `EdgeFlow-用户手册-v0.1.0.pdf` 发布）。

## 更新方式

1. 修改对应章节 `chapters/*.tex`（保持 ctexbook 章节结构）；
2. 更新版本信息页（版本号、编制日期）；
3. 新增功能时在附录 D 增加变更记录条目，并按"已实现 / 即将上线"口径同步；
4. 重新编译，将 PDF 与源码一并提交。

## 内容口径（重要）

- 只描述 v0.1.0 已实现功能；未实现功能一律标注"即将上线"或列入附录 E；
- 命令、参数、环境变量均与代码实现一致（见 docs/API-SPEC.md、docs/KEADM.md）；
- 修改后请用 `xelatex` 编译验证无致命错误再提交。
