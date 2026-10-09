# Design System Master File

> **LOGIC:** When building a specific page, first check `design-system/pages/[page-name].md`.
> If that file exists, its rules **override** this Master file.
> If not, strictly follow the rules below.

---

**Project:** Friday（小五 · 个人生活管家）
**Generated:** 2026-09-30（v5 现代简约双主题，基于 `design/modern-ui-preview` 分支样例落地；替代 v4 粗野 / v3 线条 / v2 暖调 / v1 AI 紫）
**Category:** Personal AI Assistant / Consumer Productivity
**样例基准：** 远端分支 `design/modern-ui-preview`（design-system/friday/ui-preview.html，9 屏 + GSAP）

---

## 设计方向（Design Direction）

**Quiet SaaS（浅）/ Quiet Glass（深）双主题**：中性锌灰色阶主导，墨色为行动色，
靛蓝小面积点缀；克制圆角（8-12px）、极轻中性阴影、浅底深字的柔和标签；
深色下升级为极光玻璃（磨砂卡片 + 极光光斑 + 靛紫渐变行动色）。

1. **双主题** — `html[data-theme="light|dark"]` 令牌覆盖；默认跟随系统，
   localStorage `friday-theme` 持久化；index.html 头部内联预设防首帧闪烁。
2. **极光背景** — 全站 fixed 光斑层（blur 120px × 3 团：靛/青/紫），深色 opacity .20
   可见 + GSAP 有机漂移，浅色 opacity 0 仅点阵纹理。
3. **行动色** — 浅色为墨 `#18181B`；深色为靛紫渐变 `linear-gradient(135deg,#6366F1,#7C3AED)`
   （主按钮/用户气泡/发送键/品牌标），带 inset 高光与投影。
4. **交互** — 160ms cubic-bezier(.4,0,.2,1)；按钮 hover 变底色不加位移；
   发送键 active scale(.9)；GSAP：外壳入场/欢迎逐字/视图切换错峰/数字滚动/气泡弹出，
   `prefers-reduced-motion` 全部降级。
5. **语义标签一律「浅底深字」** — blue/warn/teal/cyan/accent subtle 底 + 同色深字；
   深色下换半透明同色底。

---

## Color Palette（完整令牌见 web/src/style.css :root 与 html[data-theme="dark"]）

| Role | Light | Dark |
|------|-------|------|
| bg | `#F6F6F8` | `#0A0A10` |
| sidebar / card | `#FFFFFF` | `rgba(255,255,255,.025/.045)`（玻璃） |
| fg / fg-2 / muted / faint | `#18181B / #3F3F46 / #71717A / #A1A1AA` | `#ECECF2 / #C5C6D2 / #8E90A0 / #606270` |
| border / strong | `#E9E9ED / #D6D6DC` | `rgba(255,255,255,.08/.17)` |
| primary（行动） | `#18181B` | `#6D6BF2`（hover 渐变） |
| accent（点缀） | `#4338CA` | `#A5A2FF` |
| danger / ok / warn / blue / teal / cyan | 语义色 + `-subtle` 浅底 | 深色换亮色 + 半透明底 |

阴影四档 `--shadow-xs/sm/md/overlay`（深色加重）；圆角 `12/8/6px`。

---

## Component Specs（均已在 web/src/components 对应实现）

- **按钮** — `.btn` 描边 / `.primary` 墨（深色渐变+扫光）/ `.danger`；表格内 `.sm`
- **欢迎态** — welcome-logo 呼吸光晕、标题逐字浮现（GSAP stagger）、副标题打字机轮播、
  建议 chips（点击直接发送）
- **Codex 风 composer** — `.inputbar` 大圆角胶囊（深色玻璃+靛紫 focus 光晕）、
  圆形加号（旋转 45° 开弹窗）、圆形发送键（空输入禁用）
- **加号弹窗** — `.popover` 玻璃圆角 18px：添加图片 / 计划模式 / 目标模式（互斥选中，
  切 placeholder + 发送时加 `【计划】/【目标】` 前缀）/ 快捷创建（定时提醒、新建待办 → 预填输入框）
- **聊天气泡** — 用户=primary、bot=白卡/玻璃、错误=红浅底；MD 排版（代码块墨底）
- **表格** — 无外框、细线行、hover 浅底；徽章/chip 全部浅底深字
- **状态卡** — 数字 GSAP 滚动（`data-count` + runCountUp），非数值 15px
- **弹层三件套** — toast（墨色/深色玻璃）、modal、审批卡（左橙线玻璃卡）
- **快捷面板 palette** — 深色玻璃同源令牌（靛紫 logo/光标）

---

## Anti-Patterns (Do NOT Use)

- ❌ 硬投影 / 点阵纹理 / 荧光黄章（v4 遗产，已全部移除）
- ❌ 深色下用纯白卡片（必须玻璃 + 半透明令牌）
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
