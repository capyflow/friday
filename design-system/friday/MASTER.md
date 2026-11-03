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
   localStorage `friday-theme` 持久化（无键 = 跟随系统，`initTheme` 挂 `prefers-color-scheme`
   监听实时跟随）；index.html 头部内联预设防首帧闪烁。两个入口（侧栏账号菜单的快捷切换、
   设置 → 外观的三档）共用同一个值，改动一律经 `theme.js` 的 `setTheme`/`clearTheme`
   发出 `friday:theme` 广播，别在组件里各自持有主题状态。
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

**深色浮层两条硬规则**（`--card` 是半透明面，浮层直接拿来用会透出下层内容）：

- 弹窗 / 抽屉 / 悬浮卡深色下统一 `background: rgba(24,24,24,.94)` +
  `backdrop-filter: blur(28px) saturate(1.4)` + 边框 `rgba(255,255,255,.1)`
  （`.modal` / `.set-dialog` / `.approval-card` / `.user-pop` 都是这一套，遮罩浅色 `rgba(24,24,27,.45)`、深色 `rgba(0,0,0,.6)`）。
- 实心 `--primary` 上的前景一律用 `--on-primary`；**没有 `--primary-fg` 这个令牌**，
  写 `var(--primary-fg, #fff)` 会在深色下变成亮灰底白字（看不见）。

---

## Component Specs（均已在 web/src/components 对应实现）

- **按钮** — `.btn` 描边 / `.primary` 墨（深色亮灰反白）/ `.danger`；表格内 `.sm`
- **下拉框** — 一律用 `components/SelectBox.vue`，不写原生 `<select>`（原生控件的展开列表由
  操作系统渲染，深浅两套主题都盖不住）。选项 `[{ value, label, disabled? }]`，value 可以是数字
  （验证超时档位就是）；停用的项照列出来但选不中，键盘上下键直接跨过它——已停用的 agent 藏着
  不如标出来。`id` 透传到触发按钮（`<label for>` 才连得上），`class` 落在外层包装上（它才是布局盒）；
  换档要顺手做点别的（比如切回免密钥档就清掉 API Key），用 `:model-value` + `@update:model-value`
  自己收口，没有 `@change`。弹层 Teleport 到 body、`z-index: 240`，压得过最上层的文件抽屉（228），
  滚动或缩放即收起。两档尺寸：默认与 `input` 同节奏（9px 12px / `--border` / `--radius-sm`），
  `compact` 给表格与抽屉行内
- **欢迎态** — 标题逐字浮现（GSAP stagger）、副标题打字机轮播、
  建议 chips（点击直接发送）；logo 呼吸光晕已移除
- **Qoder 风 composer** — `.inputbar` 描边卡片（圆角 16px；浅色白卡 + `--shadow-xs`，
  深色半透明面 + 细边；`:focus-within` 起 3px 克制蓝聚焦环）。两行：textarea 独占第一行
  （autoGrow 到 160px 后内部滚动），第二行 `.composer-bar` 左为圆形加号（旋转 45° 开弹窗）、
  权限 chip、模式 chip 与上下文 chip，右为快捷键提示（窄屏隐藏）与圆形发送键（空输入禁用，
  回复中切暂停/继续）
- **上下文 chip + 弹层** — `.ctx-btn` 与权限 chip 同形（药丸 + 15px 描边图标 + `tabular-nums` 数字），
  三档配色：未达阈值 `--muted-fg`、达软阈值 `.warm` 转 `--warn`、逼近硬阈值 `.hot` 转 `--danger-fg`
  （深色各配一档亮色）。`.ctx-popover` 复用 `.popover` 玻璃层与 `popIn` 编排：占比条 `.ctx-meter`
  在软阈值处放一根刻度 `.ctx-mark`（看得见"离自动归档还有多远"），下面两行 `.ctx-line` 给
  活跃区/窗口 token 与块数，动作项沿用 `.pop-item`；压缩项的说明要讲清为什么置灰，
  用 `.ctx-item-text` + `.ctx-tip` 单独占一行而不是 `.pd` 的省略号尾巴。三个弹层（加号/权限/上下文）互斥
- **加号弹窗** — `.popover` 玻璃圆角 18px：添加图片 / 计划模式 / 目标模式（互斥选中，
  切 placeholder + 发送时加 `【计划】/【目标】` 前缀）/ 快捷创建（定时提醒、新建待办 → 预填输入框）
- **聊天气泡** — 用户=primary（深色下 `#262626` 中性面）、错误=红浅底（bot 不再套气泡）；MD 排版（代码块墨底）
- **bot 回答时间线**（zcode 风）— `.turn` 内按 LLM 轮次排列（一轮里思考与正文各只占一块，
  分片交错到达也并回同一块），思考与工具调用同为裸行，
  前导共用 14px 描边 SVG 槽 `.step-ico`（`flex` 居中、跟随 `currentColor`，两行标签左边缘因此对齐，
  展开区 `.think-body` / `.tool-body` 统一缩进 21px）：思考块 `.think` = 灯泡（流式时 `.live`
  只淡入淡出呼吸、颜色转 `--primary`，定稿后回落 `--faint-fg`）+ 一行实时预览/轮次摘要，
  点击展开全文；工具调用行 `.tool` 的展示口径**按来源分两套**：Friday 自己 agent 的调用
  （ChatView 那条链路，step.kind=tool）查 `src/tool-catalog.js`，给中文名（`delete_todo` →
  删除待办）与按动作归类的 14px 描边图标（文档 / 加号文档 / 铅笔 / 文件夹 / 放大镜 / 终端 /
  勾框 / 本子 / 时钟 / 云 / 数据库 / 插头 / 喇叭 / 地球 / 扳手），我们挂出去的 MCP 工具
  （`mcp_<服务器>_<工具>`，见 internal/mcp/manager.go 的 ToolPrefix）说成「MCP · 服务器」；
  ACP 会话页是外部 agent 给的名字（codex 的 Bash、它也有自己的 read_file），改不动也不替它
  翻译——`TimelineBlocks` 的 `ownTools` prop 决定走不走目录，不传就照直显示原始工具名 +
  默认扳手，名字缺失退回「工具调用」，绝不空行。目录只收自己人，`tests/tool-catalog.test.js`
  拿 `internal/**/*.go` 对账，工具改了名而这里没跟上会被抓到。运行中前导换成旋转环 `.step-ring`，
  仅失败时换告警圈并整行转红（`.tool.is-fail` 配 `--danger-fg`/`--danger`，`title` 把
  「工具执行出错」说在前面），定稿后才回落成工具自己的图标（成功不打勾）；点开才给原始工具名、
  参数与等宽结果——收起态要回答的是「哪个工具」，不是「调了几次工具」；
  连续多次工具调用归成一叠 `.tool-stack`：收起只报「执行工具
  N 次」，点开才逐条列出（每条仍是可再展开的 `ToolStep`，展开区同样缩进 21px）。失败不许藏进
  展开区——有失败时文案补「· M 次出错」、整叠转红、前导换告警圈；运行中跟前导换 `.step-ring`
  转环。只调用一次不套壳，直接摆一行，别让人多点一下。叠的开合记在这一叠第一个块的 `gopen` 上
  （后续到达的块挤不掉用户已经点开的状态）；箭头一律写 `> .tool-head .think-chev` 直选，
  写成后代选择器会把叠里每条工具行的箭头一起转过去；
  正文全宽 `.chat-md`；流式光标 `cursor` 跟本轮正在写的正文块。该轮末尾一行 `.turn-total` 收口本轮
  用量（11.5px `--faint-fg` + `tabular-nums`，「本轮 · 1.2k+300 tokens · 耗时 3.4s」）——
  token 不摊在每行思考上，只在轮末说一次；刷新后由历史接口按轮带回，每轮各留自己那一行
- **表格** — 无外框、细线行、hover 浅底；徽章/chip 全部浅底深字
- **状态卡** — 数字 GSAP 滚动（`data-count` + runCountUp），非数值 15px
- **弹层三件套** — toast（墨色/深色炭灰玻璃）、modal、审批卡（左橙线玻璃卡）
- **空状态插画** — `components/art/EmptyState.vue`：单色细线，与 24 网格线性图标同族（viewBox 120 /
  stroke 1.5），按 `kind` 取帧（trace / todo / wiki / wikiPick / notif / mcp / usage / session /
  skill / schedule / error）+ 插槽文案。约束：一帧一个主体名词、accent 只落在引导元素上、
  虚线恒表示「尚未发生」、断开处用留白而非 fill 挖洞（深色 `--card` 半透明，挖洞会漏线）。
  `tests/empty-art.test.js` 校验 kind 与各页调用点对齐
- **设置弹窗** — 左侧分组导航（模型与人格 / 能力与行为 / 运行观测 / 数据与系统）+ 右侧内容；
  `.set-pane` 默认 640px，运行状态 / 调用轨迹 / 记忆这类数据密集页标 `wide` 放开到内容区宽度。
  侧栏只留聊天 / 自动化 / 闪念 / 通知，观测与记忆页收进设置；`store.settingsSection` 做一次性深链定位
  （自动化「查看执行轮次」→ 调用轨迹并带会话过滤，过滤 chip 放 `.trace-bar` 行内而非页头）
- **表单弹窗** — `.dlg-mask / .dlg / .dlg-head / .dlg-body / .dlg-foot`（自动化页新建与编辑任务）：
  头部标题 + ✕，正文为可滚动的表单栅格，底部一行放错误文案与右对齐的「取消 / 确认」。
  遮罩、✕、Esc 与「取消」同义（清空草稿后收起），只有确认成功才带着数据关闭；提交失败留在弹窗内。
  z-index 206，低于 `.selbox-pop`（240）以免弹窗里的下拉被压住
- **能力卡片（`.svc-*`，style.css 共用外壳）** — 设置页「一项能力一张卡」：36px 图标槽 +
  名称与状态徽章 + 一行摘要，右侧「配置」展开编辑区、再点「收起」。展开用 `v-show`（表单常驻
  DOM，改到一半不丢输入）。已在用：设置 → 工具（联网搜索）、设置 → MCP 服务器（每台一张）。
  徽章走语义令牌 `.ok/.run/.bad`（浅底深字，深色换半透明底），别写死色值；摘要只说「是哪一台」，
  整条命令/地址进 `title`，展开才看全——同工作区列表不铺路径的取舍。
  **头部只留一个「配置」开关**：改这件东西本身的动作（保存、取消、重连、删除、授权）一律收在
  展开的表单里，头部不摊一排按钮
- **设置页的写动作** — 保存即生效的能力（MCP、工具）不再摆整页的「保存并生效」：每张卡自带
  「保存 / 取消」，保存只提交这一张（其余按上次保存的值原样带上，别张卡没保存的草稿不会被裹进去），
  取消回到上次保存的值并收起；新增走弹窗表单（`.dlg` 骨架 + 取消/保存），不往列表里塞空行。
  字段定义抽在 `settings/McpFields.vue`，用 `v-model:row` 交草稿——卡片展开态与弹窗共用一份，
  落盘时机归调用方
- **模型 chip + 弹层** — 与上下文 chip 同形（`.ctx-btn model-btn`，模型名 110px 截断），
  点开列模型库（当前项 `.selected`、仅文本/支持图片与窗口标注），点选即 `POST /api/provider/activate`
  热切换；「管理模型库…」经 `store.settingsSection='model'` 深链进设置。设置里的模型库行卡片
  `.pm-row` 收成一行摘要（radio + 名称 + model·host + `图片`/`密钥` 徽标 + 生效中 + hover 显现的
  测试/删除 + chevron），点击展开才是两列编辑表单——列表态保持安静，表单细节不摊在大面上。
  激活条目 `inputs` 不含 image 时，加号弹窗「添加图片」置灰并说明，粘贴图片同样被拒
- **快捷面板 palette** — 深色炭灰同源令牌（亮灰 logo / 蓝光标）

---

## Anti-Patterns (Do NOT Use)

- ❌ 硬投影 / 点阵纹理 / 荧光黄章（v4 遗产，已全部移除）
- ❌ 深色下用纯白卡片（必须半透明面 + 细边令牌）
- ❌ 紫色渐变 / 发光 / 扫光 / 光斑（「AI 紫」风味，2026-10-14 全部移除；深色保持无色相）
- ❌ 用原生 `<select>` 做下拉（展开列表归操作系统画，描边风格与深色主题都盖不住）——
  `style.css` 里那条 `select { appearance: none }` 只是防漏网的兜底，不是许可
- ❌ 语义标签用整块饱和大色底
- ❌ 用 ✓/✕ 文本字符当**块身份图标**（时间线折叠行的前导必须用 14px 描边 SVG + `currentColor`，
  字符的字号与基线随平台字体度量漂移，且无法与描边图标同槽对齐）；纯状态枚举列可以留字符
  ——计划卡 `.pc-state` 的 `·…✓✕−`、下拉选中项的 `::after` 勾，但颜色一律走 `--ok`/`--danger` 令牌
- ❌ 忽略 `prefers-reduced-motion`（GSAP 编排必须可降级）
- ❌ 引入 CDN 字体/脚本（保持单文件离线；GSAP 走 npm 打包）

---

## Pre-Delivery Checklist

- [ ] 双主题均过对比度（深色 body 自动跟踪）
- [ ] 主题切换后 theme-color meta 同步
- [ ] `npm run build` → `internal/admin/webdist/` → 重启 Go 服务（embed 编译期固化）
- [ ] GSAP 动效在 reduced-motion 下为静态
- [ ] 760px 断点：顶栏化、审批卡贴边、悬浮窗 fixed 逃逸滚动容器
