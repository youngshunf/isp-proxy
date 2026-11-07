---
name: ISP 代理（占位名）
description: 用户前台：每个 IP 订阅是一张 SIM 卡，卡面是出口，卡托是套餐余量
colors:
  tray: "#eef1f0"
  tray-2: "#e3e8e6"
  card: "#ffffff"
  ink: "#14231f"
  ink-2: "#43524d"
  ink-3: "#5c6b66"
  line: "#d2dad7"
  line-2: "#e4e9e7"
  control-line: "#c3cdc9"
  row-hover: "#f5f8f7"
  teal: "#00505c"
  teal-2: "#006a7a"
  teal-tint: "#e3eef0"
  teal-tint-2: "#cde2e5"
  ok: "#1d7a4f"
  warn: "#8f5500"
  warn-fill: "#c48219"
  warn-tint: "#fbf1de"
  stop: "#b3261e"
  stop-tint: "#faeae8"
typography:
  headline:
    fontFamily: "'Barlow', 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei UI', 'Microsoft YaHei', 'Noto Sans CJK SC', 'Source Han Sans SC', system-ui, sans-serif"
    fontSize: "28px"
    fontWeight: 600
    lineHeight: 1.25
    letterSpacing: "0"
  title:
    fontFamily: "'Barlow', 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei UI', 'Microsoft YaHei', 'Noto Sans CJK SC', 'Source Han Sans SC', system-ui, sans-serif"
    fontSize: "17px"
    fontWeight: 600
    lineHeight: 1.4
  title-card:
    fontFamily: "'Barlow', 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei UI', 'Microsoft YaHei', 'Noto Sans CJK SC', 'Source Han Sans SC', system-ui, sans-serif"
    fontSize: "16px"
    fontWeight: 600
    lineHeight: 1.35
  body:
    fontFamily: "'Barlow', 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei UI', 'Microsoft YaHei', 'Noto Sans CJK SC', 'Source Han Sans SC', system-ui, sans-serif"
    fontSize: "14px"
    fontWeight: 400
    lineHeight: 1.6
    fontFeature: "'tnum' 1"
  label:
    fontFamily: "'Barlow', 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei UI', 'Microsoft YaHei', 'Noto Sans CJK SC', 'Source Han Sans SC', system-ui, sans-serif"
    fontSize: "13px"
    fontWeight: 400
    lineHeight: 1.45
  readout:
    fontFamily: "'Barlow', 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei UI', 'Microsoft YaHei', 'Noto Sans CJK SC', 'Source Han Sans SC', system-ui, sans-serif"
    fontSize: "18px"
    fontWeight: 600
    fontFeature: "'tnum' 1"
  mono-display:
    fontFamily: "'Red Hat Mono', ui-monospace, 'SF Mono', Menlo, Consolas, monospace"
    fontSize: "26px"
    fontWeight: 500
    lineHeight: 1.2
    letterSpacing: "-0.01em"
  mono:
    fontFamily: "'Red Hat Mono', ui-monospace, 'SF Mono', Menlo, Consolas, monospace"
    fontSize: "13px"
    fontWeight: 400
    letterSpacing: "0"
rounded:
  control: "2px"
  plate: "0px"
  chamfer: "22px"
  chamfer-sm: "14px"
spacing:
  xs: "6px"
  sm: "10px"
  md: "14px"
  plate-x: "22px"
  plate-x-mobile: "18px"
  gap: "24px"
  page-top: "36px"
  page-max: "1320px"
components:
  button-primary:
    backgroundColor: "{colors.teal}"
    textColor: "{colors.card}"
    rounded: "{rounded.control}"
    height: "36px"
  button-primary-hover:
    backgroundColor: "{colors.teal-2}"
  button-default:
    backgroundColor: "{colors.card}"
    textColor: "{colors.ink}"
    rounded: "{rounded.control}"
    height: "36px"
  plate:
    backgroundColor: "{colors.card}"
    rounded: "{rounded.plate}"
    padding: "16px 22px 22px"
  state-cell:
    backgroundColor: "{colors.card}"
    typography: "{typography.label}"
    height: "34px"
    padding: "0 12px"
  meter-track:
    backgroundColor: "{colors.tray-2}"
    height: "6px"
  meter-fill:
    backgroundColor: "{colors.teal}"
    height: "6px"
  protocol-copy-button:
    backgroundColor: "{colors.card}"
    textColor: "{colors.ink}"
    rounded: "{rounded.control}"
    height: "32px"
    padding: "0 11px"
  protocol-copy-button-hover:
    backgroundColor: "{colors.teal-tint}"
    textColor: "{colors.teal}"
  protocol-copy-button-copied:
    backgroundColor: "{colors.teal}"
    textColor: "{colors.card}"
  conn-row:
    backgroundColor: "{colors.tray}"
    typography: "{typography.mono}"
    padding: "8px 10px 8px 12px"
  status-cell:
    backgroundColor: "{colors.card}"
    height: "26px"
  choice:
    backgroundColor: "{colors.card}"
    textColor: "{colors.ink}"
    rounded: "{rounded.control}"
    padding: "10px 14px"
  choice-selected:
    backgroundColor: "{colors.teal-tint}"
  nav-link-active:
    textColor: "{colors.ink}"
    height: "60px"
---

# Design System: ISP 代理（占位名）

## Overview

**Creative North Star: "SIM 卡与套餐余量"**

每个 IP 订阅是一张 SIM 卡：卡面印着出口（大号等宽 IP、地区、信号格），卡托装着套餐余量（剩余时长、剩余流量两条横条），卡脚是一排协议复制钮。补卡（换接入）不换号，换号（换出口）不丢余量——视觉上卡面与卡托分区，正对应这条产品事实。页面在冷灰卡托底（`tray`）上铺白色卡体，右上角 45° 切角是唯一的形体签名；切角旁的固定格用实心/空心/半实心圆点报告状态。

密度是"运营台面"而非"控制台"：桌面 1440 宽三列卡片网格，没有左侧导航、没有统计卡、没有代理大表。颜色极克制：深墨绿文字，运营商深青承担所有操作与主读数，卡面左上角是出口所在国家的国旗。数字一律等宽（全局 `tnum`），IP、端口、连接串用 Red Hat Mono。深度只靠两层很轻的 drop-shadow 与边线，没有渐变装饰、没有圆角卡片。

本文件记录的是已交付的用户前台（`web/apps/portal`）。管理后台将复用同一组 token，但有自己的布局；卡片网格与 SIM 卡结构属于前台，不是后台的默认排法。

**Key Characteristics:**
- 冷灰卡托底 + 纯白卡体 + 右上 45° 切角（22px，小号 14px）
- 状态 = 切角旁固定格 + 形状编码圆点（实心成立 / 空心不成立 / 半实心处理中），不只靠颜色
- 余量 = "剩余 / 总量"横条，条长表示剩余而非已用
- 深青是唯一操作色；卡面识别靠国旗，不用装饰色
- 全局等宽数字；IP、端口、订单号、连接串用等宽字体
- 控件近乎直角（2px），按钮无投影

## Colors

冷而克制的运营商配色：一种操作色、一种卡面金属色，其余全部是带绿味的冷灰阶与三档语义色。

### Primary
- **运营商深青 Carrier Teal**（teal）：主按钮、链接、导航激活下划线、余量条填充、焦点环、已复制状态、"处理中/等待"语义。是页面上唯一可以点的颜色。
- **浅深青 Teal Lift**（teal-2）：链接与主按钮悬停、选项卡悬停描边。
- **深青淡底 Teal Wash**（teal-tint / teal-tint-2）：选中行、选中选项、复制钮悬停、确认条底色、换接入后的变更高亮、地区代码格；teal-tint-2 作淡底上的描边与选区色。

### Secondary
- **国旗 Country Flag**：卡面识别标记，来自 flag-icons（MIT）4:3 SVG，按出口国家（未开通时按套餐国家）显示；40×30（表格内 28×21），3px 圆角，内描边 1px `rgb(20 35 31 / 0.16)`，保证白底国旗在白卡上有边界；未知国家显示两位国家代码。

### Neutral
- **卡托冷灰 Tray Grey**（tray）：页面底、连接串行底、键名格底、顶栏半透明底（94%）。
- **卡槽灰 Slot Grey**（tray-2）：余量条空轨。
- **卡体白 Card White**（card）：所有切角容器、状态格、复制钮、底部标签栏。
- **深墨绿 Deep Ink**（ink）：正文与读数主色，也是顶部原型标识条底色。
- **次墨 Ink 2 / 三墨 Ink 3**（ink-2 / ink-3）：说明文字 / 元信息、单位、键名、停用读数。
- **边线 Line / 细分线 Line 2**（line / line-2）：切角容器的 1px 外框、状态格边 / 卡内分区线、列表行分隔。
- **控件描边 Control Line**（control-line）：antd 输入框与默认按钮描边，比 line 深一档以保证控件边界可辨。
- **行悬停 Row Hover**（row-hover）：订单行、规格表行悬停底，antd `colorFillAlter`。

### 语义色
- **可用 OK**（ok）：可用/已确认/已完成的状态格与计数。
- **留意 Warn**（warn 文本 / warn-fill 条填充 / warn-tint 底）：即将到期、余量不足 10%、待核款。
- **阻断 Stop**（stop / stop-tint）：停用、用尽、到期、失败；阻断余量条用 stop-tint 与 #f3d4d1 的 -45° 斜纹空轨。

### Named Rules
**The Flag Identity Rule.** 卡面左上角只放出口国家的国旗，用户一眼按国家认卡；国旗是卡面唯一的多彩元素，品牌标、按钮、高亮、价格都不得引入额外装饰色；品牌标里的芯片是白色。

**The One Operator Rule.** 可操作的东西只用深青。语义色（ok / warn / stop）只报告状态，不做按钮底色；破坏性操作走确认框，影响写在 stop 色提示条里。

## Typography

**Display Font:** 无独立展示字体
**Body Font:** Barlow（400/500/600，经 @fontsource 自托管），CJK 回落 PingFang SC → Hiragino Sans GB → Microsoft YaHei → Noto Sans CJK SC → Source Han Sans SC → system-ui
**Label/Mono Font:** Red Hat Mono（400/500），回落 ui-monospace、SF Mono、Menlo、Consolas

**Character:** Barlow 偏窄、略带工业铭牌感的拉丁字形配系统中文黑体，像运营商卡面印字；Red Hat Mono 只给机器要读的字符串，让 IP 和连接串一眼可辨、可逐位核对。

### Hierarchy
- **Headline**（600, 28px, 1.25；手机 24px；登录页 24px）：页标题"我的 IP""订单"等，每页一个。
- **Title**（600, 17px, 1.4）：切角容器分区标题（接入、记录、出口检测与更换）。
- **Title card**（600, 16px, 1.35）：SIM 卡名称，单行省略。
- **Body**（400, 14px, 1.6，全局 `tnum`）：正文与表格。
- **Label**（400, 13px）：余量标签、元信息、说明（plate-note）、状态格文字（600）。小号 12px 只给信号格延迟、标签片与"新"标。
- **Readout**（600, 18px）：余量读数中的主数字；单位与总量降为 13px ink-3。订单合计 26px/600。
- **Mono display**（500, 26px, -0.01em；详情页卡面 32px，手机 24px）：卡面出口 IP。
- **Mono**（400, 13–15px）：连接串、端口、订单号、订阅编号；付款识别码 30px/500，字距 0.12em。

### Named Rules
**The Tabular Rule.** 全局开启 `tnum`；余量、价格、时间、计数一律等宽数字，`.mono` 元素关闭 `tnum` 改用真正的等宽字体。

**The Machine String Rule.** 等宽字体只给 IP、端口、主机名、用户名、密码、连接串、订单/订阅编号——用户要复制或逐位核对的东西。标题与说明永远用 Barlow。

## Layout

- **容器：** 页面最大宽 1320px，左右 24px；页面上 36px、下 72px。顶栏 60px 粘性，半透明卡托底加 10px 模糊。顶部固定一条 28px 深墨原型标识条（示意数据声明）。
- **页头：** 标题 + 一行状态计数（形状圆点 + 数字）左对齐，主按钮右侧底对齐。其下可有一条白底"留意条"（到期、低流量、待核款）。
- **我的 IP：** SIM 卡网格 3 列（≤1180px 2 列，≤720px 1 列），列间距 24px（手机 16px）。卡片等高，卡托 `flex: 1` 撑满，复制钮贴底。
- **详情页：** 5fr / 7fr 两栏，左栏是卡面+余量与记录，右栏是接入与出口；≤1180px 单栏。出口只在卡面上出现一次。
- **购买页：** 左侧规格表 + 右侧 380px 粘性订单摘要（top 84px）；≤1180px 单栏并取消粘性。
- **列表页（订单、申请）：** 一个切角容器内的行列表，行间 line-2 分隔；订单行桌面 5 列网格，手机两列堆叠。
- **间距节奏：** 容器内边距 22px（手机 18px）；行内间距 6 / 10 / 14px；分区之间 24px。
- **手机（≤720px）：** 顶部导航隐藏，改为底部 60px 白色标签栏（5 项，图标 20px + 11px 字）并让出安全区；页头纵向堆叠，主按钮满宽；协议复制钮满宽均分、高 40px。≤480px 时键值表键名换到值上方，接入连接串换行显示（`overflow-wrap: anywhere`）。

## Elevation & Depth

近乎平面。深度由三层表达：卡托灰底 → 白色卡体（1px line 外框）→ 卡内用 line-2 分线和 tray 色凹槽（连接串行、键名格、余量空轨）。阴影只有两档、用 `filter: drop-shadow` 实现，因为切角容器用 clip-path 裁形，box-shadow 会被裁掉。

### Shadow Vocabulary
- **卡体静置**（`filter: drop-shadow(0 1px 1px rgb(20 35 31 / 0.07)) drop-shadow(0 6px 14px rgb(20 35 31 / 0.06))`）：所有切角容器。
- **卡体悬停**（`filter: drop-shadow(0 2px 2px rgb(20 35 31 / 0.08)) drop-shadow(0 10px 22px rgb(20 35 31 / 0.1))`）：仅可点击的 SIM 卡，240ms `cubic-bezier(0.16, 1, 0.3, 1)`。

### Named Rules
**The Drop-Shadow Rule.** 切角容器的阴影加在外包裹层的 `filter` 上，不加在被裁形的元素本身；按钮一律无投影（antd primaryShadow/defaultShadow 均为 none）。

## Shapes

形体语言只有两种：一级容器是右上角 45° 切角的矩形（22px；次级小容器 14px），其余一切是近直角（2px）。切角由 clip-path 实现：外层 1px line 色衬底 + 内层白体同形裁切，形成连续的 1px 描边。状态格贴在切角左侧、顶边对齐，左下两边描线，像卡面上的固定印格。品牌标是同一切角形（7px 切角）的深青小卡，内嵌白色芯片。国旗是唯一的小圆角元素（3px）。

**The One Notch Rule.** 每个一级分区是一张切角卡，切角永远在右上；卡内不再嵌套卡片，内部分组用分线或 tray 凹槽。

## Components

### Buttons
近直角、无投影、500 字重，像设备上的实体键。
- **Shape:** 2px 圆角，高 36px（antd `controlHeight`）。
- **Primary:** 深青底白字；每屏一个主动作（"购买 IP"、"我已更新程序"）。卡托内的补购钮（续时长、买流量）在阻断时用主色小号，正常时用默认小号。
- **Hover / Focus:** 悬停转 teal-2；全局焦点环 2px 深青实线、外偏 2px；输入框聚焦 `0 0 0 3px rgba(0,80,92,0.14)`。
- **Default:** 白底、control-line 描边、墨色字；表格内复制等次要动作用 text 型小号按钮。

### Chips
- **状态格（cell）：** 26px 高，左侧 tray 底键名（"收款"，以及"开通"或"入账"）+ 右侧形状圆点与 600 字状态，文字色按 tone；订单把收款状态与交付状态分成两格。
- **标签片（chip）：** 24px 高、12px 字、1px 描边，warn/stop/wait 各带对应淡底。
- **地区代码格：** 34×24，深青字、teal-tint 底，12px/600、字距 0.04em。

### Cards / Containers
- **Corner Style:** 右上 45° 切角（22px / 14px），其余直角。
- **Background:** card 白；卡托区从白到 #fbfcfc 的极淡竖向过渡。
- **Shadow Strategy:** 见 Elevation 两档 drop-shadow。
- **Border:** 1px line，经 clip-path 衬底实现。
- **Internal Padding:** 标题行 18px 22px 0（右侧留 40px 给切角），正文 16px 22px 22px；手机 18px。

### Inputs / Fields
- **Style:** antd Input，2px 圆角、control-line 描边、白底、36px 高；验证码输入用等宽 18px、字距 0.3em。
- **Focus:** 深青描边 + 3px 深青 14% 光晕。
- **选项卡（choice）：** 白底 1px line 描边、10px 14px 内边距、最小宽 132px；选中为深青描边 + 内 1px 深青描边 + teal-tint 底（`aria-pressed`）。

### Navigation
- **顶栏：** 品牌标 + 名称（16px/600）、四项主导航（15px，ink-2；激活为 ink、600、底部 2px 深青线贴到顶栏底边）、右侧账号按钮（悬停出现白底描边）。
- **手机：** 底部 5 格标签栏，未激活 ink-3，激活深青 600。

### SIM 卡（SimCard，签名组件）
卡面：左上国旗（40×30，出口国家；未开通时按套餐国家）+ 名称 + 订阅编号（等宽 `SUB-XXXX`）· 并发 · 带宽；右上切角旁状态格（34px 高、最小 92px，ok 字色白底 / stop 红字淡红底 / wait 深青字淡青底）。中部 ExitFace：等宽出口 IP、右侧信号格 + 延迟；下行地区 · 实测时间。阻断时 IP 降为 ink-3；开通中显示无衬线"出口开通中"。卡托：两条 Meter，阻断原因与补购钮直接写在对应余量条下。卡脚：协议复制组 + "详情与更换"链接。

### 余量条（Meter）
标签左、读数右（主数字 18px/600，单位与总量 13px ink-3），下方 6px 直角轨道，深青填充表示剩余。warn：填充改 warn-fill、数字 warn。stop：空轨换成 stop 斜纹、数字 stop。附注行可右挂一个补购按钮。轨道带 `role="meter"` 与完整读数。

### 状态点与信号格（marks）
StateMark 10px：实心（成立）、空心 1.6px 描边（不成立）、左半实心（处理中），颜色继承 tone。SignalBars 18×14 四格（高 4/7/10/13）：点亮格按 tone 着色（ok 用深青），未亮 #cfd8d4；检测过期时改为灰色虚线空心格。

### 协议复制组（ProtocolCopy）
HTTP / HTTPS / SOCKS5 三钮连成一组，1px line 外框、内部竖分线，32px 高、13px/500；悬停 teal-tint 底深青字，复制成功瞬间变深青实底白字并显示对勾。卡片停用时文字降为 ink-3。

### 接入面板（AccessPanel）
键值表（键名 96px、值、右侧动作列，line-2 行分隔）+ 三行连接串（tray 底、line-2 描边、协议名 64px 列 + 等宽串 + 复制钮）。换接入后：被改动的端口/密码以 teal-tint 底高亮、900ms 自左向右展开，并带深青"新"标；面板顶部出现 teal-tint 确认条，内含说明与主按钮"我已更新程序"，获焦并以 `role="status"` 播报，确认后高亮清除。

### 时间线（记录、订单进度）
16px 标记列 + 1px line 竖线串联，标题 500、元信息 13px ink-3。

## Do's and Don'ts

### Do:
- **Do** 把每个订阅画成一张切角 SIM 卡：卡面（出口）、卡托（余量）、卡脚（复制）三段，用 line-2 分线隔开。
- **Do** 用形状 + 颜色双重编码状态：实心 / 空心 / 半实心圆点，放在切角旁的固定格或带键名的状态格里。
- **Do** 把付款状态与交付状态分成两个独立状态格，"已支付""已交付""可用"永远分开表达。
- **Do** 余量写成"剩余 / 总量"，条长表示剩余；时长与流量分两条，阻断原因与对应的购买按钮写在那一条下面。
- **Do** 给 IP、端口、连接串、编号用 Red Hat Mono；其余数字保持 Barlow + `tnum`。
- **Do** 保留顶部原型标识条，直到价格、库存与 IP 不再是示意数据。
- **Do** 尊重 `prefers-reduced-motion`：动画与过渡压到 1ms。

### Don't:
- **Don't** 用 emoji 国旗（Windows 上显示为字母）或在国旗之外引入装饰色（含品牌标、高亮、价格、按钮）。
- **Don't** 把前台排成"左侧导航 + 统计卡 + 代理表格"的控制台。
- **Don't** 在切角卡里再嵌套卡片，或给一级容器加圆角。
- **Don't** 给按钮加投影，或对切角容器用 box-shadow（会被 clip-path 裁掉）。
- **Don't** 只靠颜色区分状态；每个状态都要有形状圆点和文字标签。
- **Don't** 在详情页同时出现两处出口 IP；出口只印在卡面上。
- **Don't** 用语义色（ok / warn / stop）做按钮底色。
