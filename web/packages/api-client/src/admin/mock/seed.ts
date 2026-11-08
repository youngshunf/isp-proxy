// 管理端 mock 的种子数据。全部是示意：IP 为 RFC 5737 文档保留地址（192.0.2.0/24、198.51.100.0/24、203.0.113.0/24），
// 域名与邮箱为 example.com / example.net，价格与供应商为占位（D-04、D-05 未决）。
import type {
  AccessVersion,
  Actor,
  Admin,
  AdminAccess,
  AdminExit,
  AdminExitRequest,
  AdminLifecycle,
  AdminOrder,
  AdminSubscriptionEvent,
  Alert,
  AuditLog,
  ExitBinding,
  LedgerEntry,
  Money,
  Node,
  PlanVersion,
  Probe,
  Refund,
  RulesVersion,
  SaleStatus,
  Task,
  TrafficPackVersion,
  Transfer,
  Upstream,
} from '../types'
import { fid, payRefOf, secretOf } from './ids'

export const GB = 1_000_000_000
export const MIN = 60_000
export const HOUR = 3_600_000
export const DAY = 86_400_000
export const MONTH_S = 30 * 86_400

export const iso = (t: number) => new Date(t).toISOString()
export const cny = (yuan: number): Money => ({ amount: Math.round(yuan * 100), currency: 'CNY' })

/** 原型登录信息：示意账号，只存在于内存 mock。 */
export const MOCK_ADMIN_LOGIN = {
  email: 'ops@example.com',
  password: 'admin-pass-1',
  /** mock 接受的 TOTP 验证码（真实环境按 RFC 6238 计算）。 */
  totp: '246810',
  viewerEmail: 'audit@example.com',
  viewerPassword: 'viewer-pass-1',
  enrollEmail: 'new-admin@example.com',
  enrollPassword: 'admin-pass-1',
} as const

export interface AdminRow {
  admin: Admin
  password: string
  totpSecret: string
  recoveryCodes: string[]
}

export interface UserRow {
  id: string
  email: string
  verified: boolean
  status: 'active' | 'banned'
  passwordSet: boolean
  bannedAt: string | null
  bannedReason: string | null
  createdAt: string
  createdByAdmin: { id: string; email: string } | null
  lastLoginAt: string | null
  activeSessions: number
  resetSentAt: number | null
}

export type UsageProfile = 'fresh' | 'stale' | 'gappy' | 'idle' | 'none'

export interface SubRow {
  id: string
  name: string
  userId: string
  nodeId: string | null
  plan: { planId: string; planVersionId: string; planVersion: number; name: string; countryCode: string; country: string; city: string; concurrency: number; bandwidthMbps: number }
  /** 服务器直出代理（D-25）：有值时 plan 只是占位（视图里输出 null），没有出口与账本。 */
  direct?: { bandwidthMbps: number; maxConnections: number }
  lifecycle: AdminLifecycle
  lifecycleReason: string | null
  adminPaused: boolean
  pausedReason: string | null
  startedAt: string | null
  expiresAt: string | null
  retentionEndsAt: string | null
  usedBytes: number
  exit: AdminExit | null
  access: AdminAccess | null
  accessPassword: string | null
  activeConns: number | null
  usageReportedAt: string | null
  desiredVersion: number
  appliedVersion: number
  createdAt: string
  firstReadyAt: string | null
  releasedAt: string | null
  deletedAt: string | null
  createdFromOrderId: string | null
  rulesVersion: string
  accessChangesLeft: number
  freeExitChangesLeft: number
  freeExitWindowEndsAt: string | null
  usage: UsageProfile
  /** 用量量级（字节/小时），趋势图用。 */
  usageScale: number
  accessVersions: AccessVersion[]
  exitBindings: ExitBinding[]
  events: AdminSubscriptionEvent[]
}

export interface PlanRow {
  id: string
  code: string
  status: SaleStatus
  sortOrder: number
  versions: PlanVersion[]
}

export interface PackRow {
  id: string
  code: string
  status: SaleStatus
  sortOrder: number
  versions: TrafficPackVersion[]
}

/** 上游资源 + mock 内部的“真实出口”（经该上游访问回显端点会看到什么）。 */
export interface UpstreamRow {
  u: Upstream
  probe: { ip: string; countryCode: string; city: string; latencyMs: number } | { error: string }
  /** 提交切换后数据面复核失败（演示失败回滚）。 */
  failsOnApply?: boolean
}

export interface TaskRow {
  t: Task
  polls: number
  /** 任务完成时要做的事。 */
  apply?: () => void
  failure?: { code: string; reason: string }
}

export interface ProbeRow {
  p: Probe
  polls: number
}

export interface Db {
  admins: AdminRow[]
  users: UserRow[]
  subs: SubRow[]
  ledger: LedgerEntry[]
  plans: PlanRow[]
  packs: PackRow[]
  rules: RulesVersion[]
  upstreams: UpstreamRow[]
  orders: AdminOrder[]
  refunds: Refund[]
  exitRequests: AdminExitRequest[]
  tasks: TaskRow[]
  probes: ProbeRow[]
  transfers: Transfer[]
  nodes: Node[]
  alerts: Alert[]
  audit: AuditLog[]
  seq: number
}

export const SYSTEM: Actor = { kind: 'system', id: null }

// ------------------------------------------------------------------ 构造

export function seed(now: number): Db {
  const ops: Admin = {
    id: fid('adm', 'ops'),
    email: MOCK_ADMIN_LOGIN.email,
    displayName: '值班运营',
    role: 'admin',
    totpEnrolled: true,
    lastLoginAt: iso(now - 20 * HOUR),
  }
  const viewer: Admin = {
    id: fid('adm', 'viewer'),
    email: MOCK_ADMIN_LOGIN.viewerEmail,
    displayName: '财务复核（只读）',
    role: 'viewer',
    totpEnrolled: true,
    lastLoginAt: iso(now - 3 * DAY),
  }
  const fresh: Admin = {
    id: fid('adm', 'fresh'),
    email: MOCK_ADMIN_LOGIN.enrollEmail,
    displayName: '新管理员',
    role: 'admin',
    totpEnrolled: false,
    lastLoginAt: null,
  }
  const opsRef = { id: ops.id, email: ops.email }
  const opsActor: Actor = { kind: 'admin', id: ops.id, email: ops.email }

  const admins: AdminRow[] = [
    { admin: ops, password: MOCK_ADMIN_LOGIN.password, totpSecret: 'JBSWY3DPEHPK3PXPJBSWY3DP', recoveryCodes: codes('ops') },
    { admin: viewer, password: MOCK_ADMIN_LOGIN.viewerPassword, totpSecret: 'KRSXG5CTMVRXEZLUKRSXG5CT', recoveryCodes: codes('viewer') },
    { admin: fresh, password: MOCK_ADMIN_LOGIN.enrollPassword, totpSecret: 'MZXW6YTBOI3DMZLSMZXW6YTB', recoveryCodes: [] },
  ]

  // ---------------------------------------------------------------- 节点
  const nodeLax = fid('nod', 'lax')
  const nodeFra = fid('nod', 'fra')
  const nodeSjc = fid('nod', 'sjc')
  const nodes: Node[] = [
    {
      id: nodeLax,
      name: 'lax-01',
      publicHost: 'proxy.example.com',
      portRange: { start: 20000, end: 29999 },
      status: 'active',
      online: true,
      lastSeenAt: iso(now - 4_000),
      lastReportAt: iso(now - 4_000),
      desiredRevision: 1842,
      appliedRevision: 1842,
      agentVersion: '3.3.3-isp.1',
      persistOk: true,
      clockSkewMs: 12,
      capabilities: { protocols: ['http', 'https', 'socks5'], ipv6: false, tls: true },
      ports: { total: 10000, free: 9900, allocated: 84, cooldown: 16 },
      subscriptions: 0,
      activeConns: 0,
      createdAt: iso(now - 60 * DAY),
    },
    {
      id: nodeFra,
      name: 'fra-01',
      publicHost: 'fra.proxy.example.com',
      portRange: { start: 20000, end: 24999 },
      status: 'active',
      online: false,
      lastSeenAt: iso(now - 42 * MIN),
      lastReportAt: iso(now - 42 * MIN),
      desiredRevision: 977,
      appliedRevision: 975,
      agentVersion: '3.3.3-isp.1',
      persistOk: null,
      clockSkewMs: null,
      capabilities: { protocols: ['http', 'https', 'socks5'], ipv6: true, tls: true },
      ports: { total: 5000, free: 4973, allocated: 21, cooldown: 6 },
      subscriptions: 0,
      activeConns: null,
      createdAt: iso(now - 45 * DAY),
    },
    {
      id: nodeSjc,
      name: 'sjc-02',
      publicHost: 'sjc.proxy.example.com',
      portRange: { start: 20000, end: 21999 },
      status: 'draining',
      online: true,
      lastSeenAt: iso(now - 6_000),
      lastReportAt: iso(now - 6_000),
      desiredRevision: 310,
      appliedRevision: 310,
      agentVersion: '3.3.3-isp.1',
      persistOk: true,
      clockSkewMs: 1_480,
      capabilities: { protocols: ['http', 'socks5'], ipv6: false, tls: false },
      ports: { total: 2000, free: 1994, allocated: 3, cooldown: 3 },
      subscriptions: 0,
      activeConns: 0,
      createdAt: iso(now - 20 * DAY),
    },
  ]

  // ---------------------------------------------------------------- 套餐、流量包、规则
  const planDefs = [
    { code: 'us-lax', cc: 'us', country: '美国', city: '洛杉矶', price: 168, gb: 50, conc: 50, bw: 20, status: 'on_sale' as const },
    { code: 'us-nyc', cc: 'us', country: '美国', city: '纽约', price: 178, gb: 50, conc: 50, bw: 20, status: 'on_sale' as const },
    { code: 'gb-lon', cc: 'gb', country: '英国', city: '伦敦', price: 188, gb: 50, conc: 50, bw: 20, status: 'on_sale' as const },
    { code: 'de-fra', cc: 'de', country: '德国', city: '法兰克福', price: 188, gb: 50, conc: 50, bw: 20, status: 'on_sale' as const },
    { code: 'jp-tyo', cc: 'jp', country: '日本', city: '东京', price: 198, gb: 100, conc: 80, bw: 30, status: 'on_sale' as const },
    { code: 'sg-sin', cc: 'sg', country: '新加坡', city: '新加坡', price: 158, gb: 50, conc: 50, bw: 20, status: 'on_sale' as const },
    { code: 'ca-yvr', cc: 'ca', country: '加拿大', city: '温哥华', price: 178, gb: 50, conc: 50, bw: 20, status: 'draft' as const },
  ]
  const plans: PlanRow[] = planDefs.map((d, i) => {
    const id = fid('pln', d.code)
    const v1: PlanVersion = {
      id: fid('plv', `${d.code}:1`),
      version: 1,
      name: `${d.country} · ${d.city} 包月`,
      countryCode: d.cc,
      country: d.country,
      city: d.city,
      price: cny(d.code === 'us-lax' ? 158 : d.price),
      periodDays: 30,
      includedBytes: d.gb * GB,
      concurrency: d.conc,
      bandwidthMbps: d.bw,
      createdAt: iso(now - 60 * DAY),
      createdBy: opsRef,
    }
    const versions = [v1]
    if (d.code === 'us-lax') {
      versions.unshift({ ...v1, id: fid('plv', `${d.code}:2`), version: 2, price: cny(d.price), createdAt: iso(now - 12 * DAY) })
    }
    return { id, code: d.code, status: d.status, sortOrder: (i + 1) * 10, versions }
  })
  const planBy = (code: string) => plans.find((p) => p.code === code)!

  const packs: PackRow[] = [
    { code: 't10', gb: 10, price: 18 },
    { code: 't50', gb: 50, price: 75 },
    { code: 't200', gb: 200, price: 260 },
  ].map((d, i) => ({
    id: fid('tpk', d.code),
    code: d.code,
    status: 'on_sale' as const,
    sortOrder: (i + 1) * 10,
    versions: [
      { id: fid('tpv', `${d.code}:1`), version: 1, bytes: d.gb * GB, price: cny(d.price), createdAt: iso(now - 60 * DAY), createdBy: opsRef },
    ],
  }))

  const baseRules = {
    periodDays: 30,
    retentionDays: 7,
    portCooldownDays: 7,
    resourceCooldownDays: 7,
    accessChangesPerWindow: 5,
    accessChangeWindowHours: 24,
    freeExitChangesPerWindow: 1,
    exitChangeWindowDays: 30,
    unpaidHoldHours: 24,
    maxUnpaidNewOrders: 3,
    monthOptions: [1, 3, 6],
    snapshotLines: [
      '1 个月 = 30 × 24 小时，首次开通就绪时起算',
      '流量长期保留，不按月清零；到期与释放都不清余额',
      '平台内独享出口；换接入、换出口不改变时长与流量',
      '人工核款，确认收款前不开通',
    ],
  }
  const rules: RulesVersion[] = [
    {
      ...baseRules,
      version: 'R-2026-11-08.1',
      effectiveFrom: iso(now - 20 * HOUR),
      payment: {
        payee: '示意收款主体（原型）',
        bank: '示意银行',
        account: '0000 0000 0000 0000',
        note: '转账时在附言填写付款识别码，便于核对。核款为人工处理，确认前订单不会开通。',
      },
      createdAt: iso(now - 20 * HOUR),
      createdBy: opsRef,
    },
    {
      ...baseRules,
      version: 'R-2026-11-08',
      effectiveFrom: iso(now - 30 * HOUR),
      payment: { payee: '', bank: '', account: '', note: '' },
      createdAt: iso(now - 30 * HOUR),
      createdBy: null,
    },
  ]

  // ---------------------------------------------------------------- 用户
  const users: UserRow[] = []
  const user = (key: string, email: string, extra: Partial<UserRow> = {}): UserRow => {
    const u: UserRow = {
      id: fid('usr', key),
      email,
      verified: true,
      status: 'active',
      passwordSet: true,
      bannedAt: null,
      bannedReason: null,
      createdAt: iso(now - (40 + users.length) * DAY),
      createdByAdmin: null,
      lastLoginAt: iso(now - (users.length % 5) * HOUR - 20 * MIN),
      activeSessions: 1 + (users.length % 3),
      resetSentAt: null,
      ...extra,
    }
    users.push(u)
    return u
  }
  const alice = user('alice', 'alice@example.com')
  const bob = user('bob', 'bob@example.com')
  const carol = user('carol', 'carol@example.com')
  const dave = user('dave', 'dave@example.com', {
    status: 'banned',
    bannedAt: iso(now - 2 * DAY),
    bannedReason: '滥用投诉：批量注册目标站点账号（工单 #2031）',
    activeSessions: 0,
  })
  user('erin', 'erin@example.net', {
    verified: false,
    passwordSet: false,
    createdByAdmin: opsRef,
    createdAt: iso(now - 5 * HOUR),
    lastLoginAt: null,
    activeSessions: 0,
  })
  const frank = user('frank', 'frank@example.com')
  const grace = user('grace', 'grace@example.com')
  const hank = user('hank', 'hank@example.net', { createdAt: iso(now - 3 * HOUR), lastLoginAt: iso(now - 2 * HOUR) })
  const fillers: UserRow[] = []
  for (let i = 1; i <= 14; i++) fillers.push(user(`u${i}`, `user${String(i).padStart(2, '0')}@example.com`))

  // ---------------------------------------------------------------- 上游资源
  const upstreams: UpstreamRow[] = []
  const upstream = (
    key: string,
    o: {
      node: string
      plan: string
      host: string
      port: number
      protocol?: 'http' | 'https' | 'socks5'
      provider?: string
      status: Upstream['status']
      exit: UpstreamRow['probe']
      bound?: string | null
      cooldownUntil?: number
      statusReason?: string
      verifiedOk?: boolean
      failsOnApply?: boolean
      note?: string
      createdAgo?: number
    },
  ): UpstreamRow => {
    const p = planBy(o.plan).versions[0]!
    const probe = o.exit
    const row: UpstreamRow = {
      u: {
        id: fid('res', key),
        nodeId: o.node,
        protocol: o.protocol ?? 'socks5',
        host: o.host,
        port: o.port,
        username: `isp-${key}`,
        hasPassword: true,
        countryCode: p.countryCode,
        country: p.country,
        city: p.city,
        provider: o.provider ?? '示意供应商 A',
        note: o.note ?? '',
        status: o.status,
        statusReason: o.statusReason ?? null,
        reservedOrderId: null,
        reservedTaskId: null,
        reservedUntil: null,
        boundSubscriptionId: o.bound ?? null,
        cooldownUntil: o.cooldownUntil ? iso(o.cooldownUntil) : null,
        lastVerify:
          o.verifiedOk === false
            ? null
            : 'error' in probe
              ? { at: iso(now - 2 * HOUR), ok: false, exitIp: null, exitCountryCode: null, error: probe.error }
              : { at: iso(now - (o.createdAgo ?? 10 * DAY)), ok: true, exitIp: probe.ip, exitCountryCode: probe.countryCode, error: null },
        rowVersion: 1,
        createdAt: iso(now - (o.createdAgo ?? 10 * DAY)),
        updatedAt: iso(now - (o.createdAgo ?? 10 * DAY)),
      },
      probe,
      ...(o.failsOnApply ? { failsOnApply: true } : {}),
    }
    upstreams.push(row)
    return row
  }

  // ---------------------------------------------------------------- 订阅
  const subs: SubRow[] = []
  const ledger: LedgerEntry[] = []
  const orders: AdminOrder[] = []
  let orderSeq = 30

  const ledgerEntry = (sub: SubRow, kind: 'time' | 'traffic', amount: number, src: LedgerEntry['sourceType'], at: number, extra: Partial<LedgerEntry> = {}) => {
    ledger.push({
      id: fid('led', `${sub.id}:${ledger.length}`),
      subscriptionId: sub.id,
      entitlementKind: kind,
      amount,
      unit: kind === 'time' ? 'seconds' : 'bytes',
      sourceType: src,
      sourceId: extra.orderId ?? fid('src', `${sub.id}:${ledger.length}`),
      orderId: null,
      refundId: null,
      transferId: null,
      reason: '',
      admin: src === 'order' ? null : opsRef,
      createdAt: iso(at),
      ...extra,
    })
  }

  const orderNo = (at: number) => {
    orderSeq += 1
    const d = new Date(at + 8 * HOUR)
    const ymd = `${d.getUTCFullYear()}${String(d.getUTCMonth() + 1).padStart(2, '0')}${String(d.getUTCDate()).padStart(2, '0')}`
    return `ORD-${ymd}-${String(orderSeq).padStart(4, '0')}`
  }

  const makeOrder = (o: {
    key: string
    user: UserRow
    kind: AdminOrder['kind']
    at: number
    lines: { label: string; detail: string; amount: Money }[]
    payment: AdminOrder['payment']
    fulfillment: AdminOrder['fulfillment']
    sub?: SubRow | null
    planVersionId?: string | null
    claimedAgo?: number
    confirmedAt?: number
    deliveredAt?: number
    fulfillmentError?: string
    cancelReason?: AdminOrder['cancelReason']
    externalRef?: string
    rulesVersion?: string
  }): AdminOrder => {
    const total = o.lines.reduce((a, l) => a + l.amount.amount, 0)
    const id = fid('ord', o.key)
    const confirmed = o.confirmedAt ?? null
    const order: AdminOrder = {
      id,
      no: orderNo(o.at),
      kind: o.kind,
      createdAt: iso(o.at),
      holdUntil: o.payment === 'awaiting' || o.cancelReason === 'timeout' ? iso(o.at + 24 * HOUR) : null,
      amount: { amount: total, currency: 'CNY' },
      payment: o.payment,
      fulfillment: o.fulfillment,
      userClaimedPaidAt: o.claimedAgo !== undefined ? iso(now - o.claimedAgo) : null,
      payRef: payRefOf(o.key),
      subscriptionId: o.sub?.id ?? null,
      subscriptionName: o.sub?.name ?? null,
      lines: o.lines,
      steps: [],
      rulesSnapshot: rules[0]!.snapshotLines,
      user: { id: o.user.id, email: o.user.email },
      paymentMethod: 'manual_transfer',
      paymentRecord:
        confirmed !== null
          ? {
              id: fid('pay', o.key),
              amount: { amount: total, currency: 'CNY' },
              method: 'manual_transfer',
              externalRef: o.externalRef ?? `BANK-${payRefOf(o.key)}-${String(o.at).slice(-6)}`,
              lateArrival: false,
              note: '',
              confirmedBy: opsRef,
              confirmedAt: iso(confirmed),
            }
          : null,
      ledgerEntries: [],
      refunds: [],
      rulesVersion: o.rulesVersion ?? rules[0]!.version,
      planVersionId: o.planVersionId ?? null,
      fulfillmentError: o.fulfillmentError ?? null,
      cancelReason: o.cancelReason ?? null,
      confirmedAt: confirmed !== null ? iso(confirmed) : null,
      deliveredAt: o.deliveredAt !== undefined ? iso(o.deliveredAt) : null,
      createdByAdmin: null,
    }
    orders.push(order)
    return order
  }

  let portBase = 21000
  const makeSub = (o: {
    key: string
    name: string
    user: UserRow
    plan: string
    node?: string
    lifecycle: AdminLifecycle
    startedAgoDays?: number
    months?: number
    expiresIn?: number
    retentionEndsIn?: number
    usedGB?: number
    extraTrafficGB?: number
    exit?: { ip: string; check?: AdminExit['check']; latencyMs?: number | null; checkedAgo?: number; error?: string; provider?: string } | null
    access?: boolean
    paused?: string
    usage?: UsageProfile
    usageScale?: number
    conns?: number | null
    lifecycleReason?: string
    freeExitLeft?: number
  }): SubRow => {
    const plan = planBy(o.plan)
    const pv = plan.versions[plan.versions.length - 1]!
    const id = fid('sub', o.key)
    const node = o.node ?? (o.plan === 'de-fra' ? nodeFra : nodeLax)
    const started = o.startedAgoDays !== undefined ? now - o.startedAgoDays * DAY : null
    const months = o.months ?? 1
    const ready = started !== null
    const createdAt = (started ?? now - 2 * HOUR) - 30 * MIN
    const sub: SubRow = {
      id,
      name: o.name,
      userId: o.user.id,
      nodeId: o.lifecycle === 'pending' && !ready ? node : node,
      plan: {
        planId: plan.id,
        planVersionId: pv.id,
        planVersion: pv.version,
        name: pv.name,
        countryCode: pv.countryCode,
        country: pv.country,
        city: pv.city,
        concurrency: pv.concurrency,
        bandwidthMbps: pv.bandwidthMbps,
      },
      lifecycle: o.lifecycle,
      lifecycleReason: o.lifecycleReason ?? null,
      adminPaused: Boolean(o.paused),
      pausedReason: o.paused ?? null,
      startedAt: started !== null ? iso(started) : null,
      expiresAt: o.expiresIn !== undefined ? iso(now + o.expiresIn) : started !== null ? iso(started + months * 30 * DAY) : null,
      retentionEndsAt: o.retentionEndsIn !== undefined ? iso(now + o.retentionEndsIn) : null,
      usedBytes: Math.round((o.usedGB ?? 0) * GB),
      exit: null,
      access: null,
      accessPassword: null,
      activeConns: o.conns === undefined ? (ready ? 3 : null) : o.conns,
      usageReportedAt: ready ? iso(now - 4_000) : null,
      desiredVersion: 0,
      appliedVersion: 0,
      createdAt: iso(createdAt),
      firstReadyAt: started !== null ? iso(started) : null,
      releasedAt: o.lifecycle === 'released' ? iso(now - 2 * DAY) : null,
      deletedAt: null,
      createdFromOrderId: null,
      rulesVersion: rules[0]!.version,
      accessChangesLeft: 5,
      freeExitChangesLeft: o.freeExitLeft ?? 1,
      freeExitWindowEndsAt: (o.freeExitLeft ?? 1) === 0 ? iso(now + 12 * DAY) : null,
      usage: o.usage ?? (ready ? 'fresh' : 'none'),
      usageScale: o.usageScale ?? 0.4 * GB,
      accessVersions: [],
      exitBindings: [],
      events: [],
    }
    subs.push(sub)

    // 首购订单与账本
    const orderAt = createdAt - 20 * MIN
    const order = makeOrder({
      key: `new:${o.key}`,
      user: o.user,
      kind: 'new',
      at: orderAt,
      lines: [{ label: pv.name, detail: `${months} 个月 · 含 ${pv.includedBytes / GB} GB`, amount: { amount: pv.price.amount * months, currency: 'CNY' } }],
      payment: 'confirmed',
      fulfillment: o.lifecycle === 'pending' ? 'processing' : o.lifecycle === 'fulfill_failed' ? 'failed' : 'delivered',
      sub,
      planVersionId: pv.id,
      confirmedAt: orderAt + 15 * MIN,
      ...(ready ? { deliveredAt: started! } : {}),
      ...(o.lifecycle === 'fulfill_failed' ? { fulfillmentError: o.lifecycleReason ?? '无可售库存' } : {}),
    })
    sub.createdFromOrderId = order.id
    ledgerEntry(sub, 'time', months * MONTH_S, 'order', orderAt + 15 * MIN, { orderId: order.id, sourceId: order.id, reason: '新购' })
    ledgerEntry(sub, 'traffic', pv.includedBytes, 'order', orderAt + 15 * MIN, { orderId: order.id, sourceId: order.id, reason: '新购' })
    order.ledgerEntries = ledger.filter((l) => l.orderId === order.id)
    if (o.extraTrafficGB) {
      ledgerEntry(sub, 'traffic', o.extraTrafficGB * GB, 'gift', now - 6 * DAY, { reason: '首月体验补偿流量' })
    }

    if (!ready) return sub

    // 接入与出口
    const port = (portBase += 3)
    const user = `u${id.slice(4, 9)}`
    const pw = secretOf(id)
    const accId = fid('acc', `${o.key}:1`)
    sub.access = {
      id: accId,
      version: 1,
      state: 'active',
      host: nodes.find((n) => n.id === node)!.publicHost,
      username: user,
      ports: { http: port, https: port + 1, socks5: port + 2 },
      issuedAt: iso(started!),
      activatedAt: iso(started!),
    }
    sub.accessPassword = pw
    sub.accessVersions.push({
      id: accId,
      version: 1,
      changeKind: 'initial',
      state: 'active',
      username: user,
      ports: { http: port, https: port + 1, socks5: port + 2 },
      issuedAt: iso(started!),
      activatedAt: iso(started!),
      revokedAt: null,
      revokeReason: null,
      taskId: null,
    })
    if (o.exit && o.lifecycle !== 'released') {
      const res = upstream(`bound:${o.key}`, {
        node,
        plan: o.plan,
        host: `gw${subs.length}.isp-a.example.net`,
        port: 1080 + subs.length,
        provider: o.exit.provider ?? '示意供应商 A',
        status: 'bound',
        exit: { ip: o.exit.ip, countryCode: pv.countryCode, city: pv.city, latencyMs: o.exit.latencyMs ?? 180 },
        bound: id,
        createdAgo: (o.startedAgoDays ?? 1) * DAY + 2 * HOUR,
      })
      const checkedAt = now - (o.exit.checkedAgo ?? 6 * MIN)
      const exb = fid('exb', `${o.key}:1`)
      sub.exit = {
        bindingId: exb,
        version: 1,
        resourceId: res.u.id,
        provider: res.u.provider,
        upstreamProtocol: res.u.protocol,
        upstreamAddr: `${res.u.host}:${res.u.port}`,
        verifiedIp: o.exit.ip,
        ip: o.exit.check === 'failed' ? o.exit.ip : o.exit.ip,
        countryCode: pv.countryCode,
        country: pv.country,
        city: pv.city,
        check: o.exit.check ?? 'ok',
        latencyMs: o.exit.check === 'failed' ? null : (o.exit.latencyMs ?? 180),
        checkedAt: iso(checkedAt),
        checkError: o.exit.error ?? null,
        boundAt: iso(started!),
      }
      sub.exitBindings.push({
        id: exb,
        version: 1,
        state: 'active',
        resourceId: res.u.id,
        verifiedIp: o.exit.ip,
        verifiedCountryCode: pv.countryCode,
        check: {
          result: o.exit.check === 'failed' ? 'failed' : o.exit.check === 'slow' ? 'slow' : 'ok',
          ip: o.exit.check === 'failed' ? null : o.exit.ip,
          countryCode: pv.countryCode,
          city: pv.city,
          asn: 'AS64500 示意 ISP',
          latencyMs: o.exit.check === 'failed' ? null : (o.exit.latencyMs ?? 180),
          error: o.exit.error ?? null,
          checkedAt: iso(checkedAt),
        },
        boundAt: iso(started!),
        unboundAt: null,
        unboundReason: null,
        taskId: null,
      })
    }
    sub.desiredVersion = 3
    sub.appliedVersion = 3
    sub.events.push({
      id: fid('evt', `${o.key}:ready`),
      at: iso(started!),
      kind: 'ready',
      detail: `开通就绪，开始计时；${o.exit ? `出口 ${o.exit.ip}，` : ''}接入 v1`,
      customerVisible: true,
      orderId: order.id,
      taskId: null,
      actor: { kind: 'node', id: node },
    })
    return sub
  }

  // —— alice
  const tiktok = makeSub({
    key: '7k2m',
    name: 'TikTok 美区 1 号',
    user: alice,
    plan: 'us-lax',
    lifecycle: 'active',
    startedAgoDays: 37,
    months: 2,
    usedGB: 61.8,
    extraTrafficGB: 0,
    exit: { ip: '203.0.113.48', latencyMs: 182 },
    usage: 'fresh',
    usageScale: 0.9 * GB,
    conns: 14,
    freeExitLeft: 0,
  })
  const amazonDe = makeSub({
    key: 'd3fq',
    name: '亚马逊德国店',
    user: alice,
    plan: 'de-fra',
    node: nodeFra,
    lifecycle: 'active',
    startedAgoDays: 28,
    usedGB: 46.9,
    exit: { ip: '198.51.100.23', latencyMs: 236, checkedAgo: 47 * MIN },
    usage: 'stale',
    usageScale: 0.5 * GB,
    conns: null,
  })
  amazonDe.usageReportedAt = iso(now - 42 * MIN)
  const backup = makeSub({
    key: 'j9tx',
    name: '美区备用',
    user: alice,
    plan: 'us-lax',
    lifecycle: 'active',
    startedAgoDays: 12,
    usedGB: 50.0004,
    exit: { ip: '203.0.113.61', latencyMs: 171 },
    usage: 'fresh',
    usageScale: 0.3 * GB,
    conns: 0,
  })
  backup.events.push({
    id: fid('evt', 'j9tx:exhausted'),
    at: iso(now - 9 * HOUR),
    kind: 'exhausted',
    detail: '流量用尽，停止授权并断开 6 条连接（透支 412,330 字节）',
    customerVisible: true,
    orderId: null,
    taskId: null,
    actor: { kind: 'node', id: nodeLax },
  })

  // —— bob
  const japan = makeSub({
    key: 's4wp',
    name: '日本站点',
    user: bob,
    plan: 'jp-tyo',
    lifecycle: 'retained',
    startedAgoDays: 33,
    expiresIn: -3 * DAY,
    retentionEndsIn: 4 * DAY,
    usedGB: 31.2,
    exit: { ip: '192.0.2.77', latencyMs: 96 },
    usage: 'idle',
    conns: 0,
  })
  japan.usageReportedAt = iso(now - 5_000)
  const oldUs = makeSub({
    key: 'r8vd',
    name: '旧美区线路',
    user: bob,
    plan: 'us-nyc',
    lifecycle: 'released',
    startedAgoDays: 45,
    expiresIn: -15 * DAY,
    usedGB: 38,
    exit: null,
    usage: 'none',
    conns: null,
  })
  oldUs.access = null
  oldUs.accessPassword = null
  oldUs.accessVersions[0]!.state = 'revoked'
  oldUs.accessVersions[0]!.revokedAt = iso(now - 8 * DAY)
  oldUs.accessVersions[0]!.revokeReason = '保留期满释放'
  oldUs.usageReportedAt = null
  oldUs.lifecycleReason = '保留期满（7 天）'
  const nyc2 = makeSub({
    key: 'n2ry',
    name: '纽约 2 号',
    user: bob,
    plan: 'us-nyc',
    lifecycle: 'active',
    startedAgoDays: 9,
    usedGB: 12.4,
    exit: { ip: '203.0.113.120', latencyMs: 204 },
    usage: 'gappy',
    conns: 5,
  })

  // —— carol：管理员暂停
  const london = makeSub({
    key: 'p6hz',
    name: '伦敦数据采集',
    user: carol,
    plan: 'gb-lon',
    lifecycle: 'active',
    startedAgoDays: 18,
    usedGB: 22.7,
    exit: { ip: '198.51.100.140', latencyMs: 148, provider: '示意供应商 B' },
    paused: '对目标站点的请求速率异常，暂停待核实（工单 #2044）',
    conns: 0,
  })
  london.events.push({
    id: fid('evt', 'p6hz:paused'),
    at: iso(now - 5 * HOUR),
    kind: 'paused',
    detail: '管理员暂停：对目标站点的请求速率异常，暂停待核实（工单 #2044）',
    customerVisible: true,
    orderId: null,
    taskId: null,
    actor: opsActor,
  })

  // —— dave：账号封禁
  makeSub({
    key: 'b7cw',
    name: '批量注册',
    user: dave,
    plan: 'us-lax',
    lifecycle: 'active',
    startedAgoDays: 6,
    usedGB: 8.3,
    exit: { ip: '203.0.113.201', latencyMs: 190 },
    conns: 0,
  })

  // —— frank：待履约与履约异常
  const pendingSub = makeSub({ key: 'g5ln', name: '新 IP', user: frank, plan: 'us-nyc', lifecycle: 'pending', conns: null })
  makeSub({
    key: 'w3sx',
    name: '新加坡站点',
    user: frank,
    plan: 'sg-sin',
    lifecycle: 'fulfill_failed',
    lifecycleReason: '无可售库存（新加坡 0）',
    conns: null,
  })

  // —— grace：空闲（真实零值）与变更中
  const idle = makeSub({
    key: 'k4mt',
    name: '夜间任务',
    user: grace,
    plan: 'us-lax',
    lifecycle: 'active',
    startedAgoDays: 4,
    usedGB: 3.1,
    exit: { ip: '203.0.113.15', latencyMs: 177 },
    usage: 'idle',
    conns: 0,
  })
  const changing = makeSub({
    key: 'z8qe',
    name: '直播推流',
    user: grace,
    plan: 'us-lax',
    lifecycle: 'active',
    startedAgoDays: 15,
    usedGB: 27.5,
    exit: { ip: '203.0.113.92', latencyMs: 211, check: 'slow' },
    usageScale: 1.2 * GB,
    conns: 22,
  })

  // —— 其他用户（列表规模）
  const fillerPlans = ['us-lax', 'us-nyc', 'gb-lon', 'jp-tyo', 'us-lax', 'de-fra', 'us-nyc']
  fillers.forEach((u, i) => {
    const plan = fillerPlans[i % fillerPlans.length]!
    const exhausted = i === 3 || i === 9
    makeSub({
      key: `f${i}`,
      name: ['店铺后台', '广告投放', '社媒账号', '数据采集', '比价监控', '测试线路', '客服工作台'][i % 7]! + ` ${i + 1}`,
      user: u,
      plan,
      node: plan === 'de-fra' ? nodeFra : nodeLax,
      lifecycle: 'active',
      startedAgoDays: 2 + ((i * 7) % 27),
      usedGB: exhausted ? 50.0002 : 4 + ((i * 13) % 40),
      exit: {
        ip: plan.startsWith('us') ? `203.0.113.${130 + i}` : plan === 'jp-tyo' ? `192.0.2.${20 + i}` : `198.51.100.${60 + i}`,
        latencyMs: 120 + ((i * 17) % 120),
        ...(i === 5 ? { check: 'failed' as const, error: '经出口访问回显端点超时（10 s）' } : {}),
        checkedAgo: plan === 'de-fra' ? 47 * MIN : (3 + i) * MIN,
      },
      usage: plan === 'de-fra' ? 'stale' : 'fresh',
      usageScale: (0.15 + (i % 5) * 0.12) * GB,
      conns: plan === 'de-fra' ? null : exhausted ? 0 : (i * 3) % 11,
    })
  })
  for (const s of subs) if (s.nodeId === nodeFra && s.usageReportedAt) s.usageReportedAt = iso(now - 42 * MIN)

  // —— 候选上游（换出口向导）：各种拒绝原因都有
  upstream('cand-ok', {
    node: nodeLax,
    plan: 'us-lax',
    host: 'gw-c1.isp-b.example.net',
    port: 1080,
    provider: '示意供应商 B',
    status: 'available',
    exit: { ip: '203.0.113.77', countryCode: 'us', city: '洛杉矶', latencyMs: 164 },
    createdAgo: 2 * DAY,
  })
  upstream('cand-ok2', {
    node: nodeLax,
    plan: 'us-nyc',
    host: 'gw-c2.isp-b.example.net',
    port: 1080,
    provider: '示意供应商 B',
    status: 'available',
    exit: { ip: '203.0.113.88', countryCode: 'us', city: '纽约', latencyMs: 198 },
    createdAgo: 2 * DAY,
  })
  upstream('cand-same', {
    node: nodeLax,
    plan: 'us-lax',
    host: 'gw-c3.isp-a.example.net',
    port: 1081,
    status: 'available',
    note: '供应商称已换 IP，待复核',
    exit: { ip: '203.0.113.48', countryCode: 'us', city: '洛杉矶', latencyMs: 181 },
    createdAgo: 1 * DAY,
  })
  upstream('cand-taken', {
    node: nodeLax,
    plan: 'us-lax',
    host: 'gw-c4.isp-c.example.net',
    port: 3128,
    protocol: 'http',
    provider: '示意供应商 C',
    status: 'available',
    exit: { ip: '203.0.113.120', countryCode: 'us', city: '纽约', latencyMs: 230 },
    createdAgo: 3 * DAY,
  })
  upstream('cand-gb', {
    node: nodeLax,
    plan: 'gb-lon',
    host: 'gw-c5.isp-b.example.net',
    port: 1080,
    provider: '示意供应商 B',
    status: 'available',
    exit: { ip: '198.51.100.171', countryCode: 'gb', city: '伦敦', latencyMs: 152 },
    createdAgo: 4 * DAY,
  })
  upstream('cand-dead', {
    node: nodeLax,
    plan: 'us-lax',
    host: 'gw-c6.isp-c.example.net',
    port: 1080,
    provider: '示意供应商 C',
    status: 'pending_verify',
    verifiedOk: false,
    exit: { error: '上游认证失败：SOCKS5 用户名或密码被拒绝' },
    createdAgo: 3 * HOUR,
  })
  upstream('cand-flaky', {
    node: nodeLax,
    plan: 'us-lax',
    host: 'gw-c7.isp-a.example.net',
    port: 1082,
    status: 'available',
    note: '偶发断流，观察中',
    exit: { ip: '203.0.113.99', countryCode: 'us', city: '洛杉矶', latencyMs: 342 },
    failsOnApply: true,
    createdAgo: 5 * DAY,
  })
  upstream('jp-spare', {
    node: nodeLax,
    plan: 'jp-tyo',
    host: 'gw-j1.isp-b.example.net',
    port: 1080,
    provider: '示意供应商 B',
    status: 'available',
    exit: { ip: '192.0.2.140', countryCode: 'jp', city: '东京', latencyMs: 88 },
  })
  upstream('old-tiktok', {
    node: nodeLax,
    plan: 'us-lax',
    host: 'gw-o1.isp-a.example.net',
    port: 1080,
    status: 'cooldown',
    cooldownUntil: now - 18 * DAY + 7 * DAY + 2 * DAY,
    exit: { ip: '203.0.113.9', countryCode: 'us', city: '洛杉矶', latencyMs: 190 },
    statusReason: '换出口释放（REQ-1020-0003）',
    createdAgo: 40 * DAY,
  })
  upstream('quarantined', {
    node: nodeLax,
    plan: 'us-nyc',
    host: 'gw-q1.isp-c.example.net',
    port: 8080,
    protocol: 'http',
    provider: '示意供应商 C',
    status: 'quarantine',
    statusReason: '检测到出口 IP 被多家目标站点列入黑名单',
    exit: { ip: '203.0.113.240', countryCode: 'us', city: '纽约', latencyMs: 260 },
    createdAgo: 25 * DAY,
  })
  upstream('retired', {
    node: nodeFra,
    plan: 'de-fra',
    host: 'gw-r1.isp-a.example.net',
    port: 1080,
    status: 'retired',
    statusReason: '供应商合同到期',
    exit: { ip: '198.51.100.9', countryCode: 'de', city: '法兰克福', latencyMs: 230 },
    createdAgo: 58 * DAY,
  })
  upstream('ipv6', {
    node: nodeFra,
    plan: 'de-fra',
    host: '[2001:db8::42]',
    port: 1080,
    status: 'available',
    note: 'IPv6 入口，出口仍为 IPv4',
    exit: { ip: '198.51.100.31', countryCode: 'de', city: '法兰克福', latencyMs: 211 },
    createdAgo: 6 * DAY,
  })
  const reserved = upstream('reserved-nyc', {
    node: nodeLax,
    plan: 'us-nyc',
    host: 'gw-c8.isp-b.example.net',
    port: 1080,
    provider: '示意供应商 B',
    status: 'reserved',
    exit: { ip: '203.0.113.52', countryCode: 'us', city: '纽约', latencyMs: 207 },
  })

  // 历史：TikTok 1 号换过一次出口和一次接入
  const tikExit = tiktok.exitBindings[0]!
  tiktok.exitBindings.unshift({
    ...tikExit,
    id: fid('exb', '7k2m:0'),
    version: 1,
    state: 'unbound',
    resourceId: upstreams.find((u) => u.u.host === 'gw-o1.isp-a.example.net')!.u.id,
    verifiedIp: '203.0.113.9',
    check: { ...tikExit.check!, ip: '203.0.113.9', checkedAt: iso(now - 18 * DAY - HOUR) },
    boundAt: iso(now - 37 * DAY),
    unboundAt: iso(now - 18 * DAY),
    unboundReason: '换出口（申请 REQ-1020-0003）',
    taskId: fid('tsk', '7k2m:exit'),
  })
  tikExit.version = 2
  tikExit.boundAt = iso(now - 18 * DAY)
  tikExit.taskId = fid('tsk', '7k2m:exit')
  tiktok.exit!.version = 2
  tiktok.exit!.boundAt = iso(now - 18 * DAY)
  tiktok.exitBindings.reverse()
  const tikAcc = tiktok.accessVersions[0]!
  tikAcc.state = 'revoked'
  tikAcc.revokedAt = iso(now - 18 * DAY + HOUR)
  tikAcc.revokeReason = '更换接入（端口与密码）'
  const tikAcc2: AccessVersion = {
    ...tikAcc,
    id: fid('acc', '7k2m:2'),
    version: 2,
    changeKind: 'ports_and_password',
    state: 'active',
    ports: { http: 31080, https: 31081, socks5: 31082 },
    issuedAt: iso(now - 18 * DAY + HOUR),
    activatedAt: iso(now - 18 * DAY + HOUR),
    revokedAt: null,
    revokeReason: null,
    taskId: fid('tsk', '7k2m:acc'),
  }
  tiktok.accessVersions.unshift(tikAcc2)
  tiktok.access = { ...tiktok.access!, id: tikAcc2.id, version: 2, ports: tikAcc2.ports, issuedAt: tikAcc2.issuedAt, activatedAt: tikAcc2.activatedAt }
  tiktok.desiredVersion = 7
  tiktok.appliedVersion = 7
  tiktok.events.push(
    {
      id: fid('evt', '7k2m:exit'),
      at: iso(now - 18 * DAY),
      kind: 'exit_changed',
      detail: '出口 203.0.113.9 → 203.0.113.48（申请 REQ-1020-0003）；接入未变',
      customerVisible: true,
      orderId: null,
      taskId: fid('tsk', '7k2m:exit'),
      actor: opsActor,
    },
    {
      id: fid('evt', '7k2m:acc'),
      at: iso(now - 18 * DAY + HOUR),
      kind: 'access_changed',
      detail: '接入 v1 → v2：端口与密码已更换；出口未变',
      customerVisible: true,
      orderId: null,
      taskId: fid('tsk', '7k2m:acc'),
      actor: { kind: 'user', id: alice.id, email: alice.email },
    },
  )

  // 含流量续费：TikTok 1 号 3 天前续时长 + 50 GB
  const renewTik = makeOrder({
    key: 'renew:7k2m',
    user: alice,
    kind: 'renew_time',
    at: now - 3 * DAY - 30 * MIN,
    lines: [
      { label: '续时长', detail: '1 个月', amount: cny(168) },
      { label: '流量包', detail: '50 GB', amount: cny(75) },
    ],
    payment: 'confirmed',
    fulfillment: 'delivered',
    sub: tiktok,
    confirmedAt: now - 3 * DAY,
    deliveredAt: now - 3 * DAY,
  })
  ledgerEntry(tiktok, 'time', MONTH_S, 'order', now - 3 * DAY, { orderId: renewTik.id, sourceId: renewTik.id, reason: '续时长' })
  ledgerEntry(tiktok, 'traffic', 50 * GB, 'order', now - 3 * DAY, { orderId: renewTik.id, sourceId: renewTik.id, reason: '流量包 50 GB' })
  renewTik.ledgerEntries = ledger.filter((l) => l.orderId === renewTik.id)
  tiktok.events.push(
    { id: fid('evt', '7k2m:time'), at: iso(now - 3 * DAY), kind: 'time_added', detail: '续时长 +30 天，到期时间顺延', customerVisible: true, orderId: renewTik.id, taskId: null, actor: opsActor },
    { id: fid('evt', '7k2m:traffic'), at: iso(now - 3 * DAY), kind: 'traffic_added', detail: '买流量 +50 GB', customerVisible: true, orderId: renewTik.id, taskId: null, actor: opsActor },
  )
  // 一次补偿（补时，D-21）
  ledgerEntry(tiktok, 'time', 2 * 3_600, 'compensation', now - 10 * DAY, {
    reason: '10-28 上游中断 2 小时，按实际时长补时（D-21）',
  })

  // ---------------------------------------------------------------- 待处理订单（概览置顶）
  const nycPlan = planBy('us-nyc').versions[0]!
  const awaiting1 = makeOrder({
    key: 'await:hank',
    user: hank,
    kind: 'new',
    at: now - 2 * HOUR,
    lines: [{ label: nycPlan.name, detail: '1 个月 · 含 50 GB', amount: nycPlan.price }],
    payment: 'awaiting',
    fulfillment: 'not_started',
    planVersionId: nycPlan.id,
    claimedAgo: 30 * MIN,
  })
  reserved.u.reservedOrderId = awaiting1.id
  reserved.u.reservedUntil = awaiting1.holdUntil
  makeOrder({
    key: 'await:alice-de',
    user: alice,
    kind: 'renew_time',
    at: now - 50 * MIN,
    lines: [{ label: '续时长', detail: '1 个月', amount: cny(188) }],
    payment: 'awaiting',
    fulfillment: 'not_started',
    sub: amazonDe,
  })
  makeOrder({
    key: 'await:bob-traffic',
    user: bob,
    kind: 'add_traffic',
    at: now - 4 * HOUR,
    lines: [{ label: '流量包', detail: '50 GB', amount: cny(75) }],
    payment: 'awaiting',
    fulfillment: 'not_started',
    sub: nyc2,
    claimedAgo: 3 * HOUR,
  })
  makeOrder({
    key: 'await:backup-traffic',
    user: alice,
    kind: 'add_traffic',
    at: now - 8 * HOUR,
    lines: [{ label: '流量包', detail: '10 GB', amount: cny(18) }],
    payment: 'awaiting',
    fulfillment: 'not_started',
    sub: backup,
  })
  // 超时取消后晚到款
  const jpPlan = planBy('jp-tyo').versions[0]!
  const late = makeOrder({
    key: 'late:grace',
    user: grace,
    kind: 'new',
    at: now - 27 * HOUR,
    lines: [{ label: jpPlan.name, detail: '3 个月 · 含 100 GB', amount: { amount: jpPlan.price.amount * 3, currency: 'CNY' } }],
    payment: 'cancelled',
    fulfillment: 'not_started',
    planVersionId: jpPlan.id,
    cancelReason: 'timeout',
    claimedAgo: 2 * HOUR,
  })
  // 退款复核中
  const londonOrder = orders.find((o) => o.subscriptionId === london.id && o.kind === 'new')!
  londonOrder.payment = 'refund_review'
  const refundReview: Refund = {
    id: fid('rfd', 'london'),
    orderId: londonOrder.id,
    status: 'review',
    amount: cny(80),
    reclaimBytes: 27 * GB,
    reclaimSeconds: 12 * 86_400,
    reason: '客户要求提前终止，按剩余时长与流量比例退款',
    externalRef: null,
    requestedBy: opsRef,
    requestedAt: iso(now - 6 * HOUR),
    decidedBy: null,
    decidedAt: null,
    decisionNote: null,
  }
  londonOrder.refunds = [refundReview]
  // 已退款
  const daveOrder = makeOrder({
    key: 'dave:traffic',
    user: dave,
    kind: 'add_traffic',
    at: now - 4 * DAY,
    lines: [{ label: '流量包', detail: '10 GB', amount: cny(18) }],
    payment: 'refunded',
    fulfillment: 'delivered',
    sub: subs.find((s) => s.userId === dave.id)!,
    confirmedAt: now - 4 * DAY + HOUR,
    deliveredAt: now - 4 * DAY + HOUR,
  })
  const refundDone: Refund = {
    id: fid('rfd', 'dave'),
    orderId: daveOrder.id,
    status: 'approved',
    amount: cny(18),
    reclaimBytes: 10 * GB,
    reclaimSeconds: 0,
    reason: '重复购买',
    externalRef: 'REFUND-20261105-0007',
    requestedBy: opsRef,
    requestedAt: iso(now - 3 * DAY),
    decidedBy: opsRef,
    decidedAt: iso(now - 3 * DAY + 2 * HOUR),
    decisionNote: '核对无误',
  }
  daveOrder.refunds = [refundDone]
  const daveSub = subs.find((s) => s.userId === dave.id)!
  ledgerEntry(daveSub, 'traffic', 10 * GB, 'order', now - 4 * DAY + HOUR, { orderId: daveOrder.id, sourceId: daveOrder.id, reason: '流量包 10 GB' })
  ledgerEntry(daveSub, 'traffic', -10 * GB, 'refund', now - 3 * DAY + 2 * HOUR, { refundId: refundDone.id, sourceId: refundDone.id, orderId: daveOrder.id, reason: '退款回收：重复购买' })
  daveOrder.ledgerEntries = ledger.filter((l) => l.orderId === daveOrder.id)

  // ---------------------------------------------------------------- 任务
  const tasks: TaskRow[] = []
  const task = (key: string, t: Partial<Task> & Pick<Task, 'subscriptionId' | 'kind' | 'state'>, polls = 99): TaskRow => {
    const row: TaskRow = {
      t: {
        id: fid('tsk', key),
        requestedBy: opsActor,
        orderId: null,
        exitRequestId: null,
        newAccessVersion: null,
        newExitVersion: null,
        targetVersion: null,
        attempts: 1,
        failureCode: null,
        failureReason: null,
        createdAt: iso(now - HOUR),
        startedAt: iso(now - HOUR),
        finishedAt: iso(now - HOUR + 3_000),
        ...t,
      },
      polls,
    }
    tasks.push(row)
    return row
  }
  task('7k2m:exit', { subscriptionId: tiktok.id, kind: 'exit_change', state: 'succeeded', newExitVersion: 2, targetVersion: 5, createdAt: iso(now - 18 * DAY), startedAt: iso(now - 18 * DAY), finishedAt: iso(now - 18 * DAY + 4_000) })
  task('7k2m:acc', { subscriptionId: tiktok.id, kind: 'access_change', state: 'succeeded', newAccessVersion: 2, targetVersion: 6, requestedBy: { kind: 'user', id: alice.id, email: alice.email }, createdAt: iso(now - 18 * DAY + HOUR), startedAt: iso(now - 18 * DAY + HOUR), finishedAt: iso(now - 18 * DAY + HOUR + 2_000) })
  task('7k2m:failed', {
    subscriptionId: tiktok.id,
    kind: 'exit_change',
    state: 'failed',
    failureCode: 'exit_unverified',
    failureReason: '经客户入口复核出口失败，已回到原绑定',
    createdAt: iso(now - 25 * DAY),
    startedAt: iso(now - 25 * DAY),
    finishedAt: iso(now - 25 * DAY + 11_000),
  })
  const changingTask = task(
    'z8qe:acc',
    { subscriptionId: changing.id, kind: 'access_change', state: 'running', newAccessVersion: 2, targetVersion: 4, createdAt: iso(now - 40_000), startedAt: iso(now - 38_000), finishedAt: null },
    3,
  )
  changing.desiredVersion = 4
  task('g5ln:fulfill', { subscriptionId: pendingSub.id, kind: 'fulfill', state: 'running', orderId: pendingSub.createdFromOrderId, createdAt: iso(now - 2 * HOUR), startedAt: iso(now - 2 * HOUR), finishedAt: null }, 4)
  const failedFulfill = subs.find((s) => s.lifecycle === 'fulfill_failed')!
  task('w3sx:fulfill', {
    subscriptionId: failedFulfill.id,
    kind: 'fulfill',
    state: 'failed',
    orderId: failedFulfill.createdFromOrderId,
    failureCode: 'no_stock',
    failureReason: '无可售库存（新加坡 0），已回收端口',
    createdAt: iso(now - 90 * MIN),
    startedAt: iso(now - 90 * MIN),
    finishedAt: iso(now - 89 * MIN),
  })
  failedFulfill.events.push({
    id: fid('evt', 'w3sx:failed'),
    at: iso(now - 89 * MIN),
    kind: 'fulfill_failed',
    detail: '开通失败：无可售库存（新加坡 0），已回收端口',
    customerVisible: false,
    orderId: failedFulfill.createdFromOrderId,
    taskId: fid('tsk', 'w3sx:fulfill'),
    actor: SYSTEM,
  })

  // ---------------------------------------------------------------- 换出口申请
  const req = (
    key: string,
    no: string,
    sub: SubRow,
    o: Partial<AdminExitRequest> & Pick<AdminExitRequest, 'status' | 'reason' | 'charge' | 'feeStatus'>,
  ): AdminExitRequest => {
    const u = users.find((x) => x.id === sub.userId)!
    return {
      id: fid('req', key),
      no,
      subscriptionId: sub.id,
      subscriptionName: sub.name,
      createdAt: iso(now - HOUR),
      updatedAt: iso(now - HOUR),
      note: '',
      alsoChangeAccess: false,
      oldIp: sub.exit?.verifiedIp ?? '—',
      newIp: null,
      failureReason: null,
      user: { id: u.id, email: u.email },
      feeAmount: null,
      feeExternalRef: null,
      internalNote: '',
      taskId: null,
      handledBy: null,
      acceptedAt: null,
      finishedAt: null,
      ...o,
    }
  }
  const exitRequests: AdminExitRequest[] = [
    req('nyc2', 'REQ-1107-0008', nyc2, {
      status: 'pending',
      reason: 'quality',
      charge: 'free',
      feeStatus: 'not_applicable',
      note: '晚高峰延迟明显升高，经常超时',
      createdAt: iso(now - 70 * MIN),
      updatedAt: iso(now - 70 * MIN),
    }),
    req('tiktok', 'REQ-1107-0009', tiktok, {
      status: 'pending',
      reason: 'target_blocked',
      charge: 'manual',
      feeStatus: 'awaiting_decision',
      alsoChangeAccess: true,
      note: '账号登录提示 IP 风险，想同时换连接',
      createdAt: iso(now - 25 * MIN),
      updatedAt: iso(now - 25 * MIN),
    }),
    req('idle', 'REQ-1106-0005', idle, {
      status: 'processing',
      reason: 'region',
      charge: 'free',
      feeStatus: 'not_applicable',
      note: '希望换到洛杉矶以外的城市',
      createdAt: iso(now - 20 * HOUR),
      updatedAt: iso(now - 19 * HOUR),
      acceptedAt: iso(now - 19 * HOUR),
      handledBy: opsRef,
      internalNote: '等供应商 B 补货后执行',
    }),
    req('tiktok-old', 'REQ-1020-0003', tiktok, {
      status: 'succeeded',
      reason: 'target_blocked',
      charge: 'free',
      feeStatus: 'not_applicable',
      oldIp: '203.0.113.9',
      newIp: '203.0.113.48',
      createdAt: iso(now - 18 * DAY - 3 * HOUR),
      updatedAt: iso(now - 18 * DAY),
      acceptedAt: iso(now - 18 * DAY - HOUR),
      finishedAt: iso(now - 18 * DAY),
      handledBy: opsRef,
      taskId: fid('tsk', '7k2m:exit'),
    }),
    req('rejected', 'REQ-1101-0002', london, {
      status: 'failed',
      reason: 'other',
      charge: 'free',
      feeStatus: 'not_applicable',
      note: '想换成美国 IP',
      failureReason: '换出口只能在同一国家内进行；需要美国 IP 请另购美国套餐。',
      internalNote: '跨国家，驳回',
      createdAt: iso(now - 6 * DAY),
      updatedAt: iso(now - 6 * DAY + 2 * HOUR),
      finishedAt: iso(now - 6 * DAY + 2 * HOUR),
      handledBy: opsRef,
    }),
    req('cancelled', 'REQ-1103-0004', backup, {
      status: 'cancelled',
      reason: 'quality',
      charge: 'free',
      feeStatus: 'not_applicable',
      createdAt: iso(now - 4 * DAY),
      updatedAt: iso(now - 4 * DAY + HOUR),
      finishedAt: iso(now - 4 * DAY + HOUR),
    }),
  ]

  // ---------------------------------------------------------------- 告警
  const alert = (key: string, a: Partial<Alert> & Pick<Alert, 'kind' | 'severity' | 'status' | 'message'>): Alert => ({
    id: fid('alt', key),
    details: {},
    nodeId: null,
    subscriptionId: null,
    resourceId: null,
    orderId: null,
    taskId: null,
    occurrences: 1,
    firstSeenAt: iso(now - HOUR),
    lastSeenAt: iso(now - 2 * MIN),
    ackedBy: null,
    ackedAt: null,
    ackNote: null,
    resolvedAt: null,
    ...a,
  })
  const failedExitSub = subs.find((s) => s.exit?.check === 'failed')!
  const alerts: Alert[] = [
    alert('fra-offline', {
      kind: 'node_offline',
      severity: 'critical',
      status: 'open',
      message: '节点 fra-01 已 42 分钟没有上报；数据面按本地已知额度与期限继续执行',
      nodeId: nodeFra,
      occurrences: 1,
      firstSeenAt: iso(now - 42 * MIN),
      lastSeenAt: iso(now - 15_000),
      details: { lastReportAt: iso(now - 42 * MIN), appliedRevision: 975, desiredRevision: 977 },
    }),
    alert('no-stock', {
      kind: 'fulfill_failed',
      severity: 'critical',
      status: 'open',
      message: '订单开通失败：新加坡无可售库存',
      subscriptionId: failedFulfill.id,
      orderId: failedFulfill.createdFromOrderId,
      taskId: fid('tsk', 'w3sx:fulfill'),
      firstSeenAt: iso(now - 89 * MIN),
      lastSeenAt: iso(now - 89 * MIN),
    }),
    alert('upstream', {
      kind: 'upstream_error',
      severity: 'warning',
      status: 'open',
      message: `出口检测失败：${failedExitSub.exit!.checkError}`,
      subscriptionId: failedExitSub.id,
      resourceId: failedExitSub.exit!.resourceId,
      occurrences: 4,
      firstSeenAt: iso(now - 26 * MIN),
    }),
    alert('late', {
      kind: 'late_payment',
      severity: 'warning',
      status: 'open',
      message: `用户反馈已转账，但订单 ${late.no} 已超时取消，需人工处置（重新履约或退款）`,
      orderId: late.id,
      firstSeenAt: iso(now - 2 * HOUR),
      lastSeenAt: iso(now - 2 * HOUR),
    }),
    alert('stock-sg', {
      kind: 'stock_low',
      severity: 'info',
      status: 'open',
      message: '套餐 新加坡 · 新加坡 包月 可售库存为 0，前台显示“缺货”',
      firstSeenAt: iso(now - 2 * DAY),
      lastSeenAt: iso(now - 5 * MIN),
      occurrences: 1,
    }),
    alert('cert', {
      kind: 'cert_risk',
      severity: 'warning',
      status: 'acked',
      message: '节点 lax-01 的 HTTPS 入口证书 9 天后到期',
      nodeId: nodeLax,
      firstSeenAt: iso(now - DAY),
      lastSeenAt: iso(now - 10 * MIN),
      ackedBy: opsRef,
      ackedAt: iso(now - 20 * HOUR),
      ackNote: '已提交续期，预计明天生效',
    }),
    alert('skew', {
      kind: 'clock_skew',
      severity: 'warning',
      status: 'open',
      message: '节点 sjc-02 时钟偏差 1.48 秒，超过 1 秒阈值',
      nodeId: nodeSjc,
      details: { clockSkewMs: 1480 },
      firstSeenAt: iso(now - 3 * HOUR),
    }),
    alert('persist', {
      kind: 'persist_failed',
      severity: 'critical',
      status: 'resolved',
      message: '节点 lax-01 计量持久化失败（磁盘写入超时），已停止新的字节授权',
      nodeId: nodeLax,
      firstSeenAt: iso(now - 3 * DAY),
      lastSeenAt: iso(now - 3 * DAY + 4 * MIN),
      resolvedAt: iso(now - 3 * DAY + 4 * MIN),
      occurrences: 2,
    }),
  ]

  // ---------------------------------------------------------------- 审计
  const audit: AuditLog[] = []
  const log = (at: number, action: string, targetType: string | null, targetId: string | null, details: Record<string, unknown> = {}, actor: Actor = opsActor, outcome: AuditLog['outcome'] = 'ok') =>
    audit.push({ id: audit.length + 1, at: iso(at), actor, action, targetType, targetId, outcome, ip: actor.kind === 'admin' ? '192.0.2.10' : null, userAgent: actor.kind === 'admin' ? 'Mozilla/5.0（示意）' : null, details })
  log(now - 18 * DAY, 'exit.change', 'subscription', tiktok.id, { from: '203.0.113.9', to: '203.0.113.48', exitRequest: 'REQ-1020-0003' })
  log(now - 10 * DAY, 'ledger.create', 'subscription', tiktok.id, { sourceType: 'compensation', kind: 'time', amount: 7200 })
  log(now - 4 * DAY + HOUR, 'order.confirm_payment', 'order', daveOrder.id, { no: daveOrder.no, amount: 1800 })
  log(now - 3 * DAY, 'order.confirm_payment', 'order', renewTik.id, { no: renewTik.no, amount: 24300 })
  log(now - 3 * DAY + 2 * HOUR, 'refund.decide', 'refund', refundDone.id, { approve: true })
  log(now - 2 * DAY, 'user.ban', 'user', dave.id, { reason: dave.bannedReason, revokedSessions: 2 })
  log(now - 30 * HOUR, 'rules.create', 'rules', 'R-2026-11-08', {}, SYSTEM)
  log(now - 20 * HOUR, 'rules.create', 'rules', 'R-2026-11-08.1', { paymentConfigured: true })
  log(now - 20 * HOUR, 'admin.login', 'admin', ops.id, { mfa: 'totp' })
  log(now - 19 * HOUR, 'exit_request.accept', 'exit_request', fid('req', 'idle'), { fee: 'free' })
  log(now - 6 * HOUR, 'refund.request', 'refund', refundReview.id, { orderId: londonOrder.id, amount: 8000 })
  log(now - 5 * HOUR, 'subscription.pause', 'subscription', london.id, { reason: london.pausedReason })
  log(now - 5 * HOUR, 'user.create', 'user', fid('usr', 'erin'), { email: 'erin@example.net' })
  log(now - 4 * HOUR, 'access.reveal', 'subscription', nyc2.id, { reason: '客户电话求助，核对连接配置', plaintextLogged: false })
  log(now - 3 * HOUR, 'admin.login', 'admin', viewer.id, { mfa: 'totp' }, { kind: 'admin', id: viewer.id, email: viewer.email })
  log(now - 3 * HOUR, 'order.confirm_payment', 'order', orders[0]!.id, { reason: '只读账号无写权限' }, { kind: 'admin', id: viewer.id, email: viewer.email }, 'denied')
  log(now - 90 * MIN, 'fulfill.fail', 'subscription', failedFulfill.id, { reason: '无可售库存' }, SYSTEM, 'failed')
  log(now - 40_000, 'access.change', 'subscription', changing.id, { mode: 'ports_and_password', reason: '客户报告连接泄露' })
  log(now - 31 * MIN, 'admin.login', 'admin', ops.id, { mfa: 'failed' }, opsActor, 'denied')
  log(now - 30 * MIN, 'admin.login', 'admin', ops.id, { mfa: 'totp' })
  audit.reverse()
  void changingTask

  return {
    admins,
    users,
    subs,
    ledger,
    plans,
    packs,
    rules,
    upstreams,
    orders,
    refunds: [refundReview, refundDone],
    exitRequests,
    tasks,
    probes: [],
    transfers: [],
    nodes,
    alerts,
    audit,
    seq: 0,
  }
}

function codes(seed: string): string[] {
  return Array.from({ length: 10 }, (_, i) => {
    const s = secretOf(`${seed}:${i}`, 10).toLowerCase().replace(/[^a-z0-9]/g, 'x')
    return `${s.slice(0, 5)}-${s.slice(5, 10)}`
  })
}

export function recoveryCodesFor(seed: string): string[] {
  return codes(seed)
}
