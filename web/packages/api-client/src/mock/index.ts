// 内存版 mock，仅供原型与前端测试使用。
// 所有 IP 为 RFC 5737 文档保留地址，域名为 example.com，价格与库存为示意（D-04 未决）。
// 业务判断（可用条件、配额、冲突）放在这里模拟“服务端”，前端只展示返回值。
import {
  ApiError,
  type Access,
  type Account,
  type Blocker,
  type Catalog,
  type ExitRequest,
  type ExitRequestInput,
  type Order,
  type OrderInput,
  type OrderStep,
  type PortalApi,
  type Plan,
  type Subscription,
  type SubscriptionEvent,
} from '../index'

const GB = 1_000_000_000
const DAY = 86_400_000
const HOUR = 3_600_000

/** 内存 mock 额外提供的测试钩子。 */
export interface MockPortalApi extends PortalApi {
  /** 模拟会话在服务端失效（过期或在别处改密）：之后需要登录的接口返回 401。 */
  expireSession(): void
  /** 下一次调用 op 时抛出 err（失败反馈测试）。 */
  failNext(op: keyof PortalApi, err: Error): void
  /** 各方法被调用的次数（重复提交测试）。 */
  calls: Partial<Record<keyof PortalApi, number>>
}

export interface MockOptions {
  /** 模拟网络延迟区间（毫秒），测试中传 [0, 0]。 */
  latency?: [number, number]
  /** 服务端“当前时间”，固定后截图与测试稳定。 */
  now?: number
  signedIn?: boolean
}

export const MOCK_NOW = Date.parse('2026-11-07T10:00:00+08:00')

const iso = (t: number) => new Date(t).toISOString()
const cny = (yuan: number) => ({ amount: Math.round(yuan * 100), currency: 'CNY' as const })

interface SubSeed {
  sub: Omit<Subscription, 'blockers' | 'time'> & {
    time: Omit<Subscription['time'], 'remainingSeconds'>
  }
  adminPaused?: boolean
  events: SubscriptionEvent[]
}

function seed(now: number) {
  const catalog: Catalog = {
    brand: 'ISP 代理（演示）',
    plans: [
      { id: 'us-lax', versionId: 'us-lax-v1', exitMode: 'exclusive', countryCode: 'us', country: '美国', city: '洛杉矶', monthlyPrice: cny(168), includedBytes: 50 * GB, concurrency: 50, bandwidthMbps: 20, stock: 'plenty' },
      // 共享出口套餐（D-24）：同一出口最多由 N 个订阅共用，价格更低。
      { id: 'us-lax-shared', versionId: 'us-lax-shared-v1', exitMode: 'shared', countryCode: 'us', country: '美国', city: '洛杉矶', monthlyPrice: cny(68), includedBytes: 20 * GB, concurrency: 20, bandwidthMbps: 10, stock: 'plenty' },
      { id: 'us-nyc', versionId: 'us-nyc-v1', exitMode: 'exclusive', countryCode: 'us', country: '美国', city: '纽约', monthlyPrice: cny(178), includedBytes: 50 * GB, concurrency: 50, bandwidthMbps: 20, stock: 'low' },
      { id: 'gb-lon', versionId: 'gb-lon-v1', exitMode: 'exclusive', countryCode: 'gb', country: '英国', city: '伦敦', monthlyPrice: cny(188), includedBytes: 50 * GB, concurrency: 50, bandwidthMbps: 20, stock: 'plenty' },
      { id: 'de-fra', versionId: 'de-fra-v1', exitMode: 'exclusive', countryCode: 'de', country: '德国', city: '法兰克福', monthlyPrice: cny(188), includedBytes: 50 * GB, concurrency: 50, bandwidthMbps: 20, stock: 'plenty' },
      { id: 'jp-tyo', versionId: 'jp-tyo-v1', exitMode: 'exclusive', countryCode: 'jp', country: '日本', city: '东京', monthlyPrice: cny(198), includedBytes: 100 * GB, concurrency: 80, bandwidthMbps: 30, stock: 'low' },
      { id: 'sg-sin', versionId: 'sg-sin-v1', exitMode: 'exclusive', countryCode: 'sg', country: '新加坡', city: '新加坡', monthlyPrice: cny(158), includedBytes: 50 * GB, concurrency: 50, bandwidthMbps: 20, stock: 'out' },
    ],
    trafficPacks: [
      { id: 't10', versionId: 't10-v1', bytes: 10 * GB, price: cny(18) },
      { id: 't50', versionId: 't50-v1', bytes: 50 * GB, price: cny(75) },
      { id: 't200', versionId: 't200-v1', bytes: 200 * GB, price: cny(260) },
    ],
    monthOptions: [1, 3, 6],
    rules: {
      version: 'R-2026-11-07',
      periodDays: 30,
      retentionDays: 7,
      accessChangesPerDay: 5,
      freeExitChangesPer30Days: 1,
      unpaidHoldHours: 24,
      maxUnpaidNewOrders: 3,
    },
    payment: {
      payee: '示意收款主体（原型）',
      bank: '示意银行',
      account: '0000 0000 0000 0000',
      note: '转账时在附言填写付款识别码，便于管理员核对。核款为人工处理，确认前订单不会开通。',
    },
  }

  const access = (v: number, user: string, pass: string, base: number, issued: number): Access => ({
    version: v,
    host: 'proxy.example.com',
    username: user,
    password: pass,
    ports: { http: base, https: base + 1, socks5: base + 2 },
    issuedAt: iso(issued),
    unseenChanges: [],
  })

  const subs: SubSeed[] = [
    {
      sub: {
        id: 'sub_7k2m',
        name: 'TikTok 美区 1 号',
        kind: 'isp',
        direct: null,
        plan: { name: '美国 · 洛杉矶 包月', exitMode: 'exclusive', countryCode: 'us', country: '美国', city: '洛杉矶', concurrency: 50, bandwidthMbps: 20 },
        lifecycle: 'active',
        time: { periodDays: 60, startedAt: iso(now - 37 * DAY), expiresAt: iso(now + 23 * DAY + 4 * HOUR), retentionEndsAt: null },
        traffic: { totalBytes: 100 * GB, usedBytes: 61.8 * GB },
        exit: { version: 2, ip: '203.0.113.48', sharedCapacity: null, countryCode: 'us', country: '美国', city: '洛杉矶', check: 'ok', latencyMs: 182, checkedAt: iso(now - 6 * 60_000) },
        access: access(2, 'u7k2m', 'Qm4vT9xRb2Lp', 31080, now - 18 * DAY),
        quotas: { accessChangesLeft: 5, accessChangesPerDay: 5, freeExitChangesLeft: 0, freeExitWindowEndsAt: iso(now + 12 * DAY) },
        openExitRequestId: null,
        openOrderIds: [],
      },
      events: [
        { id: 'e1', at: iso(now - 37 * DAY), kind: 'ready', detail: '开通就绪，开始计时；出口 203.0.113.9，接入 v1', orderNo: 'ORD-20261001-0012' },
        { id: 'e2', at: iso(now - 18 * DAY), kind: 'exit_changed', detail: '出口 203.0.113.9 → 203.0.113.48（申请 REQ-1020-0003）；接入未变' },
        { id: 'e3', at: iso(now - 18 * DAY + HOUR), kind: 'access_changed', detail: '接入 v1 → v2：端口与密码已更换；出口未变' },
        { id: 'e4', at: iso(now - 3 * DAY), kind: 'time_added', detail: '续时长 +30 天，到期时间顺延', orderNo: 'ORD-20261104-0031' },
        { id: 'e5', at: iso(now - 3 * DAY), kind: 'traffic_added', detail: '买流量 +50 GB', orderNo: 'ORD-20261104-0031' },
      ],
    },
    {
      // 共享出口（D-24）：用户只看到“共享 · 最多 N 人共用”，看不到同出口的其他订阅与当前人数。
      sub: {
        id: 'sub_h8sc',
        name: '账号养号 · 共享',
        kind: 'isp',
        direct: null,
        plan: { name: '美国 · 洛杉矶 共享包月', exitMode: 'shared', countryCode: 'us', country: '美国', city: '洛杉矶', concurrency: 20, bandwidthMbps: 10 },
        lifecycle: 'active',
        time: { periodDays: 30, startedAt: iso(now - 6 * DAY), expiresAt: iso(now + 24 * DAY), retentionEndsAt: null },
        traffic: { totalBytes: 20 * GB, usedBytes: 3.2 * GB },
        exit: { version: 1, ip: '203.0.113.61', sharedCapacity: 3, countryCode: 'us', country: '美国', city: '洛杉矶', check: 'ok', latencyMs: 204, checkedAt: iso(now - 5 * 60_000) },
        access: access(1, 'uh8sc', 'Rt7kD2pXw4Nm', 31580, now - 6 * DAY),
        quotas: { accessChangesLeft: 5, accessChangesPerDay: 5, freeExitChangesLeft: 1, freeExitWindowEndsAt: null },
        openExitRequestId: null,
        openOrderIds: [],
      },
      events: [
        { id: 'e1', at: iso(now - 6 * DAY), kind: 'ready', detail: '开通就绪，开始计时；共享出口 203.0.113.61，接入 v1', orderNo: 'ORD-20261101-0027' },
      ],
    },
    {
      sub: {
        id: 'sub_d3fq',
        name: '亚马逊德国店',
        kind: 'isp',
        direct: null,
        plan: { name: '德国 · 法兰克福 包月', exitMode: 'exclusive', countryCode: 'de', country: '德国', city: '法兰克福', concurrency: 50, bandwidthMbps: 20 },
        lifecycle: 'active',
        time: { periodDays: 30, startedAt: iso(now - 28 * DAY), expiresAt: iso(now + 2 * DAY + 6 * HOUR), retentionEndsAt: null },
        traffic: { totalBytes: 50 * GB, usedBytes: 46.9 * GB },
        exit: { version: 1, ip: '198.51.100.23', sharedCapacity: null, countryCode: 'de', country: '德国', city: '法兰克福', check: 'ok', latencyMs: 236, checkedAt: iso(now - 11 * 60_000) },
        access: access(1, 'ud3fq', 'Hs8nW2kZp6Ye', 31180, now - 28 * DAY),
        quotas: { accessChangesLeft: 5, accessChangesPerDay: 5, freeExitChangesLeft: 1, freeExitWindowEndsAt: null },
        openExitRequestId: null,
        openOrderIds: [],
      },
      events: [
        { id: 'e1', at: iso(now - 28 * DAY), kind: 'ready', detail: '开通就绪，开始计时；出口 198.51.100.23，接入 v1', orderNo: 'ORD-20261010-0015' },
      ],
    },
    {
      sub: {
        id: 'sub_j9tx',
        name: '采集任务 · 日本',
        kind: 'isp',
        direct: null,
        plan: { name: '日本 · 东京 包月', exitMode: 'exclusive', countryCode: 'jp', country: '日本', city: '东京', concurrency: 80, bandwidthMbps: 30 },
        lifecycle: 'active',
        time: { periodDays: 30, startedAt: iso(now - 13 * DAY), expiresAt: iso(now + 17 * DAY), retentionEndsAt: null },
        traffic: { totalBytes: 100 * GB, usedBytes: 100 * GB },
        exit: { version: 1, ip: '203.0.113.112', sharedCapacity: null, countryCode: 'jp', country: '日本', city: '东京', check: 'ok', latencyMs: 96, checkedAt: iso(now - 4 * 60_000) },
        access: access(3, 'uj9tx', 'Vb3cK7mQe5Tw', 31280, now - 2 * DAY),
        quotas: { accessChangesLeft: 3, accessChangesPerDay: 5, freeExitChangesLeft: 1, freeExitWindowEndsAt: null },
        openExitRequestId: null,
        openOrderIds: ['ord_0038'],
      },
      events: [
        { id: 'e1', at: iso(now - 13 * DAY), kind: 'ready', detail: '开通就绪，开始计时；出口 203.0.113.112，接入 v1', orderNo: 'ORD-20261025-0021' },
        { id: 'e2', at: iso(now - 2 * DAY), kind: 'access_changed', detail: '接入 v1 → v2：端口与密码已更换' },
        { id: 'e3', at: iso(now - 2 * DAY + 600_000), kind: 'password_reset', detail: '接入 v2 → v3：仅重置密码，端口不变' },
        { id: 'e4', at: iso(now - 9 * HOUR), kind: 'exhausted', detail: '流量用尽，全部连接已断开；到期时间不变' },
      ],
    },
    {
      sub: {
        id: 'sub_s4wp',
        name: 'Shopee 新加坡',
        kind: 'isp',
        direct: null,
        plan: { name: '新加坡 包月', exitMode: 'exclusive', countryCode: 'sg', country: '新加坡', city: '新加坡', concurrency: 50, bandwidthMbps: 20 },
        lifecycle: 'retained',
        time: { periodDays: 30, startedAt: iso(now - 33 * DAY), expiresAt: iso(now - 3 * DAY), retentionEndsAt: iso(now + 4 * DAY) },
        traffic: { totalBytes: 50 * GB, usedBytes: 28.6 * GB },
        exit: { version: 1, ip: '198.51.100.77', sharedCapacity: null, countryCode: 'sg', country: '新加坡', city: '新加坡', check: 'stale', latencyMs: null, checkedAt: iso(now - 3 * DAY) },
        access: access(1, 'us4wp', 'Pn6xF4tGa8Rk', 31380, now - 33 * DAY),
        quotas: { accessChangesLeft: 5, accessChangesPerDay: 5, freeExitChangesLeft: 1, freeExitWindowEndsAt: null },
        openExitRequestId: null,
        openOrderIds: [],
      },
      events: [
        { id: 'e1', at: iso(now - 33 * DAY), kind: 'ready', detail: '开通就绪，开始计时；出口 198.51.100.77，接入 v1', orderNo: 'ORD-20261005-0009' },
        { id: 'e2', at: iso(now - 3 * DAY), kind: 'expired', detail: '时长到期，停止转发；保留原出口与接入 7 天，剩余流量保留' },
      ],
    },
    {
      sub: {
        id: 'sub_g5ln',
        name: 'TikTok 英国',
        kind: 'isp',
        direct: null,
        plan: { name: '英国 · 伦敦 包月', exitMode: 'exclusive', countryCode: 'gb', country: '英国', city: '伦敦', concurrency: 50, bandwidthMbps: 20 },
        lifecycle: 'pending',
        time: { periodDays: 30, startedAt: null, expiresAt: null, retentionEndsAt: null },
        traffic: { totalBytes: 50 * GB, usedBytes: 0 },
        exit: null,
        access: null,
        quotas: { accessChangesLeft: 5, accessChangesPerDay: 5, freeExitChangesLeft: 1, freeExitWindowEndsAt: null },
        openExitRequestId: null,
        openOrderIds: ['ord_0041'],
      },
      events: [],
    },
    {
      sub: {
        id: 'sub_n2ry',
        name: 'Facebook 广告 · 美东',
        kind: 'isp',
        direct: null,
        plan: { name: '美国 · 纽约 包月', exitMode: 'exclusive', countryCode: 'us', country: '美国', city: '纽约', concurrency: 50, bandwidthMbps: 20 },
        lifecycle: 'active',
        time: { periodDays: 30, startedAt: iso(now - 9 * DAY), expiresAt: iso(now + 21 * DAY), retentionEndsAt: null },
        traffic: { totalBytes: 50 * GB, usedBytes: 9.4 * GB },
        exit: { version: 1, ip: '198.51.100.140', sharedCapacity: null, countryCode: 'us', country: '美国', city: '纽约', check: 'slow', latencyMs: 912, checkedAt: iso(now - 8 * 60_000) },
        access: access(1, 'un2ry', 'Ju5rY3wNc9Hd', 31480, now - 9 * DAY),
        quotas: { accessChangesLeft: 5, accessChangesPerDay: 5, freeExitChangesLeft: 0, freeExitWindowEndsAt: iso(now + 28 * DAY) },
        openExitRequestId: 'req_0007',
        openOrderIds: [],
      },
      events: [
        { id: 'e1', at: iso(now - 9 * DAY), kind: 'ready', detail: '开通就绪，开始计时；出口 198.51.100.140，接入 v1', orderNo: 'ORD-20261029-0024' },
      ],
    },
    {
      // 服务器直出代理（D-25）：管理员赠送，没有套餐与出口；可用截止由 view() 按付费订阅计算。
      sub: {
        id: 'sub_dx01',
        name: '服务器直出',
        kind: 'direct',
        direct: { bandwidthMbps: 10, maxConnections: 64, paidUntil: null, region: { countryCode: 'hk', name: '香港' } },
        plan: null,
        lifecycle: 'active',
        time: { periodDays: 0, startedAt: iso(now - 6 * DAY), expiresAt: null, retentionEndsAt: null },
        traffic: { totalBytes: 0, usedBytes: 312.4 * GB },
        exit: null,
        access: access(1, 'udx01', 'Rt7mZ3qLw9Kc', 31580, now - 6 * DAY),
        quotas: { accessChangesLeft: 5, accessChangesPerDay: 5, freeExitChangesLeft: 0, freeExitWindowEndsAt: null },
        openExitRequestId: null,
        openOrderIds: [],
      },
      events: [
        { id: 'e1', at: iso(now - 6 * DAY), kind: 'ready', detail: '服务器直出代理已开通，接入 v1；不限流量，跟随你的付费 IP 有效' },
      ],
    },
  ]

  const done = (label: string, at: number): OrderStep => ({ label, state: 'done', at: iso(at) })
  const step = (label: string, state: OrderStep['state']): OrderStep => ({ label, state, at: null })
  const snapshot = (lines: string[]) => [
    ...lines,
    '时长按 30×24 小时计，首购在开通就绪时起算',
    '流量长期保留，不按月清零；到期和释放都不清余额',
    '时长到期或流量用尽任一发生，即停止全部转发',
    '到期后保留原出口与接入 7 天，期内续时长可恢复',
    '换接入每 24 小时 5 次；换出口每 30 天免费 1 次',
    '收款由管理员人工核对，确认前不开通、不入账',
  ]

  const orders: Order[] = [
    {
      id: 'ord_0041', no: 'ORD-20261107-0041', kind: 'new', createdAt: iso(now - 5 * HOUR), holdUntil: null,
      amount: cny(188), payment: 'confirmed', fulfillment: 'processing', userClaimedPaidAt: iso(now - 4.5 * HOUR),
      payRef: 'K7Q3', subscriptionId: 'sub_g5ln', subscriptionName: 'TikTok 英国',
      lines: [{ label: '英国 · 伦敦 包月', detail: '1 个月 · 含 50 GB · 并发 50 · 20 Mbps', amount: cny(188) }],
      steps: [done('下单', now - 5 * HOUR), done('核款确认', now - 2 * HOUR), step('开通中', 'current'), step('已交付', 'todo')],
      rulesSnapshot: snapshot(['首购：时长 30 天 + 流量 50 GB']),
    },
    {
      id: 'ord_0038', no: 'ORD-20261106-0038', kind: 'add_traffic', createdAt: iso(now - 8 * HOUR), holdUntil: iso(now + 16 * HOUR),
      amount: cny(75), payment: 'awaiting', fulfillment: 'not_started', userClaimedPaidAt: iso(now - 7 * HOUR),
      payRef: 'M2X8', subscriptionId: 'sub_j9tx', subscriptionName: '采集任务 · 日本',
      lines: [{ label: '买流量', detail: '+50 GB · 不改到期时间', amount: cny(75) }],
      steps: [done('下单', now - 8 * HOUR), step('核款确认', 'current'), step('入账', 'todo')],
      rulesSnapshot: snapshot(['买流量：只增加流量，不改到期时间']),
    },
    {
      id: 'ord_0031', no: 'ORD-20261104-0031', kind: 'renew_time', createdAt: iso(now - 3 * DAY - 3 * HOUR), holdUntil: null,
      amount: cny(243), payment: 'confirmed', fulfillment: 'delivered', userClaimedPaidAt: null,
      payRef: 'C5T1', subscriptionId: 'sub_7k2m', subscriptionName: 'TikTok 美区 1 号',
      lines: [
        { label: '续时长', detail: '+1 个月 · 接在原到期时间之后', amount: cny(168) },
        { label: '买流量', detail: '+50 GB', amount: cny(75) },
      ],
      steps: [done('下单', now - 3 * DAY - 3 * HOUR), done('核款确认', now - 3 * DAY), done('入账', now - 3 * DAY)],
      rulesSnapshot: snapshot(['续时长：只延长时长，不改流量', '买流量：只增加流量，不改到期时间']),
    },
    {
      id: 'ord_0027', no: 'ORD-20261103-0027', kind: 'new', createdAt: iso(now - 4 * DAY), holdUntil: null,
      amount: cny(178), payment: 'cancelled', fulfillment: 'not_started', userClaimedPaidAt: null,
      payRef: 'H9P4', subscriptionId: null, subscriptionName: null,
      lines: [{ label: '美国 · 纽约 包月', detail: '1 个月 · 含 50 GB', amount: cny(178) }],
      steps: [done('下单', now - 4 * DAY), { label: '超时未付款，已自动取消', state: 'failed', at: iso(now - 3 * DAY) }],
      rulesSnapshot: snapshot(['首购：时长 30 天 + 流量 50 GB']),
    },
    {
      id: 'ord_0012', no: 'ORD-20261001-0012', kind: 'new', createdAt: iso(now - 38 * DAY), holdUntil: null,
      amount: cny(168), payment: 'confirmed', fulfillment: 'delivered', userClaimedPaidAt: iso(now - 38 * DAY),
      payRef: 'A1B6', subscriptionId: 'sub_7k2m', subscriptionName: 'TikTok 美区 1 号',
      lines: [{ label: '美国 · 洛杉矶 包月', detail: '1 个月 · 含 50 GB · 并发 50 · 20 Mbps', amount: cny(168) }],
      steps: [done('下单', now - 38 * DAY), done('核款确认', now - 37.5 * DAY), done('开通中', now - 37.2 * DAY), done('已交付', now - 37 * DAY)],
      rulesSnapshot: snapshot(['首购：时长 30 天 + 流量 50 GB']),
    },
  ]

  const requests: ExitRequest[] = [
    {
      id: 'req_0007', no: 'REQ-1105-0007', subscriptionId: 'sub_n2ry', subscriptionName: 'Facebook 广告 · 美东',
      createdAt: iso(now - 2 * DAY), updatedAt: iso(now - 20 * HOUR), reason: 'quality', note: '近两天延迟高，广告后台频繁验证',
      alsoChangeAccess: false, charge: 'free', status: 'processing', oldIp: '198.51.100.140', newIp: null, failureReason: null,
    },
    {
      id: 'req_0003', no: 'REQ-1020-0003', subscriptionId: 'sub_7k2m', subscriptionName: 'TikTok 美区 1 号',
      createdAt: iso(now - 19 * DAY), updatedAt: iso(now - 18 * DAY), reason: 'target_blocked', note: '',
      alsoChangeAccess: true, charge: 'free', status: 'succeeded', oldIp: '203.0.113.9', newIp: '203.0.113.48', failureReason: null,
    },
    {
      id: 'req_0001', no: 'REQ-1029-0001', subscriptionId: 'sub_j9tx', subscriptionName: '采集任务 · 日本',
      createdAt: iso(now - 9 * DAY), updatedAt: iso(now - 8 * DAY), reason: 'region', note: '想换大阪',
      alsoChangeAccess: false, charge: 'free', status: 'failed', oldIp: '203.0.113.112', newIp: null,
      failureReason: '目标地区暂无可分配的出口，本次未计入免费次数，可稍后重新申请。',
    },
  ]

  return { catalog, subs, orders, requests }
}

/** 按 docs/01 §7.2 优先级推导阻断原因。直出代理（D-25）不会到期也不会耗尽，没有付费订阅时为 no_paid_subscription。 */
function blockersOf(s: SubSeed, remainingSeconds: number, banned: boolean, paidUntil: string | null): Blocker[] {
  const b: Blocker[] = []
  if (banned) b.push('account_banned')
  if (s.sub.kind === 'direct') {
    if (s.adminPaused) b.push('admin_paused')
    if (s.sub.lifecycle === 'pending') b.push('pending')
    if (s.sub.lifecycle === 'fulfill_failed') b.push('fulfill_failed')
    if (s.sub.lifecycle === 'active' && !paidUntil) b.push('no_paid_subscription')
    return b
  }
  if (s.sub.lifecycle === 'released') b.push('released')
  if (s.adminPaused) b.push('admin_paused')
  if (s.sub.lifecycle === 'pending') b.push('pending')
  if (s.sub.lifecycle === 'fulfill_failed') b.push('fulfill_failed')
  const timed = s.sub.lifecycle === 'active' || s.sub.lifecycle === 'retained'
  if (timed && remainingSeconds <= 0) b.push('expired')
  if (s.sub.lifecycle !== 'pending' && s.sub.traffic.usedBytes >= s.sub.traffic.totalBytes) b.push('traffic_exhausted')
  if (timed && s.sub.exit?.check === 'failed') b.push('dataplane')
  return b
}

const pick = <T,>(xs: readonly T[]): T => xs[Math.floor(Math.random() * xs.length)] as T
const ALNUM = 'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789'
const randomPassword = () => Array.from({ length: 12 }, () => pick([...ALNUM])).join('')

export function createMockApi(opts: MockOptions = {}): MockPortalApi {
  const now = opts.now ?? MOCK_NOW
  const [lo, hi] = opts.latency ?? [300, 600]
  const db = seed(now)
  let account: Account | null =
    opts.signedIn === false ? null : { email: 'demo@example.com', verified: true, status: 'active', createdAt: iso(now - 40 * DAY) }
  let pending: { email: string; password: string } | null = null
  let orderSeq = 42
  let reqSeq = 8
  // 幂等键 → 首次结果（同一键重复提交返回首次结果，与服务端一致）。
  const idem = new Map<string, unknown>()
  const once = <T,>(key: string, fn: () => T): T => {
    if (idem.has(key)) return idem.get(key) as T
    const v = fn()
    idem.set(key, v)
    return v
  }
  const calls: MockPortalApi['calls'] = {}
  const failures = new Map<keyof PortalApi, Error>()

  const wait = <T,>(fn: () => T, op?: keyof PortalApi): Promise<T> =>
    new Promise((resolve, reject) => {
      if (op) {
        calls[op] = (calls[op] ?? 0) + 1
        const f = failures.get(op)
        if (f) {
          failures.delete(op)
          setTimeout(() => reject(f), lo)
          return
        }
      }
      const ms = lo + Math.random() * (hi - lo)
      setTimeout(() => {
        try {
          resolve(structuredClone(fn()))
        } catch (e) {
          reject(e)
        }
      }, ms)
    })

  const authed = () => {
    if (!account) throw new ApiError('unauthorized', '登录已失效，请重新登录')
    if (!account.verified) throw new ApiError('unverified', '请先完成邮箱验证')
    return account
  }

  /** 直出代理的可用截止：时长有效（已开始、未到期）的 ISP 订阅中最晚的到期时间。 */
  const paidUntil = (): string | null => {
    let best: string | null = null
    for (const x of db.subs) {
      const t = x.sub.time
      if (x.sub.kind !== 'isp' || !t.startedAt || !t.expiresAt || Date.parse(t.expiresAt) <= now) continue
      if (best === null || t.expiresAt > best) best = t.expiresAt
    }
    return best
  }

  const view = (s: SubSeed): Subscription => {
    const until = s.sub.kind === 'direct' ? paidUntil() : null
    const expiresAt = s.sub.kind === 'direct' ? until : s.sub.time.expiresAt
    const exp = expiresAt ? Date.parse(expiresAt) : null
    const remainingSeconds = exp === null ? 0 : Math.max(0, Math.floor((exp - now) / 1000))
    return {
      ...s.sub,
      direct: s.sub.direct ? { ...s.sub.direct, paidUntil: until } : null,
      time: { ...s.sub.time, expiresAt, remainingSeconds },
      blockers: blockersOf(s, remainingSeconds, account?.status === 'banned', until),
    }
  }

  /** 直出代理不能续时长、加流量或换出口（服务端返回 412）。 */
  const ispOnly = (s: SubSeed) => {
    if (s.sub.kind === 'direct' || !s.sub.plan)
      throw new ApiError('precondition', '直出代理跟随你的付费 IP 有效且不限流量，不能续时长、加流量或换出口')
    return s.sub.plan
  }

  const findSub = (id: string) => {
    const s = db.subs.find((x) => x.sub.id === id)
    if (!s) throw new ApiError('not_found', '没有找到这个 IP')
    return s
  }
  const findOrder = (id: string) => {
    const o = db.orders.find((x) => x.id === id)
    if (!o) throw new ApiError('not_found', '没有找到这个订单')
    return o
  }

  return {
    session: () => wait(() => account, 'session'),
    login: (email, password) =>
      wait(() => {
        if (password.length < 8) throw new ApiError('invalid', '邮箱或密码不正确')
        const e = email.trim().toLowerCase()
        const verified = !(pending && pending.email === e)
        account = { email: e, verified, status: 'active', createdAt: iso(now - (verified ? 40 * DAY : 0)) }
        return account
      }, 'login'),
    register: (email, password) =>
      wait(() => {
        // 与服务端一致：注册不建会话，随后用验证码（带邮箱）验证即登录。
        pending = { email: email.trim().toLowerCase(), password }
        return { email: pending.email }
      }, 'register'),
    verifyEmail: (code, email) =>
      wait(() => {
        if (code.trim() !== '123456') throw new ApiError('invalid', '验证码不正确或已过期')
        if (account) {
          account = { ...account, verified: true }
          return account
        }
        const e = email?.trim().toLowerCase()
        if (!e || !pending || pending.email !== e) throw new ApiError('invalid', '验证码不正确或已过期')
        account = { email: e, verified: true, status: 'active', createdAt: iso(now) }
        pending = null
        return account
      }, 'verifyEmail'),
    resendVerification: () => wait(() => undefined, 'resendVerification'),
    requestPasswordReset: () => wait(() => undefined, 'requestPasswordReset'),
    confirmPasswordReset: (_email, code, next) =>
      wait(() => {
        if (code.trim() !== '123456') throw new ApiError('invalid', '验证码不正确或已过期')
        if (next.length < 10) throw new ApiError('invalid', '新密码至少 10 位')
        account = null
      }, 'confirmPasswordReset'),
    changePassword: (current, next) =>
      wait(() => {
        authed()
        if (current.length < 8) throw new ApiError('invalid', '当前密码不正确')
        if (next.length < 10) throw new ApiError('invalid', '新密码至少 10 位')
      }, 'changePassword'),
    logout: () =>
      wait(() => {
        account = null
      }, 'logout'),
    expireSession: () => {
      account = null
    },
    failNext: (op, err) => {
      failures.set(op, err)
    },
    calls,

    listSubscriptions: () =>
      wait(() => {
        authed()
        return db.subs.map(view)
      }),
    getSubscription: (id) =>
      wait(() => {
        authed()
        return view(findSub(id))
      }),
    listSubscriptionEvents: (id) =>
      wait(() => {
        authed()
        return [...findSub(id).events].reverse()
      }),
    renameSubscription: (id, name) =>
      wait(() => {
        authed()
        const s = findSub(id)
        const trimmed = name.trim()
        if (!trimmed || trimmed.length > 24) throw new ApiError('invalid', '名称需为 1～24 个字符')
        s.events.push({ id: `e${s.events.length + 1}`, at: iso(now), kind: 'renamed', detail: `改名：${s.sub.name} → ${trimmed}` })
        s.sub.name = trimmed
        return view(s)
      }),
    changeAccess: (id, mode, key) =>
      wait(() => once(`access:${key}`, () => {
        authed()
        const s = findSub(id)
        const v = view(s)
        if (v.blockers.includes('admin_paused') || v.blockers.includes('account_banned'))
          throw new ApiError('precondition', '当前状态不能自助换接入，请联系客服')
        if (!s.sub.access || (s.sub.lifecycle !== 'active' && s.sub.lifecycle !== 'retained'))
          throw new ApiError('precondition', '这个 IP 还没有接入可换')
        if (s.sub.quotas.accessChangesLeft <= 0)
          throw new ApiError('quota_exhausted', '24 小时内的换接入次数已用完')
        const prev = s.sub.access
        const ports = mode === 'ports_and_password'
          ? (() => {
              const base = 32000 + Math.floor(Math.random() * 900) * 3
              return { http: base, https: base + 1, socks5: base + 2 }
            })()
          : prev.ports
        const next: Access = {
          ...prev,
          version: prev.version + 1,
          password: randomPassword(),
          ports,
          issuedAt: iso(now),
          unseenChanges: mode === 'ports_and_password' ? ['ports', 'password'] : ['password'],
        }
        s.sub.access = next
        s.sub.quotas.accessChangesLeft -= 1
        s.events.push({
          id: `e${s.events.length + 1}`,
          at: iso(now),
          kind: mode === 'password' ? 'password_reset' : 'access_changed',
          detail: `接入 v${prev.version} → v${next.version}：${mode === 'password' ? '仅重置密码，端口不变' : '端口与密码已更换'}；出口与余量不变`,
        })
        return next
      }), 'changeAccess'),
    acknowledgeAccess: (id) =>
      wait(() => {
        authed()
        const a = findSub(id).sub.access
        if (a) a.unseenChanges = []
      }),

    getCatalog: () => wait(() => db.catalog),
    createOrder: (input: OrderInput, key: string) =>
      wait(() => once(`order:${key}`, () => {
        authed()
        const seq = orderSeq++
        const base = {
          id: `ord_${String(seq).padStart(4, '0')}`,
          no: `ORD-20261107-${String(seq).padStart(4, '0')}`,
          createdAt: iso(now),
          holdUntil: iso(now + db.catalog.rules.unpaidHoldHours * HOUR),
          payment: 'awaiting' as const,
          fulfillment: 'not_started' as const,
          userClaimedPaidAt: null,
          payRef: Array.from({ length: 4 }, () => pick([...'ABCDEFGHJKMNPQRSTUVWXYZ23456789'])).join(''),
        }
        let order: Order
        if (input.kind === 'new') {
          if (input.quantity !== 1) throw new ApiError('invalid', '一次订单只能购买 1 个 IP')
          const plan: Plan | undefined = db.catalog.plans.find((p) => p.id === input.planId)
          if (!plan) throw new ApiError('not_found', '套餐不存在或已下架')
          if (plan.stock === 'out') throw new ApiError('conflict', '该地区暂时无货，请选择其他地区')
          const unpaid = db.orders.filter((o) => o.kind === 'new' && o.payment === 'awaiting').length
          if (unpaid >= db.catalog.rules.maxUnpaidNewOrders)
            throw new ApiError('conflict', `最多同时保留 ${db.catalog.rules.maxUnpaidNewOrders} 个未付款新购订单`)
          const yuan = (plan.monthlyPrice.amount / 100) * input.months
          order = {
            ...base, kind: 'new', amount: cny(yuan), subscriptionId: null, subscriptionName: null,
            lines: [{
              label: `${plan.country} · ${plan.city} ${plan.exitMode === 'shared' ? '共享' : ''}包月`,
              detail: `${input.months} 个月 · 含 ${plan.includedBytes / GB} GB · 并发 ${plan.concurrency} · ${plan.bandwidthMbps} Mbps`,
              amount: cny(yuan),
            }],
            steps: [{ label: '下单', state: 'done', at: iso(now) }, { label: '核款确认', state: 'current', at: null }, { label: '开通中', state: 'todo', at: null }, { label: '已交付', state: 'todo', at: null }],
            rulesSnapshot: [`首购：时长 ${input.months * 30} 天 + 流量 ${plan.includedBytes / GB} GB`, '具体出口在开通时分配，下单不锁定地址'],
          }
        } else {
          const s = findSub(input.subscriptionId)
          const subPlan = ispOnly(s)
          if (input.kind === 'renew_time') {
            const plan = db.catalog.plans.find((p) => p.city === subPlan.city && p.exitMode === subPlan.exitMode)
            const yuan = ((plan?.monthlyPrice.amount ?? 16800) / 100) * input.months
            order = {
              ...base, kind: 'renew_time', amount: cny(yuan), subscriptionId: s.sub.id, subscriptionName: s.sub.name,
              lines: [{ label: '续时长', detail: `+${input.months * 30} 天 · 不改流量`, amount: cny(yuan) }],
              steps: [{ label: '下单', state: 'done', at: iso(now) }, { label: '核款确认', state: 'current', at: null }, { label: '入账', state: 'todo', at: null }],
              rulesSnapshot: ['续时长：只延长时长，不改流量', s.sub.lifecycle === 'retained' ? '保留期内续费：从重新就绪时起算，恢复原出口与接入' : '有效期内续费：接在原到期时间之后'],
            }
          } else {
            const pack = db.catalog.trafficPacks.find((p) => p.id === input.packId)
            if (!pack) throw new ApiError('not_found', '流量包不存在')
            order = {
              ...base, kind: 'add_traffic', amount: pack.price, subscriptionId: s.sub.id, subscriptionName: s.sub.name,
              lines: [{ label: '买流量', detail: `+${pack.bytes / GB} GB · 不改到期时间`, amount: pack.price }],
              steps: [{ label: '下单', state: 'done', at: iso(now) }, { label: '核款确认', state: 'current', at: null }, { label: '入账', state: 'todo', at: null }],
              rulesSnapshot: ['买流量：只增加流量，不改到期时间', ...(s.sub.lifecycle === 'retained' ? ['该 IP 已到期：买流量后仍需续时长才能使用'] : [])],
            }
          }
          s.sub.openOrderIds.push(order.id)
        }
        db.orders.unshift(order)
        return order
      }), 'createOrder'),
    listOrders: () =>
      wait(() => {
        authed()
        return db.orders
      }),
    getOrder: (id) =>
      wait(() => {
        authed()
        return findOrder(id)
      }),
    claimPaid: (id) =>
      wait(() => {
        authed()
        const o = findOrder(id)
        if (o.payment !== 'awaiting') throw new ApiError('precondition', '订单当前状态不需要付款反馈')
        o.userClaimedPaidAt = iso(now)
        return o
      }),
    cancelOrder: (id) =>
      wait(() => {
        authed()
        const o = findOrder(id)
        if (o.payment !== 'awaiting') throw new ApiError('precondition', '只有待核款的订单可以取消')
        if (o.userClaimedPaidAt) throw new ApiError('precondition', '已反馈付款的订单请联系客服处理')
        o.payment = 'cancelled'
        o.holdUntil = null
        o.steps = [o.steps[0] as OrderStep, { label: '已由你取消', state: 'failed', at: iso(now) }]
        for (const s of db.subs) s.sub.openOrderIds = s.sub.openOrderIds.filter((x) => x !== id)
        return o
      }),

    listExitRequests: () =>
      wait(() => {
        authed()
        return db.requests
      }),
    createExitRequest: (input: ExitRequestInput, key: string) =>
      wait(() => once(`exit:${key}`, () => {
        authed()
        const s = findSub(input.subscriptionId)
        ispOnly(s)
        if (s.sub.openExitRequestId) {
          const existing = db.requests.find((r) => r.id === s.sub.openExitRequestId)
          throw new ApiError('conflict', '这个 IP 已有未完成的换出口申请', { details: existing ? { existing } : null })
        }
        if (!s.sub.exit || (s.sub.lifecycle !== 'active' && s.sub.lifecycle !== 'retained'))
          throw new ApiError('precondition', '这个 IP 还没有出口可换')
        const seq = reqSeq++
        const free = s.sub.quotas.freeExitChangesLeft > 0
        const req: ExitRequest = {
          id: `req_${String(seq).padStart(4, '0')}`,
          no: `REQ-1107-${String(seq).padStart(4, '0')}`,
          subscriptionId: s.sub.id,
          subscriptionName: s.sub.name,
          createdAt: iso(now),
          updatedAt: iso(now),
          reason: input.reason,
          note: input.note.trim(),
          alsoChangeAccess: input.alsoChangeAccess,
          charge: free ? 'free' : 'manual',
          status: 'pending',
          oldIp: s.sub.exit.ip,
          newIp: null,
          failureReason: null,
        }
        if (free) s.sub.quotas.freeExitChangesLeft -= 1
        s.sub.openExitRequestId = req.id
        db.requests.unshift(req)
        return req
      }), 'createExitRequest'),
  }
}
