# Design System Master File

> **LOGIC:** When building a specific page, first check `design-system/pages/[page-name].md`.
> If that file exists, its rules **override** this Master file.
> If not, strictly follow the rules below.

---

**Project:** Friday（小五 · 个人生活管家）
**Generated:** 2026-09-30（v5 现代简约双主题，基于 `design/modern-ui-preview` 分支样例落地；替代 v4 粗野 / v3 线条 / v2 暖调 / v1 AI 紫）
**Revised:** 2026-10-14（深色去紫改版：移除靛紫渐变 / 极光光斑 / 发光扫光，参考 Cursor 改为中性炭灰 + 克制蓝点缀）
**Category:** Personal AI Assistant / Consumer Productivity
**样例基准：** 远端分支 `design/modern-ui-preview`（design-system/friday/ui-preview.html，9 屏 + GSAP）

---

## 设计方向（Design Direction）

**Quiet SaaS（浅）/ Quiet Carbon（深）双主题**：中性锌灰色阶主导，墨色为行动色，
靛蓝小面积点缀；克制圆角（8-12px）、极轻中性阴影、浅底深字的柔和标签；
深色为中性炭灰（参考 Cursor）：无色相表面 + 细线边框，不用渐变、光斑与光晕。

1. **双主题** — `html[data-theme="light|dark"]` 令牌覆盖；默认跟随系统，
   localStorage `friday-theme` 持久化；index.html 头部内联预设防首帧闪烁。
2. **背景** — 全站 fixed 点阵纹理层；紫色极光光斑已下线（旧「AI 风」的主要来源）。
3. **行动色** — 浅色为墨 `#18181B`；深色为亮灰反白 `#E8E8E8`（主按钮/发送键/品牌标，
   与浅色墨色互为反相）；用户气泡深色下为 `#262626` 细边中性面。
4. **交互** — 160ms cubic-bezier(.4,0,.2,1)；按钮 hover 变底色不加位移；
   发送键 active scale(.9)；GSAP：外壳入场/欢迎逐字/视图切换错峰/数字滚动/气泡弹出，
   `prefers-reduced-motion` 全部降级。
5. **语义标签一律「浅底深字」** — blue/warn/teal/cyan/accent subtle 底 + 同色深字；
   深色下换半透明同色底。点缀色深色下为克制蓝 `#6EA8F2`（链接/选中/聚焦）。

---

## Color Palette（完整令牌见 web/src/style.css :root 与 html[data-theme="dark"]）

| Role | Light | Dark |
|------|-------|------|
| bg | `#F6F6F8` | `#141414` |
| sidebar / card | `#FFFFFF` | `rgba(255,255,255,.025/.045)`（半透明面） |
| fg / fg-2 / muted / faint | `#18181B / #3F3F46 / #71717A / #A1A1AA` | `#E8E8E8 / #C2C2C2 / #949494 / #616161` |
| border / strong | `#E9E9ED / #D6D6DC` | `rgba(255,255,255,.08/.17)` |
| primary（行动） | `#18181B`（墨） | `#E8E8E8`（亮灰反白，on-primary `#141414`） |
| accent（点缀） | `#4338CA` | `#6EA8F2`（克制蓝） |
| danger / ok / warn / blue / teal / cyan | 语义色 + `-subtle` 浅底 | 深色换亮色 + 半透明底 |

阴影四档 `--shadow-xs/sm/md/overlay`（深色加重）；圆角 `12/8/6px`。

---

## Component Specs（均已在 web/src/components 对应实现）

- **按钮** — `.btn` 描边 / `.primary` 墨（深色亮灰反白）/ `.danger`；表格内 `.sm`
- **欢迎态** — 标题逐字浮现（GSAP stagger）、副标题打字机轮播、
  建议 chips（点击直接发送）；logo 呼吸光晕已移除
- **Qoder 风 composer** — `.inputbar` 描边卡片（圆角 16px；浅色白卡 + `--shadow-xs`，
  深色半透明面 + 细边；`:focus-within` 起 3px 克制蓝聚焦环）。两行：textarea 独占第一行
  （autoGrow 到 160px 后内部滚动），第二行 `.composer-bar` 左为圆形加号（旋转 45° 开弹窗）
  与模式 chip、右为快捷键提示（窄屏隐藏）与圆形发送键（空输入禁用，回复中切暂停/继续）
- **加号弹窗** — `.popover` 玻璃圆角 18px：添加图片 / 计划模式 / 目标模式（互斥选中，
  切 placeholder + 发送时加 `【计划】/【目标】` 前缀）/ 快捷创建（定时提醒、新建待办 → 预填输入框）
- **聊天气泡** — 用户=primary（深色下 `#262626` 中性面）、错误=红浅底（bot 不再套气泡）；MD 排版（代码块墨底）
- **bot 回答时间线**（zcode 风）— `.turn` 内按到达顺序排列：思考块 `.think`（裸行：状态点 +
  一行实时预览/轮次摘要，点击展开全文，展开区 `.think-body` 才是盒）、工具调用行 `.tool`
  （✓/✕/旋转环 + 统一文案「工具调用」，点击展开工具名、参数与等宽结果）、正文全宽 `.chat-md`；
  流式光标 `cursor` 跟最后一块正文
- **表格** — 无外框、细线行、hover 浅底；徽章/chip 全部浅底深字
- **状态卡** — 数字 GSAP 滚动（`data-count` + runCountUp），非数值 15px
- **弹层三件套** — toast（墨色/深色炭灰玻璃）、modal、审批卡（左橙线玻璃卡）
- **快捷面板 palette** — 深色炭灰同源令牌（亮灰 logo / 蓝光标）

---

## Anti-Patterns (Do NOT Use)

- ❌ 硬投影 / 点阵纹理 / 荧光黄章（v4 遗产，已全部移除）
- ❌ 深色下用纯白卡片（必须半透明面 + 细边令牌）
- ❌ 紫色渐变 / 发光 / 扫光 / 光斑（「AI 紫」风味，2026-10-14 全部移除；深色保持无色相）
- ❌ 语义标签用整块饱和大色底
- ❌ 忽略 `prefers-reduced-motion`（GSAP 编排必须可降级）
- ❌ 引入 CDN 字体/脚本（保持单文件离线；GSAP 走 npm 打包）

---

## Pre-Delivery Checklist

- [ ] 双主题均过对比度（深色 body 自动跟踪）
- [ ] 主题切换后 theme-color meta 同步
- [ ] `npm run build` → `internal/admin/webdist/` → 重启 Go 服务（embed 编译期固化）
- [ ] GSAP 动效在 reduced-motion 下为静态
- [ ] 760px 断点：顶栏化、审批卡贴边、悬浮窗 fixed 逃逸滚动容器
