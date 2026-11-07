---
version: 1
slug: "web-apps-portal"
primary_target: "web/apps/portal"
related_targets: []
---

# 用户前台 portal

范围：web/apps/portal 全部客户页面（账号、概览、购买 IP、我的 IP、IP 详情、订单、申请、账号设置）。模式：Operate。

受众与任务：跨境电商/社媒运营与爬虫开发者并存。高频任务是看余量、复制连接、续时长/买流量；低频但关键的是连接泄露时换接入、申请换出口、下单后跟进人工核款。桌面白天办公为主，手机上查余量、复制、续购。

约束：权益属于订阅，接入与出口可独立更换（需求 R-04、R-11、R-18）；已支付/已交付/可用分开表达；不展示供应商信息与 GOST 配置。排斥：灰产/机场站、云厂商控制台、模板 SaaS、过度花哨。价格、库存与 IP 均为示意数据，须标注。

## Direction contract

THESIS：每个 IP 订阅是一张 SIM 卡——卡面是出口，卡下是套餐余量（时长、流量分列），补卡（换接入）不换号，换号（换出口）不丢余量。拒绝“左侧导航 + 统计卡 + 代理表格”的控制台排法。

OWN-WORLD：冷灰卡托底 #EEF1F0，卡体纯白，深墨绿文字 #14231F，运营商深青 #00505C 承担操作色，卡面左上角是出口国家的国旗（2026-11-08 用户要求替换原 SIM 金触点）。容器是右上角 45° 切角的卡片；状态在切角旁的固定格里用实心/空心/半实心圆点表示；余量是“剩余 / 总量”的横条读数，数字一律等宽；出口健康用信号格图标。系统中文字体栈，等宽字体只给 IP、端口和连接串。

STORY：用户一眼看懂每个 IP 能不能用、为什么不能用、该买哪一项；复制连接一步到位；连接泄露时自己换接入，换 IP 时提交申请并看到进度。

FIRST VIEWPORT：顶栏（品牌、我的 IP / 购买 / 订单 / 申请、账号）。标题“我的 IP”与一行状态计数，右侧主按钮“购买 IP”。下方 SIM 卡片网格（1440 宽三列）：卡面上方国旗 + 名称 + 状态格；中部大号等宽出口 IP 与地区；下方两条余量（剩余天数、剩余流量）；底部三个协议复制钮。停用卡片在余量条上标出阻断原因并给出对应购买按钮。手机为单列卡片 + 底部标签栏。

FORM：SIM 卡与套餐余量，自排候选第 1 位（作为 IMPECCABLE’S PICK 被选中），seed key a7439a18。

FINISH：unreviewed and undocumented is unfinished; this build ends with the finish review, the verdict, DESIGN.md, and every shipping raster carrying its provenance

## 未决

- 品牌名未定，原型使用占位名“ISP 代理”。
- 价格、币种、套餐规格为示意（D-04）。
