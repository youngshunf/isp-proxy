// 管理端内存 mock：实现完整的 AdminApi，并在“服务端”一侧执行关键规则，界面可以在没有后端时开发与测试。
// 模拟的规则（与 api/openapi.yaml 的描述一致）：
//   - 登录两步：密码 → TOTP（首次绑定返回密钥与恢复码）；会话过期后一律 401；只读角色写操作 403。
//   - 确认收款以订单为业务键只执行一次：重复确认返回已有记录，请求体不同 409，金额不符 422，已取消订单须标记晚到款。
//   - 账本只追加：赠送/补偿必须为正，扣回不能让合计为负；划转只限同一用户、来源须有剩余。
//   - 换出口：候选须为可售/待验证且未被占用；提交须引用该订阅该资源最近一次通过的比较探测；每订阅同一时刻只有一个进行中任务。
//   - 上游状态只允许规定的人工转换；修改连接信息回到待验证，已占用的资源不能改连接信息。
//   - 幂等键：同一键同一请求体返回首次结果，请求体不同 409。
// 任务与探测按“被查询的次数”推进（jobSteps），不依赖真实时间，测试可控。
import { ApiError, type ApiErrorCode } from '../../index'
import { MOCK_NOW } from '../../mock'
import type {
  AccessVersion,
  Actor,
  AdminApi,
  AdminExitRequest,
  AdminOrder,
  AdminPlan,
  AdminSubscription,
  AdminSubscriptionDetail,
  AdminSubscriptionEvent,
  AdminTrafficPack,
  AdminUser,
  AdminUserDetail,
  Blocker,
  LedgerEntry,
  Node,
  Overview,
  Page,
  Probe,
  Task,
  Upstream,
  UsagePoint,
  UsageSeries,
} from '../types'
import { fid, payRefOf, rng, secretOf } from './ids'
import {
  DAY,
  GB,
  HOUR,
  MIN,
  MOCK_ADMIN_LOGIN,
  MONTH_S,
  SYSTEM,
  iso,
  recoveryCodesFor,
  seed,
  type Db,
  type ProbeRow,
  type SubRow,
  type TaskRow,
  type UpstreamRow,
  type UserRow,
} from './seed'

export { MOCK_ADMIN_LOGIN } from './seed'
export { MOCK_NOW }

export interface MockAdminOptions {
  /** 模拟网络延迟区间（毫秒），测试中传 [0, 0]。 */
  latency?: [number, number]
  /** 服务端“当前时间”，固定后截图与测试稳定。 */
  now?: number
  /** 初始即登录（测试用）；'viewer' 为只读角色。 */
  signedIn?: boolean | 'viewer'
  /** 任务与探测需要被查询几次才完成，默认 2（界面能看到“执行中”）；测试可设 1。 */
  jobSteps?: number
}

export interface MockAdminControls {
  /** 让当前会话失效（模拟过期或被撤销），之后的请求返回 401。 */
  expireSession(): void
  /** 直接进入已登录状态。 */
  signIn(role?: 'admin' | 'viewer'): void
  readonly now: number
}

export type MockAdminApi = AdminApi & { mock: MockAdminControls }

const BLOCKER_ORDER: Blocker[] = [
  'account_banned',
  'released',
  'admin_paused',
  'pending',
  'fulfill_failed',
  'expired',
  'no_paid_subscription',
  'traffic_exhausted',
  'changing',
  'dataplane',
]

const fail = (code: ApiErrorCode, message: string, details?: Record<string, unknown>): never => {
  throw new ApiError(code, message, details ? { details } : {})
}

const clone = <T,>(x: T): T => structuredClone(x)

function stableHash(x: unknown): string {
  return JSON.stringify(x, (_, v) => (v && typeof v === 'object' && !Array.isArray(v) ? Object.fromEntries(Object.entries(v).sort()) : v))
}

function paginate<T>(items: T[], query: { cursor?: string; limit?: number } | undefined): Page<T> {
  const start = query?.cursor ? Number(query.cursor) || 0 : 0
  const limit = Math.min(200, Math.max(1, query?.limit ?? 50))
  const slice = items.slice(start, start + limit)
  return { items: slice, nextCursor: start + limit < items.length ? String(start + limit) : null }
}

const asArray = <T,>(v: T | T[] | undefined): T[] | undefined => (v === undefined ? undefined : Array.isArray(v) ? v : [v])

export function createMockAdminApi(opts: MockAdminOptions = {}): MockAdminApi {
  const now = opts.now ?? MOCK_NOW
  const [minLat, maxLat] = opts.latency ?? [120, 380]
  const steps = Math.max(1, opts.jobSteps ?? 2)
  const db: Db = seed(now)
  const idem = new Map<string, { hash: string; result: unknown }>()

  const session: { stage: 'none' | 'mfa' | 'ok'; adminId: string | null } = { stage: 'none', adminId: null }
  if (opts.signedIn) {
    session.stage = 'ok'
    session.adminId = db.admins.find((a) => a.admin.role === (opts.signedIn === 'viewer' ? 'viewer' : 'admin'))!.admin.id
  }

  const wait = <T,>(fn: () => T): Promise<T> =>
    new Promise((resolve, reject) => {
      const ms = minLat + Math.random() * (maxLat - minLat)
      const run = () => {
        try {
          resolve(clone(fn()))
        } catch (e) {
          reject(e)
        }
      }
      if (ms <= 0) queueMicrotask(run)
      else setTimeout(run, ms)
    })

  const nextId = (prefix: string) => fid(prefix, `run:${++db.seq}`)

  // ---------------------------------------------------------------- 会话与权限
  const me = () => {
    if (session.stage !== 'ok' || !session.adminId) fail('unauthorized', '登录已失效，请重新登录')
    return db.admins.find((a) => a.admin.id === session.adminId)!
  }
  const actor = (): Actor => {
    const a = me().admin
    return { kind: 'admin', id: a.id, email: a.email }
  }
  const adminRef = () => {
    const a = me().admin
    return { id: a.id, email: a.email }
  }
  const audit = (action: string, targetType: string | null, targetId: string | null, details: Record<string, unknown> = {}, outcome: 'ok' | 'denied' | 'failed' = 'ok') => {
    db.audit.unshift({
      id: db.audit.length + 1,
      at: iso(now),
      actor: session.adminId ? actor() : SYSTEM,
      action,
      targetType,
      targetId,
      outcome,
      ip: '192.0.2.10',
      userAgent: 'Mozilla/5.0（示意）',
      details,
    })
  }
  /** 写操作：已登录且不是只读角色。 */
  const writer = (action: string, targetType: string | null = null, targetId: string | null = null) => {
    const a = me()
    if (a.admin.role !== 'admin') {
      audit(action, targetType, targetId, { reason: '只读账号无写权限' }, 'denied')
      fail('forbidden', '当前账号是只读角色，不能执行这个操作')
    }
    return a
  }
  const once = <T,>(scope: string, key: string | undefined, body: unknown, fn: () => T): T => {
    if (!key) fail('invalid', '缺少 Idempotency-Key')
    const k = `${scope}:${key}`
    const h = stableHash(body)
    const hit = idem.get(k)
    if (hit) {
      if (hit.hash !== h) fail('conflict', '同一幂等键对应了不同的请求内容')
      return hit.result as T
    }
    const result = fn()
    idem.set(k, { hash: h, result: clone(result) })
    return result
  }

  // ---------------------------------------------------------------- 查找
  const findSub = (id: string) => db.subs.find((s) => s.id === id) ?? fail('not_found', '没有找到这个订阅')
  const findUser = (id: string) => db.users.find((u) => u.id === id) ?? fail('not_found', '没有找到这个用户')
  const findOrder = (id: string) => db.orders.find((o) => o.id === id) ?? fail('not_found', '没有找到这个订单')
  const findUpstream = (id: string) => db.upstreams.find((u) => u.u.id === id) ?? fail('not_found', '没有找到这个上游资源')
  const findReq = (id: string) => db.exitRequests.find((r) => r.id === id) ?? fail('not_found', '没有找到这个申请')
  const userOf = (s: SubRow) => db.users.find((u) => u.id === s.userId)!

  // ---------------------------------------------------------------- 推导
  const sums = (subId: string) => {
    let bytes = 0
    let seconds = 0
    for (const l of db.ledger) {
      if (l.subscriptionId !== subId) continue
      if (l.entitlementKind === 'traffic') bytes += l.amount
      else seconds += l.amount
    }
    return { bytes, seconds }
  }
  const activeTask = (subId: string) => db.tasks.find((t) => t.t.subscriptionId === subId && (t.t.state === 'queued' || t.t.state === 'running'))
  const nodeOnline = (id: string | null) => (id ? (db.nodes.find((n) => n.id === id)?.online ?? false) : false)

  /** 直出代理的可用截止（D-25）：该用户时长有效（已开始、未到期、未删除）的 ISP 订阅中最晚的到期时间。 */
  const paidUntil = (userId: string): string | null => {
    let best: string | null = null
    for (const x of db.subs) {
      if (x.userId !== userId || x.direct || x.lifecycle === 'deleted' || !x.startedAt || !x.expiresAt) continue
      if (Date.parse(x.expiresAt) <= now) continue
      if (best === null || x.expiresAt > best) best = x.expiresAt
    }
    return best
  }

  const blockersOf = (s: SubRow): Blocker[] => {
    const set = new Set<Blocker>()
    if (s.direct) {
      if (userOf(s).status === 'banned') set.add('account_banned')
      if (s.adminPaused) set.add('admin_paused')
      if (s.lifecycle === 'pending') set.add('pending')
      if (s.lifecycle === 'fulfill_failed') set.add('fulfill_failed')
      if (s.lifecycle === 'active' && !paidUntil(s.userId)) set.add('no_paid_subscription')
      const t = activeTask(s.id)
      if (t) set.add('changing')
      if (s.lifecycle === 'active' && s.nodeId && !nodeOnline(s.nodeId)) set.add('dataplane')
      return BLOCKER_ORDER.filter((b) => set.has(b))
    }
    if (userOf(s).status === 'banned') set.add('account_banned')
    if (s.lifecycle === 'released' || s.lifecycle === 'deleted') set.add('released')
    if (s.adminPaused) set.add('admin_paused')
    if (s.lifecycle === 'pending') set.add('pending')
    if (s.lifecycle === 'fulfill_failed') set.add('fulfill_failed')
    if (s.lifecycle === 'retained') set.add('expired')
    const { bytes } = sums(s.id)
    if ((s.lifecycle === 'active' || s.lifecycle === 'retained' || s.lifecycle === 'released') && s.usedBytes >= bytes) set.add('traffic_exhausted')
    const t = activeTask(s.id)
    if (t && t.t.kind !== 'fulfill') set.add('changing')
    if (s.lifecycle === 'active' && (s.exit?.check === 'failed' || (s.nodeId && !nodeOnline(s.nodeId)))) set.add('dataplane')
    return BLOCKER_ORDER.filter((b) => set.has(b))
  }

  const subView = (s: SubRow): AdminSubscription => {
    const { bytes, seconds } = sums(s.id)
    const u = userOf(s)
    const until = s.direct ? paidUntil(s.userId) : s.expiresAt
    const remainingSeconds = until && s.lifecycle === 'active' ? Math.max(0, Math.floor((Date.parse(until) - now) / 1000)) : 0
    return {
      id: s.id,
      name: s.name,
      kind: s.direct ? 'direct' : 'isp',
      user: { id: u.id, email: u.email },
      nodeId: s.nodeId,
      plan: s.direct ? null : s.plan,
      direct: s.direct ? { ...s.direct, paidUntil: until } : null,
      lifecycle: s.lifecycle,
      blockers: blockersOf(s),
      adminPaused: s.adminPaused,
      pausedReason: s.pausedReason,
      time: {
        periodDays: Math.round(seconds / 86_400),
        startedAt: s.startedAt,
        expiresAt: until,
        remainingSeconds,
        retentionEndsAt: s.retentionEndsAt,
      },
      traffic: { totalBytes: bytes, usedBytes: s.usedBytes, remainingBytes: Math.max(0, bytes - s.usedBytes) },
      exit: s.exit,
      access: s.access,
      activeConns: nodeOnline(s.nodeId) && s.usageReportedAt ? s.activeConns : null,
      usageReportedAt: s.usageReportedAt,
      desiredVersion: s.desiredVersion,
      appliedVersion: s.appliedVersion,
      activeTaskId: activeTask(s.id)?.t.id ?? null,
      openExitRequestId: db.exitRequests.find((r) => r.subscriptionId === s.id && (r.status === 'pending' || r.status === 'processing'))?.id ?? null,
      openOrderIds: db.orders
        .filter((o) => o.subscriptionId === s.id && (o.payment === 'awaiting' || o.fulfillment === 'processing' || o.fulfillment === 'failed'))
        .map((o) => o.id),
      createdAt: s.createdAt,
    }
  }

  const subDetail = (s: SubRow): AdminSubscriptionDetail => ({
    ...subView(s),
    quotas: {
      accessChangesLeft: s.accessChangesLeft,
      accessChangesPerDay: 5,
      freeExitChangesLeft: s.freeExitChangesLeft,
      freeExitWindowEndsAt: s.freeExitWindowEndsAt,
    },
    activeTask: activeTask(s.id)?.t ?? null,
    rulesVersion: s.rulesVersion,
    lifecycleReason: s.lifecycleReason,
    firstReadyAt: s.firstReadyAt,
    releasedAt: s.releasedAt,
    deletedAt: s.deletedAt,
    createdFromOrderId: s.createdFromOrderId,
  })

  const userView = (u: UserRow): AdminUser => ({
    id: u.id,
    email: u.email,
    verified: u.verified,
    status: u.status,
    passwordSet: u.passwordSet,
    bannedAt: u.bannedAt,
    bannedReason: u.bannedReason,
    createdAt: u.createdAt,
    createdByAdmin: u.createdByAdmin,
    subscriptionCount: db.subs.filter((s) => s.userId === u.id && s.lifecycle !== 'deleted').length,
    openOrderCount: db.orders.filter((o) => o.user.id === u.id && o.payment === 'awaiting').length,
    lastLoginAt: u.lastLoginAt,
  })

  const planStock = (cc: string, city: string) => {
    const rs = db.upstreams.filter((r) => r.u.countryCode === cc && r.u.city === city)
    const available = rs.filter((r) => r.u.status === 'available').length
    return {
      available,
      reserved: rs.filter((r) => r.u.status === 'reserved').length,
      bound: rs.filter((r) => r.u.status === 'bound').length,
      level: available === 0 ? ('out' as const) : available <= 3 ? ('low' as const) : ('plenty' as const),
    }
  }
  const planView = (p: Db['plans'][number], all: boolean): AdminPlan => {
    const current = p.versions[0]!
    return {
      id: p.id,
      code: p.code,
      status: p.status,
      sortOrder: p.sortOrder,
      current,
      stock: planStock(current.countryCode, current.city),
      versions: all ? p.versions : [current],
    }
  }
  const packView = (p: Db['packs'][number], all: boolean): AdminTrafficPack => ({
    id: p.id,
    code: p.code,
    status: p.status,
    sortOrder: p.sortOrder,
    current: p.versions[0]!,
    versions: all ? p.versions : [p.versions[0]!],
  })

  const orderView = (o: AdminOrder): AdminOrder => {
    const at = (x: string | null) => x
    const steps: AdminOrder['steps'] = [
      { label: '下单', state: 'done', at: o.createdAt },
      {
        label: '核实收款',
        state: o.payment === 'awaiting' ? 'current' : o.payment === 'cancelled' ? 'failed' : 'done',
        at: at(o.confirmedAt),
      },
      {
        label: o.kind === 'new' ? '开通' : '入账',
        state:
          o.fulfillment === 'delivered'
            ? 'done'
            : o.fulfillment === 'failed'
              ? 'failed'
              : o.fulfillment === 'processing'
                ? 'current'
                : 'todo',
        at: at(o.deliveredAt),
      },
    ]
    return {
      ...o,
      steps,
      subscriptionName: o.subscriptionId ? (db.subs.find((s) => s.id === o.subscriptionId)?.name ?? null) : null,
      ledgerEntries: db.ledger.filter((l) => l.orderId === o.id),
      refunds: db.refunds.filter((r) => r.orderId === o.id),
    }
  }

  const nodeView = (n: Node): Node => {
    const subsOn = db.subs.filter((s) => s.nodeId === n.id && (s.lifecycle === 'active' || s.lifecycle === 'retained'))
    return {
      ...n,
      subscriptions: subsOn.length,
      activeConns: n.online ? subsOn.reduce((a, s) => a + (s.activeConns ?? 0), 0) : null,
    }
  }

  // ---------------------------------------------------------------- 事件、账本
  const event = (s: SubRow, kind: AdminSubscriptionEvent['kind'], detail: string, extra: Partial<AdminSubscriptionEvent> = {}) => {
    s.events.push({
      id: nextId('evt'),
      at: iso(now),
      kind,
      detail,
      customerVisible: !['task_failed', 'fulfill_failed', 'adjusted'].includes(kind),
      orderId: null,
      taskId: null,
      actor: session.adminId ? actor() : SYSTEM,
      ...extra,
    })
  }

  const addLedger = (s: SubRow, entry: Omit<LedgerEntry, 'id' | 'subscriptionId' | 'unit' | 'createdAt'>): LedgerEntry => {
    const e: LedgerEntry = {
      id: nextId('led'),
      subscriptionId: s.id,
      unit: entry.entitlementKind === 'time' ? 'seconds' : 'bytes',
      createdAt: iso(now),
      ...entry,
    }
    db.ledger.push(e)
    if (e.entitlementKind === 'time' && e.amount !== 0) {
      if (s.lifecycle === 'active' && s.expiresAt) {
        s.expiresAt = iso(Date.parse(s.expiresAt) + e.amount * 1000)
      } else if (s.lifecycle === 'retained' && e.amount > 0) {
        // 保留期内续时长：恢复原出口与接入，从重新就绪时起算（R-18）。
        s.lifecycle = 'active'
        s.expiresAt = iso(now + e.amount * 1000)
        s.retentionEndsAt = null
      }
    }
    return e
  }

  // ---------------------------------------------------------------- 任务与探测
  const newTask = (s: SubRow, t: Partial<Task> & Pick<Task, 'kind'>, apply?: () => void, failure?: TaskRow['failure']): TaskRow => {
    const row: TaskRow = {
      t: {
        id: nextId('tsk'),
        subscriptionId: s.id,
        state: 'queued',
        requestedBy: actor(),
        orderId: null,
        exitRequestId: null,
        newAccessVersion: null,
        newExitVersion: null,
        targetVersion: s.desiredVersion + 1,
        attempts: 1,
        failureCode: null,
        failureReason: null,
        createdAt: iso(now),
        startedAt: null,
        finishedAt: null,
        ...t,
      },
      polls: steps,
      ...(apply ? { apply } : {}),
      ...(failure ? { failure } : {}),
    }
    s.desiredVersion += 1
    db.tasks.push(row)
    return row
  }

  const advanceTask = (row: TaskRow) => {
    if (row.t.state !== 'queued' && row.t.state !== 'running') return
    row.polls -= 1
    if (row.polls > 0) {
      row.t.state = 'running'
      row.t.startedAt ??= iso(now)
      return
    }
    row.t.startedAt ??= iso(now)
    row.t.finishedAt = iso(now)
    const s = db.subs.find((x) => x.id === row.t.subscriptionId)
    const failure = row.failure
    if (failure) {
      row.t.state = 'failed'
      row.t.failureCode = failure.code
      row.t.failureReason = failure.reason
      if (s) {
        s.desiredVersion = s.appliedVersion
        event(s, 'task_failed', `任务失败：${failure.reason}`, { taskId: row.t.id, actor: SYSTEM })
      }
      row.apply?.()
      return
    }
    row.t.state = 'succeeded'
    row.apply?.()
    if (s) s.appliedVersion = Math.max(s.appliedVersion, row.t.targetVersion ?? s.appliedVersion)
  }

  const observe = (r: UpstreamRow) => r.probe
  const newProbe = (p: Partial<Probe> & Pick<Probe, 'kind' | 'nodeId'>): ProbeRow => {
    const row: ProbeRow = {
      p: {
        id: nextId('prb'),
        subscriptionId: null,
        resourceId: null,
        status: 'queued',
        result: null,
        observedIp: null,
        observedCountryCode: null,
        observedCity: null,
        latencyMs: null,
        error: null,
        comparison: null,
        createdAt: iso(now),
        completedAt: null,
        deadlineAt: iso(now + 60_000),
        ...p,
      },
      polls: steps,
    }
    db.probes.push(row)
    return row
  }

  const advanceProbe = (row: ProbeRow) => {
    const p = row.p
    if (p.status === 'done' || p.status === 'expired') return
    row.polls -= 1
    if (row.polls > 0) {
      p.status = 'dispatched'
      return
    }
    p.status = 'done'
    p.completedAt = iso(now)
    const res = p.resourceId ? db.upstreams.find((u) => u.u.id === p.resourceId) : undefined
    const sub = p.subscriptionId ? db.subs.find((s) => s.id === p.subscriptionId) : undefined
    if (!res) {
      p.result = 'failed'
      p.error = '资源不存在'
      return
    }
    const seen = observe(res)
    if ('error' in seen) {
      p.result = 'failed'
      p.error = seen.error
    } else {
      p.result = seen.latencyMs > 300 ? 'slow' : 'ok'
      p.observedIp = seen.ip
      p.observedCountryCode = seen.countryCode
      p.observedCity = seen.city
      p.latencyMs = seen.latencyMs
    }
    if (p.kind === 'upstream_verify') {
      res.u.lastVerify = {
        at: iso(now),
        ok: p.result !== 'failed',
        exitIp: p.observedIp,
        exitCountryCode: p.observedCountryCode,
        error: p.error,
      }
      if (p.result !== 'failed' && res.u.status === 'pending_verify') res.u.status = 'available'
      res.u.updatedAt = iso(now)
    }
    if (p.kind === 'exit_check' && sub?.exit) {
      sub.exit.check = p.result
      sub.exit.ip = p.observedIp
      sub.exit.latencyMs = p.latencyMs
      sub.exit.checkedAt = iso(now)
      sub.exit.checkError = p.error
      const b = sub.exitBindings.find((x) => x.state === 'active')
      if (b && p.result)
        b.check = { result: p.result, ip: p.observedIp, countryCode: p.observedCountryCode, city: p.observedCity, asn: 'AS64500 示意 ISP', latencyMs: p.latencyMs, error: p.error, checkedAt: iso(now) }
    }
    if (p.kind === 'candidate_compare' && sub && p.result !== 'failed') {
      const ip = p.observedIp
      const current = sub.exit?.verifiedIp ?? null
      const differs = ip !== current
      const unique = !db.subs.some((o) => o.id !== sub.id && o.lifecycle !== 'deleted' && o.exit?.verifiedIp === ip)
      const countryOk = p.observedCountryCode === sub.plan.countryCode
      p.comparison = { currentIp: current, differsFromCurrent: differs, uniqueOnPlatform: unique, countryMatches: countryOk, passed: differs && unique && countryOk }
    }
  }

  // ---------------------------------------------------------------- 履约
  /** 收款确认或代建后新建的订阅（待履约，没有接入与出口）。 */
  const DIRECT_DEFAULT_MBPS = 10
  const DIRECT_MAX_CONNS = 64
  const liveDirect = (userId: string) => db.subs.find((s) => s.userId === userId && s.direct && s.lifecycle !== 'deleted')
  const directOf = (userId: string) => {
    const s = liveDirect(userId)
    return {
      subscriptionId: s?.id ?? null,
      lifecycle: s && s.lifecycle !== 'deleted' ? s.lifecycle : null,
      bandwidthMbps: s?.direct?.bandwidthMbps ?? null,
      defaultBandwidthMbps: DIRECT_DEFAULT_MBPS,
      maxConnections: s?.direct?.maxConnections ?? DIRECT_MAX_CONNS,
    }
  }

  const blankSub = (o: { name: string; userId: string; planId: string; orderId: string; nodeId?: string | null }): SubRow => {
    const plan = db.plans.find((p) => p.id === o.planId)!
    const pv = plan.versions[0]!
    return {
      id: nextId('sub'),
      name: o.name,
      userId: o.userId,
      nodeId: o.nodeId ?? null,
      plan: { planId: plan.id, planVersionId: pv.id, planVersion: pv.version, name: pv.name, countryCode: pv.countryCode, country: pv.country, city: pv.city, concurrency: pv.concurrency, bandwidthMbps: pv.bandwidthMbps },
      lifecycle: 'pending',
      lifecycleReason: null,
      adminPaused: false,
      pausedReason: null,
      startedAt: null,
      expiresAt: null,
      retentionEndsAt: null,
      usedBytes: 0,
      exit: null,
      access: null,
      accessPassword: null,
      activeConns: null,
      usageReportedAt: null,
      desiredVersion: 0,
      appliedVersion: 0,
      createdAt: iso(now),
      firstReadyAt: null,
      releasedAt: null,
      deletedAt: null,
      createdFromOrderId: o.orderId,
      rulesVersion: db.rules[0]!.version,
      accessChangesLeft: 5,
      freeExitChangesLeft: 1,
      freeExitWindowEndsAt: null,
      usage: 'none',
      usageScale: 0.3 * GB,
      accessVersions: [],
      exitBindings: [],
      events: [],
    }
  }

  const fulfill =(s: SubRow, order: AdminOrder | undefined, resourceId?: string) => {
    const pick = resourceId
      ? db.upstreams.find((u) => u.u.id === resourceId && (u.u.status === 'available' || (u.u.status === 'reserved' && u.u.reservedOrderId === order?.id)))
      : db.upstreams.find((u) => (u.u.status === 'available' || (u.u.status === 'reserved' && order && u.u.reservedOrderId === order.id)) && u.u.countryCode === s.plan.countryCode && u.u.city === s.plan.city)
    const failure: TaskRow['failure'] = !pick
      ? { code: 'no_stock', reason: `无可售库存（${s.plan.country} · ${s.plan.city}），已回收端口` }
      : 'error' in pick.probe
        ? { code: 'exit_unverified', reason: `出口验证失败：${pick.probe.error}` }
        : undefined
    if (pick) {
      pick.u.status = 'reserved'
      pick.u.reservedOrderId = order?.id ?? null
    }
    const row = newTask(
      s,
      { kind: 'fulfill', orderId: order?.id ?? null, newAccessVersion: 1, newExitVersion: (s.exitBindings.at(-1)?.version ?? 0) + 1 },
      () => {
        if (!pick || 'error' in pick.probe) {
          if (pick) Object.assign(pick.u, { status: 'pending_verify', reservedOrderId: null })
          s.lifecycle = 'fulfill_failed'
          s.lifecycleReason = row.t.failureReason
          if (order) {
            order.fulfillment = 'failed'
            order.fulfillmentError = row.t.failureReason
          }
          return
        }
        const seen = pick.probe
        const { seconds } = sums(s.id)
        pick.u.status = 'bound'
        pick.u.boundSubscriptionId = s.id
        pick.u.reservedOrderId = null
        const node = db.nodes.find((n) => n.id === pick.u.nodeId)!
        const port = 26000 + db.seq * 3
        const accId = nextId('acc')
        const version = (s.accessVersions[0]?.version ?? 0) + 1
        s.nodeId = node.id
        s.access = { id: accId, version, state: 'active', host: node.publicHost, username: `u${s.id.slice(4, 9)}`, ports: { http: port, https: port + 1, socks5: port + 2 }, issuedAt: iso(now), activatedAt: iso(now) }
        s.accessPassword = secretOf(accId)
        s.accessVersions.unshift({ id: accId, version, changeKind: version === 1 ? 'initial' : 'refulfill', state: 'active', username: s.access.username, ports: s.access.ports, issuedAt: iso(now), activatedAt: iso(now), revokedAt: null, revokeReason: null, taskId: row.t.id })
        const exb = nextId('exb')
        const ev = (s.exitBindings.at(-1)?.version ?? 0) + 1
        s.exit = {
          bindingId: exb,
          version: ev,
          resourceId: pick.u.id,
          provider: pick.u.provider,
          upstreamProtocol: pick.u.protocol,
          upstreamAddr: `${pick.u.host}:${pick.u.port}`,
          verifiedIp: seen.ip,
          ip: seen.ip,
          countryCode: seen.countryCode,
          country: s.plan.country,
          city: seen.city,
          check: 'ok',
          latencyMs: seen.latencyMs,
          checkedAt: iso(now),
          checkError: null,
          boundAt: iso(now),
        }
        s.exitBindings.push({ id: exb, version: ev, state: 'active', resourceId: pick.u.id, verifiedIp: seen.ip, verifiedCountryCode: seen.countryCode, check: { result: 'ok', ip: seen.ip, countryCode: seen.countryCode, city: seen.city, asn: 'AS64500 示意 ISP', latencyMs: seen.latencyMs, error: null, checkedAt: iso(now) }, boundAt: iso(now), unboundAt: null, unboundReason: null, taskId: row.t.id })
        s.lifecycle = 'active'
        s.lifecycleReason = null
        s.startedAt ??= iso(now)
        s.firstReadyAt ??= iso(now)
        s.expiresAt = iso(now + seconds * 1000)
        s.usageReportedAt = iso(now)
        s.activeConns = 0
        s.usage = 'idle'
        if (order) {
          order.fulfillment = 'delivered'
          order.deliveredAt = iso(now)
          order.fulfillmentError = null
        }
        event(s, 'ready', `开通就绪，开始计时；出口 ${seen.ip}，接入 v${version}`, { orderId: order?.id ?? null, taskId: row.t.id, actor: { kind: 'node', id: node.id } })
      },
      failure,
    )
    if (order) order.fulfillment = 'processing'
    s.lifecycle = 'pending'
    return row
  }

  // ---------------------------------------------------------------- 用量趋势
  const usageSeries = (s: SubRow, fromIso: string, toIso: string, bucket?: '5m' | '1h' | '1d'): UsageSeries => {
    const from = Date.parse(fromIso)
    const to = Date.parse(toIso)
    if (!(from < to)) fail('invalid', '时间范围不正确')
    if (to - from > 90 * DAY) fail('invalid', '最长查询 90 天（D-07）')
    const b = bucket ?? (to - from <= DAY ? '5m' : to - from <= 7 * DAY ? '1h' : '1d')
    const sec = b === '5m' ? 300 : b === '1h' ? 3600 : 86_400
    const ms = sec * 1000
    const started = s.firstReadyAt ? Date.parse(s.firstReadyAt) : Infinity
    const last = s.usageReportedAt ? Date.parse(s.usageReportedAt) : null
    const exhaustedAt = s.events.find((e) => e.kind === 'exhausted')?.at
    const r = rng(`usage:${s.id}:${b}`)
    const points: UsagePoint[] = []
    for (let t = Math.floor(from / ms) * ms; t < to && t <= now; t += ms) {
      const end = Math.min(t + ms, now)
      let bytes: number | null = null
      let samples = 0
      const noSample = s.usage === 'none' || end <= started || last === null || t > last
      const gap = s.usage === 'gappy' && end > now - 9 * HOUR && t < now - 6.5 * HOUR
      if (!noSample && !gap) {
        const hour = ((t / HOUR + 8) % 24 + 24) % 24
        const diurnal = 0.25 + 0.75 * Math.max(0, Math.sin(((hour - 7) / 24) * Math.PI * 2))
        const idleNow = (s.usage === 'idle' && t > now - 6 * HOUR) || (exhaustedAt && t >= Date.parse(exhaustedAt)) || s.adminPaused || s.lifecycle === 'retained'
        bytes = idleNow ? 0 : Math.round(s.usageScale * (sec / 3600) * diurnal * (0.7 + 0.6 * r()))
        if (!idleNow && hour >= 3 && hour < 5 && r() < 0.35) bytes = 0
        samples = Math.max(1, Math.round(Math.min(end, last ?? end) - t) / 5000)
      }
      points.push({
        at: iso(t),
        bytes,
        maxConns: bytes === null ? null : bytes === 0 ? 0 : Math.max(1, Math.round((s.activeConns ?? 3) * (0.5 + r()))),
        samples: bytes === null ? 0 : Math.round(samples),
        sampledAt: bytes === null ? null : iso(Math.min(end, last ?? end)),
      })
    }
    const any = points.some((p) => p.bytes !== null)
    return {
      subscriptionId: s.id,
      source: 'agent_report',
      unit: 'bytes',
      bucketSeconds: sec,
      from: iso(from),
      to: iso(to),
      lastSampleAt: any ? s.usageReportedAt : null,
      freshness: !any ? 'no_data' : last !== null && now - last <= 15_000 ? 'fresh' : 'stale',
      points,
    }
  }

  // ---------------------------------------------------------------- 接口实现
  const api: AdminApi = {
    // ---------- 登录
    adminLogin: ({ body }) =>
      wait(() => {
        const a = db.admins.find((x) => x.admin.email.toLowerCase() === body.email.trim().toLowerCase() && x.password === body.password)
        if (!a) fail('invalid', '邮箱或密码不正确')
        session.stage = 'mfa'
        session.adminId = a!.admin.id
        if (a!.admin.totpEnrolled) return { stage: 'totp_required' as const }
        const label = encodeURIComponent(`ISP 代理管理后台:${a!.admin.email}`)
        return {
          stage: 'enrollment_required' as const,
          enrollment: {
            secret: a!.totpSecret,
            otpauthUri: `otpauth://totp/${label}?secret=${a!.totpSecret}&issuer=${encodeURIComponent('ISP 代理')}&algorithm=SHA1&digits=6&period=30`,
          },
        }
      }),
    adminVerifyMfa: ({ body }) =>
      wait(() => {
        if (session.stage !== 'mfa' || !session.adminId) fail('unauthorized', '请先输入邮箱和密码')
        const a = db.admins.find((x) => x.admin.id === session.adminId)!
        let recoveryCodes: string[] | undefined
        if (body.recoveryCode) {
          if (!a.admin.totpEnrolled) fail('invalid', '首次登录需要先绑定验证器')
          const i = a.recoveryCodes.indexOf(body.recoveryCode.trim().toLowerCase())
          if (i < 0) fail('invalid', '恢复码不正确或已使用')
          a.recoveryCodes.splice(i, 1)
        } else if (body.code === MOCK_ADMIN_LOGIN.totp) {
          if (!a.admin.totpEnrolled) {
            a.admin.totpEnrolled = true
            a.recoveryCodes = recoveryCodesFor(`${a.admin.id}:${db.seq++}`)
            recoveryCodes = [...a.recoveryCodes]
          }
        } else {
          fail('invalid', '验证码不正确或已过期')
        }
        session.stage = 'ok'
        a.admin.lastLoginAt = iso(now)
        audit('admin.login', 'admin', a.admin.id, { mfa: body.recoveryCode ? 'recovery_code' : 'totp' })
        return recoveryCodes ? { admin: a.admin, recoveryCodes } : { admin: a.admin }
      }),
    adminLogout: () =>
      wait(() => {
        session.stage = 'none'
        session.adminId = null
      }),
    adminMe: () => wait(() => me().admin),
    adminRegenerateRecoveryCodes: ({ body }) =>
      wait(() => {
        const a = me()
        if (body.code !== MOCK_ADMIN_LOGIN.totp) fail('invalid', '验证码不正确或已过期')
        a.recoveryCodes = recoveryCodesFor(`${a.admin.id}:${db.seq++}`)
        audit('admin.recovery_codes', 'admin', a.admin.id)
        return { codes: [...a.recoveryCodes] }
      }),

    // ---------- 概览
    adminGetOverview: () =>
      wait((): Overview => {
        me()
        const views = db.subs.filter((s) => s.lifecycle !== 'deleted').map(subView)
        const has = (b: Blocker) => views.filter((v) => v.blockers.includes(b)).length
        const fresh = db.subs.filter((s) => s.usageReportedAt && now - Date.parse(s.usageReportedAt) <= 15_000 && nodeOnline(s.nodeId))
        const lastReport = db.nodes.map((n) => n.lastReportAt).filter((x): x is string => !!x).sort().at(-1) ?? null
        return {
          generatedAt: iso(now),
          dataAt: lastReport,
          subscriptions: {
            total: views.length,
            usable: views.filter((v) => v.blockers.length === 0).length,
            exhausted: has('traffic_exhausted'),
            expired: views.filter((v) => v.lifecycle === 'retained').length,
            paused: views.filter((v) => v.adminPaused).length,
            pending: views.filter((v) => v.lifecycle === 'pending').length,
            fulfillFailed: views.filter((v) => v.lifecycle === 'fulfill_failed').length,
            changing: has('changing'),
            dataplane: has('dataplane'),
            released: views.filter((v) => v.lifecycle === 'released').length,
          },
          traffic: {
            currentConns: fresh.length ? fresh.reduce((a, s) => a + (s.activeConns ?? 0), 0) : null,
            rateBps: fresh.length ? Math.round(fresh.reduce((a, s) => a + (s.usageScale / 3600) * (s.activeConns ? 1 : 0), 0)) : null,
            todayBytes: Math.round(db.subs.reduce((a, s) => a + (s.usage === 'none' ? 0 : s.usageScale * 10), 0)),
            totalBytes: db.subs.reduce((a, s) => a + s.usedBytes, 0),
          },
          orders: {
            awaitingPayment: db.orders.filter((o) => o.payment === 'awaiting').length,
            awaitingFulfillment: db.orders.filter((o) => o.payment === 'confirmed' && o.fulfillment === 'processing').length,
            fulfillFailed: db.orders.filter((o) => o.fulfillment === 'failed').length,
            refundReview: db.orders.filter((o) => o.payment === 'refund_review').length,
          },
          exitRequests: {
            pending: db.exitRequests.filter((r) => r.status === 'pending').length,
            awaitingFeeDecision: db.exitRequests.filter((r) => r.feeStatus === 'awaiting_decision' && r.status === 'pending').length,
            processing: db.exitRequests.filter((r) => r.status === 'processing').length,
          },
          alerts: {
            open: db.alerts.filter((a) => a.status === 'open').length,
            critical: db.alerts.filter((a) => a.status === 'open' && a.severity === 'critical').length,
          },
          nodes: { online: db.nodes.filter((n) => n.online).length, offline: db.nodes.filter((n) => !n.online).length },
        }
      }),

    // ---------- 订阅
    adminListSubscriptions: (input) =>
      wait(() => {
        me()
        const q = input?.query ?? {}
        const lifecycles = asArray(q.lifecycle)
        const needle = q.q?.trim().toLowerCase()
        const items = db.subs
          .filter((s) => (lifecycles?.length ? lifecycles.includes(s.lifecycle) : s.lifecycle !== 'deleted'))
          .filter((s) => !q.userId || s.userId === q.userId)
          .filter((s) => !q.nodeId || s.nodeId === q.nodeId)
          .filter((s) => !q.kind || (q.kind === 'direct') === !!s.direct)
          .map(subView)
          .filter((v) => !q.blocker || v.blockers.includes(q.blocker))
          .filter((v) => q.usable === undefined || (q.usable ? v.blockers.length === 0 : v.blockers.length > 0))
          .filter((v) => !q.expiresBefore || (v.time.expiresAt !== null && v.time.expiresAt < q.expiresBefore))
          .filter(
            (v) =>
              !needle ||
              [v.id, v.name, v.user.email, v.access?.username ?? '', v.exit?.ip ?? '', v.exit?.verifiedIp ?? ''].some((x) => x.toLowerCase().includes(needle)),
          )
          .sort((a, b) => b.createdAt.localeCompare(a.createdAt))
        return paginate(items, q)
      }),
    adminCreateSubscription: ({ body, idempotencyKey }) =>
      wait(() => {
        writer('subscription.create')
        return once('createSub', idempotencyKey, body, () => {
          const u = findUser(body.userId)
          if (u.status === 'banned') fail('precondition', '用户已封禁，不能代建订阅')
          const plan = db.plans.find((p) => p.id === body.planId) ?? fail('not_found', '没有找到这个套餐')
          const pv = plan.versions[0]!
          if (body.planVersionId && body.planVersionId !== pv.id) fail('conflict', '套餐已发布新版本，请刷新后重试')
          if (body.months < 1) fail('invalid', '月数至少为 1')
          const amount = body.payment.method === 'admin_gift' ? 0 : pv.price.amount * body.months
          if (body.payment.amount.amount !== amount) fail('invalid', `金额必须等于订单金额 ¥${(amount / 100).toFixed(2)}`)
          const oid = nextId('ord')
          const s = blankSub({ name: body.name ?? pv.name, userId: u.id, planId: plan.id, orderId: oid, nodeId: body.nodeId ?? null })
          const subId = s.id
          const order: AdminOrder = {
            id: oid,
            no: `ORD-20261107-${String(100 + db.seq).padStart(4, '0')}`,
            kind: 'new',
            createdAt: iso(now),
            holdUntil: null,
            amount: { amount, currency: 'CNY' },
            payment: 'confirmed',
            fulfillment: 'processing',
            userClaimedPaidAt: null,
            payRef: payRefOf(oid),
            subscriptionId: subId,
            subscriptionName: body.name ?? pv.name,
            lines: [{ label: pv.name, detail: `${body.months} 个月 · 含 ${pv.includedBytes / GB} GB`, amount: { amount, currency: 'CNY' } }],
            steps: [],
            rulesSnapshot: db.rules[0]!.snapshotLines,
            user: { id: u.id, email: u.email },
            paymentMethod: body.payment.method,
            paymentRecord: { id: nextId('pay'), amount: { amount, currency: 'CNY' }, method: body.payment.method, externalRef: body.payment.externalRef, lateArrival: false, note: body.payment.note ?? '', confirmedBy: adminRef(), confirmedAt: iso(now) },
            ledgerEntries: [],
            refunds: [],
            rulesVersion: db.rules[0]!.version,
            planVersionId: pv.id,
            fulfillmentError: null,
            cancelReason: null,
            confirmedAt: iso(now),
            deliveredAt: null,
            createdByAdmin: adminRef(),
          }
          db.orders.unshift(order)
          db.subs.unshift(s)
          addLedger(s, { entitlementKind: 'time', amount: body.months * MONTH_S, sourceType: 'order', sourceId: oid, orderId: oid, refundId: null, transferId: null, reason: '新购（代建）', admin: null })
          addLedger(s, { entitlementKind: 'traffic', amount: pv.includedBytes, sourceType: 'order', sourceId: oid, orderId: oid, refundId: null, transferId: null, reason: '新购（代建）', admin: null })
          const t = fulfill(s, order, body.resourceId)
          audit('subscription.create', 'subscription', s.id, { userId: u.id, planId: plan.id, months: body.months, payment: body.payment.method })
          return { order: orderView(order), subscription: subDetail(s), task: t.t }
        })
      }),
    adminGetSubscription: ({ subscriptionId }) =>
      wait(() => {
        me()
        return subDetail(findSub(subscriptionId))
      }),
    adminRenameSubscription: ({ subscriptionId, body }) =>
      wait(() => {
        writer('subscription.rename', 'subscription', subscriptionId)
        const s = findSub(subscriptionId)
        const name = body.name.trim()
        if (name.length < 1 || name.length > 24) fail('invalid', '名称 1～24 个字')
        event(s, 'renamed', `名称 ${s.name} → ${name}`)
        s.name = name
        return subDetail(s)
      }),
    adminPauseSubscription: ({ subscriptionId, body }) =>
      wait(() => {
        writer('subscription.pause', 'subscription', subscriptionId)
        const s = findSub(subscriptionId)
        if (s.lifecycle === 'deleted' || s.lifecycle === 'released') fail('precondition', '已释放或已删除的订阅不能暂停')
        if (!body.reason.trim()) fail('invalid', '请填写暂停原因')
        if (!s.adminPaused) {
          s.adminPaused = true
          s.pausedReason = body.reason.trim()
          s.activeConns = 0
          s.desiredVersion += 1
          s.appliedVersion = s.desiredVersion
          event(s, 'paused', `管理员暂停：${s.pausedReason}`)
          audit('subscription.pause', 'subscription', s.id, { reason: s.pausedReason })
        }
        return subDetail(s)
      }),
    adminResumeSubscription: ({ subscriptionId }) =>
      wait(() => {
        writer('subscription.resume', 'subscription', subscriptionId)
        const s = findSub(subscriptionId)
        if (s.adminPaused) {
          s.adminPaused = false
          s.pausedReason = null
          s.desiredVersion += 1
          s.appliedVersion = s.desiredVersion
          event(s, 'resumed', '管理员解除暂停（其他阻断仍然生效）')
          audit('subscription.resume', 'subscription', s.id)
        }
        return subDetail(s)
      }),
    adminDeleteSubscription: ({ subscriptionId, body }) =>
      wait(() => {
        writer('subscription.delete', 'subscription', subscriptionId)
        const s = findSub(subscriptionId)
        if (s.lifecycle !== 'active' && s.lifecycle !== 'released') fail('precondition', '只允许删除“有效”或“已释放”的订阅')
        if (activeTask(s.id)) fail('conflict', '有进行中的任务，完成后再删除')
        const remaining = Math.max(0, sums(s.id).bytes - s.usedBytes)
        if (remaining > 0 && !body.acknowledgeRemainingTraffic) fail('precondition', `订阅还有 ${(remaining / GB).toFixed(2)} GB 剩余流量，需先确认人工处置`)
        if (s.exit) {
          const r = db.upstreams.find((u) => u.u.id === s.exit!.resourceId)
          if (r) {
            r.u.status = 'cooldown'
            r.u.boundSubscriptionId = null
            r.u.cooldownUntil = iso(now + 7 * DAY)
            r.u.statusReason = '订阅删除释放'
          }
          const b = s.exitBindings.find((x) => x.state === 'active')
          if (b) Object.assign(b, { state: 'unbound', unboundAt: iso(now), unboundReason: '订阅删除' })
        }
        for (const v of s.accessVersions) if (v.state === 'active') Object.assign(v, { state: 'revoked', revokedAt: iso(now), revokeReason: '订阅删除' })
        s.access = null
        s.accessPassword = null
        s.exit = null
        s.lifecycle = 'deleted'
        s.deletedAt = iso(now)
        s.lifecycleReason = body.reason
        event(s, 'deleted', `删除订阅：${body.reason}`)
        audit('subscription.delete', 'subscription', s.id, { reason: body.reason, remainingBytes: remaining })
        return subDetail(s)
      }),
    adminRetryFulfillment: ({ subscriptionId, body }) =>
      wait(() => {
        writer('fulfill.retry', 'subscription', subscriptionId)
        const s = findSub(subscriptionId)
        if (s.lifecycle !== 'fulfill_failed' && s.lifecycle !== 'pending') fail('precondition', '只有待履约或履约异常的订阅可以重试开通')
        if (activeTask(s.id)) fail('conflict', '已有进行中的开通任务')
        const order = db.orders.find((o) => o.id === s.createdFromOrderId)
        const t = fulfill(s, order, body?.resourceId)
        audit('fulfill.retry', 'subscription', s.id, { resourceId: body?.resourceId ?? null })
        return t.t
      }),
    adminChangeAccess: ({ subscriptionId, body, idempotencyKey }) =>
      wait(() => {
        writer('access.change', 'subscription', subscriptionId)
        return once(`access:${subscriptionId}`, idempotencyKey, body, () => {
          const s = findSub(subscriptionId)
          if (!s.access) fail('precondition', '这个订阅当前没有接入（未开通或已释放）')
          if (s.lifecycle !== 'active' && s.lifecycle !== 'retained') fail('precondition', '只有“有效”或“保留期”的订阅可以换接入')
          if (!body.reason.trim()) fail('invalid', '请填写原因')
          if (activeTask(s.id)) fail('conflict', '已有进行中的任务，完成后再试')
          const version = s.access!.version + 1
          const row = newTask(s, { kind: body.mode === 'password' ? 'password_reset' : 'access_change', newAccessVersion: version }, () => {
            const old = s.accessVersions.find((v) => v.state === 'active')
            if (old) Object.assign(old, { state: 'revoked', revokedAt: iso(now), revokeReason: body.mode === 'password' ? '重置密码' : '更换接入' })
            const ports = body.mode === 'password' ? s.access!.ports : { http: 27000 + db.seq * 3, https: 27001 + db.seq * 3, socks5: 27002 + db.seq * 3 }
            const id = nextId('acc')
            const v: AccessVersion = { id, version, changeKind: body.mode === 'password' ? 'password_reset' : 'ports_and_password', state: 'active', username: s.access!.username, ports, issuedAt: iso(now), activatedAt: iso(now), revokedAt: null, revokeReason: null, taskId: row.t.id }
            s.accessVersions.unshift(v)
            s.access = { ...s.access!, id, version, ports, issuedAt: iso(now), activatedAt: iso(now) }
            s.accessPassword = secretOf(id)
            event(s, body.mode === 'password' ? 'password_reset' : 'access_changed', `接入 v${version - 1} → v${version}：${body.mode === 'password' ? '密码已重置' : '端口与密码已更换'}；出口未变（管理员代办：${body.reason}）`, { taskId: row.t.id })
          })
          audit('access.change', 'subscription', s.id, { mode: body.mode, reason: body.reason })
          return row.t
        })
      }),
    adminRevealAccess: ({ subscriptionId, body }) =>
      wait(() => {
        me()
        const s = findSub(subscriptionId)
        if (!body.reason.trim()) fail('invalid', '查看明文连接需要填写原因')
        if (!s.access || !s.accessPassword) fail('not_found', '当前没有生效的接入')
        audit('access.reveal', 'subscription', s.id, { reason: body.reason, plaintextLogged: false })
        return { version: s.access!.version, host: s.access!.host, username: s.access!.username, password: s.accessPassword!, ports: s.access!.ports }
      }),
    adminListAccessVersions: ({ subscriptionId }) =>
      wait(() => {
        me()
        return [...findSub(subscriptionId).accessVersions].sort((a, b) => b.version - a.version)
      }),
    adminListExitBindings: ({ subscriptionId }) =>
      wait(() => {
        me()
        return [...findSub(subscriptionId).exitBindings].sort((a, b) => b.version - a.version)
      }),
    adminCheckExit: ({ subscriptionId }) =>
      wait(() => {
        writer('exit.check', 'subscription', subscriptionId)
        const s = findSub(subscriptionId)
        if (!s.exit) fail('precondition', '这个订阅当前没有出口')
        return newProbe({ kind: 'exit_check', nodeId: s.nodeId!, subscriptionId: s.id, resourceId: s.exit!.resourceId }).p
      }),
    adminCompareExitCandidate: ({ subscriptionId, body }) =>
      wait(() => {
        writer('exit.compare', 'subscription', subscriptionId)
        const s = findSub(subscriptionId)
        if (s.lifecycle !== 'active' && s.lifecycle !== 'retained') fail('precondition', '只有“有效”或“保留期”的订阅可以换出口')
        const r = findUpstream(body.resourceId)
        if (r.u.boundSubscriptionId === s.id) fail('conflict', '这就是该订阅当前的上游资源')
        if (!(r.u.status === 'available' || r.u.status === 'pending_verify') || r.u.boundSubscriptionId) fail('conflict', `候选资源当前状态为“${r.u.status}”，不能作为候选（须为可售或待验证且未被占用）`)
        if (activeTask(s.id)) fail('conflict', '已有进行中的任务，完成后再试')
        return newProbe({ kind: 'candidate_compare', nodeId: r.u.nodeId, subscriptionId: s.id, resourceId: r.u.id }).p
      }),
    adminSubmitExitChange: ({ subscriptionId, body, idempotencyKey }) =>
      wait(() => {
        writer('exit.change', 'subscription', subscriptionId)
        return once(`exit:${subscriptionId}`, idempotencyKey, body, () => {
          const s = findSub(subscriptionId)
          if (!body.reason.trim()) fail('invalid', '请填写原因')
          const latest = [...db.probes].reverse().find((p) => p.p.kind === 'candidate_compare' && p.p.subscriptionId === s.id && p.p.resourceId === body.resourceId && p.p.status === 'done')
          if (!latest || latest.p.id !== body.compareProbeId) fail('precondition', '需要先对这个候选完成最近一次比较')
          const cmp = latest!.p.comparison
          if (!cmp?.passed) {
            if (cmp && !cmp.differsFromCurrent) fail('precondition', '上游已更新，出口未变：候选出口与当前出口相同，不能提交')
            fail('precondition', '候选没有通过比较（须与当前出口不同、平台内独占、国家符合）')
          }
          if (activeTask(s.id)) fail('conflict', '已有进行中的任务，完成后再试')
          const r = findUpstream(body.resourceId)
          if (r.u.status !== 'available' && r.u.status !== 'pending_verify') fail('conflict', '候选资源已被占用')
          const req = body.exitRequestId ? findReq(body.exitRequestId) : null
          if (req && req.status !== 'processing') fail('precondition', '关联的申请需要先受理')
          r.u.status = 'reserved'
          const newVersion = (s.exitBindings.reduce((m, b) => Math.max(m, b.version), 0) || 0) + 1
          const oldIp = s.exit?.verifiedIp ?? null
          const row = newTask(
            s,
            { kind: body.alsoChangeAccess ? 'combined' : 'exit_change', newExitVersion: newVersion, newAccessVersion: body.alsoChangeAccess ? (s.access?.version ?? 0) + 1 : null, exitRequestId: req?.id ?? null },
            () => {
              if (row.t.state === 'failed') {
                r.u.status = 'quarantine'
                r.u.reservedTaskId = null
                r.u.statusReason = '切换后复核失败，已隔离'
                return
              }
              const seen = r.probe as { ip: string; countryCode: string; city: string; latencyMs: number }
              const old = s.exitBindings.find((b) => b.state === 'active')
              if (old) Object.assign(old, { state: 'unbound', unboundAt: iso(now), unboundReason: body.reason })
              const oldRes = s.exit ? db.upstreams.find((u) => u.u.id === s.exit!.resourceId) : undefined
              if (oldRes) Object.assign(oldRes.u, { status: 'cooldown', boundSubscriptionId: null, cooldownUntil: iso(now + 7 * DAY), statusReason: '换出口释放', updatedAt: iso(now) })
              Object.assign(r.u, { status: 'bound', boundSubscriptionId: s.id, reservedTaskId: null, updatedAt: iso(now) })
              const exb = nextId('exb')
              s.exitBindings.push({ id: exb, version: newVersion, state: 'active', resourceId: r.u.id, verifiedIp: seen.ip, verifiedCountryCode: seen.countryCode, check: { result: 'ok', ip: seen.ip, countryCode: seen.countryCode, city: seen.city, asn: 'AS64500 示意 ISP', latencyMs: seen.latencyMs, error: null, checkedAt: iso(now) }, boundAt: iso(now), unboundAt: null, unboundReason: null, taskId: row.t.id })
              s.exit = { bindingId: exb, version: newVersion, resourceId: r.u.id, provider: r.u.provider, upstreamProtocol: r.u.protocol, upstreamAddr: `${r.u.host}:${r.u.port}`, verifiedIp: seen.ip, ip: seen.ip, countryCode: seen.countryCode, country: s.plan.country, city: seen.city, check: 'ok', latencyMs: seen.latencyMs, checkedAt: iso(now), checkError: null, boundAt: iso(now) }
              if (body.alsoChangeAccess && s.access) {
                const v = s.access.version + 1
                const oldAcc = s.accessVersions.find((x) => x.state === 'active')
                if (oldAcc) Object.assign(oldAcc, { state: 'revoked', revokedAt: iso(now), revokeReason: '组合更换' })
                const id = nextId('acc')
                const ports = { http: 28000 + db.seq * 3, https: 28001 + db.seq * 3, socks5: 28002 + db.seq * 3 }
                s.accessVersions.unshift({ id, version: v, changeKind: 'ports_and_password', state: 'active', username: s.access.username, ports, issuedAt: iso(now), activatedAt: iso(now), revokedAt: null, revokeReason: null, taskId: row.t.id })
                s.access = { ...s.access, id, version: v, ports, issuedAt: iso(now), activatedAt: iso(now) }
                s.accessPassword = secretOf(id)
              }
              event(s, 'exit_changed', `出口 ${oldIp ?? '—'} → ${seen.ip}${req ? `（申请 ${req.no}）` : ''}；${body.alsoChangeAccess ? '接入同时更换' : '接入未变'}`, { taskId: row.t.id })
              if (req) Object.assign(req, { status: 'succeeded', newIp: seen.ip, finishedAt: iso(now), updatedAt: iso(now), taskId: row.t.id })
            },
            r.failsOnApply ? { code: 'exit_unverified', reason: '经客户入口复核出口失败，已回到原绑定，服务状态不变' } : undefined,
          )
          r.u.reservedTaskId = row.t.id
          if (req) req.taskId = row.t.id
          audit('exit.change', 'subscription', s.id, { resourceId: r.u.id, alsoChangeAccess: body.alsoChangeAccess, reason: body.reason, exitRequest: req?.no ?? null })
          return row.t
        })
      }),
    adminListSubscriptionTasks: ({ subscriptionId }) =>
      wait(() => {
        me()
        findSub(subscriptionId)
        return db.tasks.filter((t) => t.t.subscriptionId === subscriptionId).map((t) => t.t).sort((a, b) => b.createdAt.localeCompare(a.createdAt))
      }),
    adminListLedger: ({ subscriptionId }) =>
      wait(() => {
        me()
        const s = findSub(subscriptionId)
        const { bytes, seconds } = sums(s.id)
        return {
          entries: db.ledger.filter((l) => l.subscriptionId === s.id).sort((a, b) => b.createdAt.localeCompare(a.createdAt)),
          entitledBytesTotal: bytes,
          usedBytesTotal: s.usedBytes,
          entitledSecondsTotal: seconds,
        }
      }),
    adminCreateLedgerEntry: ({ subscriptionId, body, idempotencyKey }) =>
      wait(() => {
        writer('ledger.create', 'subscription', subscriptionId)
        return once(`ledger:${subscriptionId}`, idempotencyKey, body, () => {
          const s = findSub(subscriptionId)
          if (!body.reason.trim()) fail('invalid', '请填写原因')
          if (!Number.isInteger(body.amount) || body.amount === 0) fail('invalid', '数量必须是非零整数')
          if ((body.sourceType === 'gift' || body.sourceType === 'compensation') && body.amount < 0) fail('invalid', '赠送与补偿只能为正数；扣回请用“调整”')
          const cur = sums(s.id)
          const total = (body.entitlementKind === 'traffic' ? cur.bytes : cur.seconds) + body.amount
          if (total < 0) fail('precondition', '扣回后合计会小于 0，不能执行')
          const e = addLedger(s, { entitlementKind: body.entitlementKind, amount: body.amount, sourceType: body.sourceType, sourceId: nextId('adj'), orderId: null, refundId: null, transferId: null, reason: body.reason.trim(), admin: adminRef() })
          event(s, 'adjusted', `${{ adjust: '调整', gift: '赠送', compensation: '补偿' }[body.sourceType]}${body.entitlementKind === 'traffic' ? '流量' : '时长'} ${body.amount > 0 ? '+' : ''}${body.entitlementKind === 'traffic' ? `${(body.amount / GB).toFixed(2)} GB` : `${(body.amount / 3600).toFixed(1)} 小时`}：${body.reason}`)
          audit('ledger.create', 'subscription', s.id, { sourceType: body.sourceType, kind: body.entitlementKind, amount: body.amount, reason: body.reason })
          return e
        })
      }),
    adminListSubscriptionEvents: ({ subscriptionId }) =>
      wait(() => {
        me()
        return [...findSub(subscriptionId).events].sort((a, b) => b.at.localeCompare(a.at))
      }),
    adminGetSubscriptionUsage: ({ subscriptionId, query }) =>
      wait(() => {
        me()
        return usageSeries(findSub(subscriptionId), query.from, query.to, query.bucket)
      }),
    adminTransferBalance: ({ body, idempotencyKey }) =>
      wait(() => {
        writer('ledger.transfer', 'subscription', body.fromSubscriptionId)
        return once('transfer', idempotencyKey, body, () => {
          if (body.fromSubscriptionId === body.toSubscriptionId) fail('invalid', '来源与目标不能是同一个订阅')
          if (!body.reason.trim()) fail('invalid', '请填写原因')
          const from = findSub(body.fromSubscriptionId)
          const to = findSub(body.toSubscriptionId)
          if (from.userId !== to.userId) fail('precondition', '两个订阅必须属于同一用户')
          if (to.lifecycle === 'deleted') fail('precondition', '目标订阅已删除')
          const remaining = Math.max(0, sums(from.id).bytes - from.usedBytes)
          if (remaining <= 0) fail('precondition', '来源订阅没有可划转的剩余流量')
          const tid = nextId('trf')
          addLedger(from, { entitlementKind: 'traffic', amount: -remaining, sourceType: 'transfer_out', sourceId: tid, orderId: null, refundId: null, transferId: tid, reason: `划转到 ${to.name}：${body.reason}`, admin: adminRef() })
          addLedger(to, { entitlementKind: 'traffic', amount: remaining, sourceType: 'transfer_in', sourceId: tid, orderId: null, refundId: null, transferId: tid, reason: `来自 ${from.name}：${body.reason}`, admin: adminRef() })
          event(from, 'transferred', `剩余流量 ${(remaining / GB).toFixed(2)} GB 全额划转到 ${to.name}，余额归零`)
          event(to, 'transferred', `收到 ${from.name} 划转的 ${(remaining / GB).toFixed(2)} GB`)
          const t = { id: tid, userId: from.userId, fromSubscriptionId: from.id, toSubscriptionId: to.id, bytes: remaining, reason: body.reason, admin: adminRef(), createdAt: iso(now) }
          db.transfers.push(t)
          audit('ledger.transfer', 'subscription', from.id, { to: to.id, bytes: remaining, reason: body.reason })
          return t
        })
      }),
    adminGetTask: ({ taskId }) =>
      wait(() => {
        me()
        const row = db.tasks.find((t) => t.t.id === taskId) ?? fail('not_found', '没有找到这个任务')
        advanceTask(row)
        return row.t
      }),
    adminGetProbe: ({ probeId }) =>
      wait(() => {
        me()
        const row = db.probes.find((p) => p.p.id === probeId) ?? fail('not_found', '没有找到这个探测')
        advanceProbe(row)
        return row.p
      }),

    // ---------- 上游
    adminListUpstreams: (input) =>
      wait(() => {
        me()
        const q = input?.query ?? {}
        const statuses = asArray(q.status)
        const needle = q.q?.trim().toLowerCase()
        const items = db.upstreams
          .map((r) => r.u)
          .filter((u) => !statuses?.length || statuses.includes(u.status))
          .filter((u) => !q.countryCode || u.countryCode === q.countryCode)
          .filter((u) => !q.city || u.city === q.city)
          .filter((u) => !q.nodeId || u.nodeId === q.nodeId)
          .filter((u) => !needle || [u.id, u.host, u.provider, u.lastVerify?.exitIp ?? '', u.username].some((x) => x.toLowerCase().includes(needle)))
          .sort((a, b) => b.createdAt.localeCompare(a.createdAt))
        return paginate(items, q)
      }),
    adminCreateUpstream: ({ body }) =>
      wait(() => {
        writer('upstream.create')
        let protocol = body.protocol
        let host = body.host
        let port = body.port
        let username = body.username
        let password = body.password
        if (body.url) {
          let parsed: URL
          try {
            parsed = new URL(body.url.trim())
          } catch {
            return fail('invalid', '无法解析上游地址，请检查格式（例如 socks5://user:pass@host:1080）')
          }
          const scheme = parsed.protocol.replace(':', '')
          if (scheme !== 'http' && scheme !== 'https' && scheme !== 'socks5') fail('invalid', '只支持 http、https、socks5 上游')
          protocol ??= scheme as 'http' | 'https' | 'socks5'
          host ??= parsed.hostname
          port ??= Number(parsed.port) || (scheme === 'socks5' ? 1080 : scheme === 'https' ? 443 : 80)
          username ??= decodeURIComponent(parsed.username)
          password ??= decodeURIComponent(parsed.password)
        }
        if (!protocol || !host || !port) fail('invalid', '协议、主机、端口必填')
        if (port! < 1 || port! > 65535) fail('invalid', '端口超出范围')
        if (!/^[a-z]{2}$/.test(body.countryCode) || !body.city.trim()) fail('invalid', '国家代码与城市必填')
        if (!db.nodes.some((n) => n.id === body.nodeId)) fail('invalid', '节点不存在')
        if (db.upstreams.some((r) => r.u.protocol === protocol && r.u.host === host && r.u.port === port && r.u.username === (username ?? ''))) fail('conflict', '同一协议、主机、端口与用户名的资源已存在')
        const id = nextId('res')
        const r = rng(id)
        const row: UpstreamRow = {
          u: {
            id,
            nodeId: body.nodeId,
            protocol: protocol!,
            host: host!,
            port: port!,
            username: username ?? '',
            hasPassword: Boolean(password),
            countryCode: body.countryCode,
            country: body.country,
            city: body.city,
            provider: body.provider ?? '',
            note: body.note ?? '',
            status: 'pending_verify',
            statusReason: null,
            reservedOrderId: null,
            reservedTaskId: null,
            reservedUntil: null,
            boundSubscriptionId: null,
            cooldownUntil: null,
            lastVerify: null,
            rowVersion: 1,
            createdAt: iso(now),
            updatedAt: iso(now),
          },
          probe: password ? { ip: `203.0.113.${Math.floor(150 + r() * 80)}`, countryCode: body.countryCode, city: body.city, latencyMs: Math.round(120 + r() * 150) } : { error: '上游要求认证，但没有提供密码' },
        }
        db.upstreams.unshift(row)
        audit('upstream.create', 'upstream', id, { protocol, host, port, hasPassword: Boolean(password) })
        return row.u
      }),
    adminGetUpstream: ({ upstreamId }) =>
      wait(() => {
        me()
        return findUpstream(upstreamId).u
      }),
    adminUpdateUpstream: ({ upstreamId, body }) =>
      wait(() => {
        writer('upstream.update', 'upstream', upstreamId)
        const r = findUpstream(upstreamId)
        if (body.rowVersion !== r.u.rowVersion) fail('conflict', '资源已被其他人修改，请刷新后再改')
        const connChanged =
          (body.protocol !== undefined && body.protocol !== r.u.protocol) ||
          (body.host !== undefined && body.host !== r.u.host) ||
          (body.port !== undefined && body.port !== r.u.port) ||
          (body.username !== undefined && body.username !== r.u.username) ||
          (body.password !== undefined && body.password !== '')
        if (connChanged && r.u.status === 'bound') fail('precondition', '已占用的资源不能改连接信息，请先为订阅换出口')
        const { rowVersion: _rv, password, ...rest } = body
        void _rv
        Object.assign(r.u, Object.fromEntries(Object.entries(rest).filter(([, v]) => v !== undefined)))
        if (password) r.u.hasPassword = true
        if (connChanged) {
          r.u.status = 'pending_verify'
          r.u.lastVerify = null
        }
        r.u.rowVersion += 1
        r.u.updatedAt = iso(now)
        audit('upstream.update', 'upstream', r.u.id, { connectionChanged: connChanged })
        return r.u
      }),
    adminSetUpstreamStatus: ({ upstreamId, body }) =>
      wait(() => {
        writer('upstream.status', 'upstream', upstreamId)
        const r = findUpstream(upstreamId)
        if (!body.reason.trim()) fail('invalid', '请填写原因')
        if (r.u.status === 'bound') fail('precondition', '已占用的资源要先为订阅换出口')
        if (r.u.status === 'reserved') fail('precondition', '已预留的资源由订单或任务维护，不能人工改状态')
        if (body.status === 'available') {
          if (!['pending_verify', 'cooldown', 'quarantine'].includes(r.u.status)) fail('precondition', '只有待验证、冷却或隔离的资源可以设为可售')
          if (!r.u.lastVerify?.ok) fail('precondition', '最近一次验证没有通过，先重新验证')
        }
        r.u.status = body.status
        r.u.statusReason = body.reason.trim()
        if (body.status === 'available') r.u.cooldownUntil = null
        r.u.updatedAt = iso(now)
        r.u.rowVersion += 1
        audit('upstream.status', 'upstream', r.u.id, { status: body.status, reason: body.reason })
        return r.u
      }),
    adminVerifyUpstream: ({ upstreamId }) =>
      wait(() => {
        writer('upstream.verify', 'upstream', upstreamId)
        const r = findUpstream(upstreamId)
        if (r.u.status === 'retired') fail('precondition', '已停用的资源不能验证')
        return newProbe({ kind: 'upstream_verify', nodeId: r.u.nodeId, resourceId: r.u.id }).p
      }),

    // ---------- 套餐、流量包、规则
    adminListPlans: () =>
      wait(() => {
        me()
        return [...db.plans].sort((a, b) => a.sortOrder - b.sortOrder).map((p) => planView(p, false))
      }),
    adminCreatePlan: ({ body }) =>
      wait(() => {
        writer('plan.create')
        if (!/^[a-z0-9][a-z0-9-]{1,31}$/.test(body.code)) fail('invalid', '代码只能用小写字母、数字和连字符')
        if (db.plans.some((p) => p.code === body.code)) fail('conflict', '套餐代码已存在')
        const id = nextId('pln')
        const p = { id, code: body.code, status: 'draft' as const, sortOrder: body.sortOrder ?? db.plans.length * 10 + 10, versions: [{ ...body.version, id: nextId('plv'), version: 1, createdAt: iso(now), createdBy: adminRef() }] }
        db.plans.push(p)
        audit('plan.create', 'plan', id, { code: body.code })
        return planView(p, true)
      }),
    adminGetPlan: ({ planId }) =>
      wait(() => {
        me()
        return planView(db.plans.find((p) => p.id === planId) ?? fail('not_found', '没有找到这个套餐'), true)
      }),
    adminCreatePlanVersion: ({ planId, body }) =>
      wait(() => {
        writer('plan.version', 'plan', planId)
        const p = db.plans.find((x) => x.id === planId) ?? fail('not_found', '没有找到这个套餐')
        if (body.price.amount < 0 || body.includedBytes < 0 || body.concurrency < 1 || body.bandwidthMbps < 1 || body.periodDays < 1) fail('invalid', '数值不合法')
        const v = { ...body, id: nextId('plv'), version: p.versions[0]!.version + 1, createdAt: iso(now), createdBy: adminRef() }
        p.versions.unshift(v)
        audit('plan.version', 'plan', p.id, { version: v.version, price: body.price.amount })
        return planView(p, true)
      }),
    adminSetPlanStatus: ({ planId, body }) =>
      wait(() => {
        writer('plan.status', 'plan', planId)
        const p = db.plans.find((x) => x.id === planId) ?? fail('not_found', '没有找到这个套餐')
        p.status = body.status
        audit('plan.status', 'plan', p.id, { status: body.status })
        return planView(p, true)
      }),
    adminListTrafficPacks: () =>
      wait(() => {
        me()
        return [...db.packs].sort((a, b) => a.sortOrder - b.sortOrder).map((p) => packView(p, false))
      }),
    adminCreateTrafficPack: ({ body }) =>
      wait(() => {
        writer('pack.create')
        if (!/^[a-z0-9][a-z0-9-]{1,31}$/.test(body.code)) fail('invalid', '代码只能用小写字母、数字和连字符')
        if (db.packs.some((p) => p.code === body.code)) fail('conflict', '流量包代码已存在')
        if (body.version.bytes < 1) fail('invalid', '流量必须大于 0')
        const p = { id: nextId('tpk'), code: body.code, status: 'draft' as const, sortOrder: body.sortOrder ?? db.packs.length * 10 + 10, versions: [{ ...body.version, id: nextId('tpv'), version: 1, createdAt: iso(now), createdBy: adminRef() }] }
        db.packs.push(p)
        audit('pack.create', 'traffic_pack', p.id, { code: body.code })
        return packView(p, true)
      }),
    adminGetTrafficPack: ({ trafficPackId }) =>
      wait(() => {
        me()
        return packView(db.packs.find((p) => p.id === trafficPackId) ?? fail('not_found', '没有找到这个流量包'), true)
      }),
    adminCreateTrafficPackVersion: ({ trafficPackId, body }) =>
      wait(() => {
        writer('pack.version', 'traffic_pack', trafficPackId)
        const p = db.packs.find((x) => x.id === trafficPackId) ?? fail('not_found', '没有找到这个流量包')
        if (body.bytes < 1 || body.price.amount < 0) fail('invalid', '数值不合法')
        p.versions.unshift({ ...body, id: nextId('tpv'), version: p.versions[0]!.version + 1, createdAt: iso(now), createdBy: adminRef() })
        audit('pack.version', 'traffic_pack', p.id, { version: p.versions[0]!.version })
        return packView(p, true)
      }),
    adminSetTrafficPackStatus: ({ trafficPackId, body }) =>
      wait(() => {
        writer('pack.status', 'traffic_pack', trafficPackId)
        const p = db.packs.find((x) => x.id === trafficPackId) ?? fail('not_found', '没有找到这个流量包')
        p.status = body.status
        audit('pack.status', 'traffic_pack', p.id, { status: body.status })
        return packView(p, true)
      }),
    adminListRules: () =>
      wait(() => {
        me()
        return [...db.rules].sort((a, b) => b.effectiveFrom.localeCompare(a.effectiveFrom))
      }),
    adminCreateRules: ({ body }) =>
      wait(() => {
        writer('rules.create')
        if (!/^R-[0-9]{4}-[0-9]{2}-[0-9]{2}(\.[0-9]+)?$/.test(body.version)) fail('invalid', '版本号格式为 R-YYYY-MM-DD 或 R-YYYY-MM-DD.N')
        if (db.rules.some((r) => r.version === body.version)) fail('conflict', '规则版本号已存在')
        if (Date.parse(body.effectiveFrom) < now - 60_000) fail('invalid', '生效时间不能早于当前时间')
        if (!body.monthOptions.length) fail('invalid', '至少一个月数选项')
        const r = { ...body, createdAt: iso(now), createdBy: adminRef() }
        db.rules.unshift(r)
        audit('rules.create', 'rules', body.version, { paymentConfigured: Boolean(body.payment.account.trim()) })
        return r
      }),

    // ---------- 用户
    adminListUsers: (input) =>
      wait(() => {
        me()
        const q = input?.query ?? {}
        const needle = q.q?.trim().toLowerCase()
        const items = db.users
          .filter((u) => !q.status || u.status === q.status)
          .filter((u) => !needle || u.email.toLowerCase().includes(needle) || u.id.includes(needle))
          .sort((a, b) => b.createdAt.localeCompare(a.createdAt))
          .map(userView)
        return paginate(items, q)
      }),
    adminCreateUser: ({ body }) =>
      wait(() => {
        writer('user.create')
        const email = body.email.trim().toLowerCase()
        if (!/^[^@\s]+@[^@\s]+\.[^@\s]+$/.test(email)) fail('invalid', '邮箱格式不正确')
        if (db.users.some((u) => u.email === email)) fail('conflict', '这个邮箱已注册')
        const u: UserRow = { id: nextId('usr'), email, verified: false, status: 'active', passwordSet: false, bannedAt: null, bannedReason: null, createdAt: iso(now), createdByAdmin: adminRef(), lastLoginAt: null, activeSessions: 0, resetSentAt: now }
        db.users.unshift(u)
        audit('user.create', 'user', u.id, { email })
        return userView(u)
      }),
    adminGetUser: ({ userId }) =>
      wait((): AdminUserDetail => {
        me()
        const u = findUser(userId)
        return {
          ...userView(u),
          subscriptions: db.subs.filter((s) => s.userId === u.id).map(subView),
          recentOrders: db.orders.filter((o) => o.user.id === u.id).sort((a, b) => b.createdAt.localeCompare(a.createdAt)).slice(0, 10).map(orderView),
          activeSessions: u.activeSessions,
          direct: directOf(u.id),
        }
      }),
    adminGrantDirect: ({ userId, body }) =>
      wait((): AdminSubscriptionDetail => {
        writer('direct.grant', 'user', userId)
        const u = findUser(userId)
        const bw = body.bandwidthMbps ?? DIRECT_DEFAULT_MBPS
        if (!Number.isInteger(bw) || bw < 1 || bw > 10000) fail('invalid', '带宽上限需为 1～10000 Mbps')
        const cur = liveDirect(u.id)
        if (cur) {
          if (cur.direct!.bandwidthMbps !== bw) {
            event(cur, 'adjusted', `带宽上限 ${cur.direct!.bandwidthMbps} → ${bw} Mbps`)
            audit('direct.update', 'subscription', cur.id, { userId: u.id, fromBandwidthMbps: cur.direct!.bandwidthMbps, bandwidthMbps: bw })
            cur.direct = { ...cur.direct!, bandwidthMbps: bw }
            cur.plan = { ...cur.plan, bandwidthMbps: bw }
            cur.desiredVersion += 1
            cur.appliedVersion = cur.desiredVersion
          }
          return subDetail(cur)
        }
        const node = db.nodes.find((n) => n.online) ?? fail('precondition', '没有可用的中转节点，暂不能赠送直出代理')
        const placeholder = db.plans[0]!
        const s = blankSub({ name: '服务器直出', userId: u.id, planId: placeholder.id, orderId: '', nodeId: node.id })
        s.createdFromOrderId = null
        s.direct = { bandwidthMbps: bw, maxConnections: DIRECT_MAX_CONNS }
        s.plan = { planId: '', planVersionId: '', planVersion: 0, name: '服务器直出', countryCode: '', country: '', city: '', concurrency: DIRECT_MAX_CONNS, bandwidthMbps: bw }
        s.freeExitChangesLeft = 0
        const port = 27000 + db.seq * 3
        const accId = nextId('acc')
        s.access = { id: accId, version: 1, state: 'active', host: node.publicHost, username: `u${s.id.slice(4, 9)}`, ports: { http: port, https: port + 1, socks5: port + 2 }, issuedAt: iso(now), activatedAt: iso(now) }
        s.accessPassword = secretOf(accId)
        s.accessVersions.unshift({ id: accId, version: 1, changeKind: 'initial', state: 'active', username: s.access.username, ports: s.access.ports, issuedAt: iso(now), activatedAt: iso(now), revokedAt: null, revokeReason: null, taskId: null })
        s.lifecycle = 'active'
        s.firstReadyAt = iso(now)
        s.usageReportedAt = iso(now)
        s.activeConns = 0
        s.desiredVersion = 1
        s.appliedVersion = 1
        db.subs.unshift(s)
        event(s, 'adjusted', `管理员赠送服务器直出代理：带宽上限 ${bw} Mbps、并发 ${DIRECT_MAX_CONNS}，不限流量，跟随付费 IP 有效`)
        event(s, 'ready', '服务器直出代理已开通，接入 v1；不限流量，跟随你的付费 IP 有效', { actor: { kind: 'node', id: node.id } })
        audit('direct.grant', 'subscription', s.id, { userId: u.id, bandwidthMbps: bw, reason: body.reason ?? '' })
        return subDetail(s)
      }),
    adminRevokeDirect: ({ userId, query }) =>
      wait((): void => {
        writer('direct.revoke', 'user', userId)
        const u = findUser(userId)
        const s = liveDirect(u.id)
        if (!s) return
        for (const v of s.accessVersions) if (v.state !== 'revoked') Object.assign(v, { state: 'revoked', revokedAt: iso(now), revokeReason: '收回直出代理' })
        s.access = null
        s.accessPassword = null
        s.lifecycle = 'deleted'
        s.deletedAt = iso(now)
        s.lifecycleReason = query?.reason || '管理员收回'
        event(s, 'deleted', `管理员收回服务器直出代理：${s.lifecycleReason}`)
        audit('direct.revoke', 'subscription', s.id, { userId: u.id, reason: s.lifecycleReason })
      }),
    adminBanUser: ({ userId, body }) =>
      wait(() => {
        writer('user.ban', 'user', userId)
        const u = findUser(userId)
        if (!body.reason.trim()) fail('invalid', '请填写封禁原因')
        if (u.status !== 'banned') {
          const revoked = u.activeSessions
          u.status = 'banned'
          u.bannedAt = iso(now)
          u.bannedReason = body.reason.trim()
          u.activeSessions = 0
          for (const s of db.subs.filter((x) => x.userId === u.id && x.lifecycle !== 'deleted')) {
            s.activeConns = 0
            event(s, 'paused', `账号封禁，停止转发：${u.bannedReason}`)
          }
          audit('user.ban', 'user', u.id, { reason: u.bannedReason, revokedSessions: revoked })
        }
        return userView(u)
      }),
    adminUnbanUser: ({ userId }) =>
      wait(() => {
        writer('user.unban', 'user', userId)
        const u = findUser(userId)
        if (u.status === 'banned') {
          u.status = 'active'
          u.bannedAt = null
          u.bannedReason = null
          audit('user.unban', 'user', u.id)
        }
        return userView(u)
      }),
    adminSendPasswordReset: ({ userId }) =>
      wait(() => {
        writer('user.password_reset_email', 'user', userId)
        const u = findUser(userId)
        if (u.resetSentAt && now - u.resetSentAt < 60_000) throw new ApiError('rate_limited', '同一邮箱 60 秒内只能发送一次', { retryAfter: 60 })
        u.resetSentAt = now
        audit('user.password_reset_email', 'user', u.id)
      }),

    // ---------- 订单与退款
    adminListOrders: (input) =>
      wait(() => {
        me()
        const q = input?.query ?? {}
        const pay = asArray(q.payment)
        const ful = asArray(q.fulfillment)
        const needle = q.q?.trim().toLowerCase()
        const items = db.orders
          .filter((o) => !pay?.length || pay.includes(o.payment))
          .filter((o) => !ful?.length || ful.includes(o.fulfillment))
          .filter((o) => !q.kind || o.kind === q.kind)
          .filter((o) => !q.userId || o.user.id === q.userId)
          .filter((o) => !q.subscriptionId || o.subscriptionId === q.subscriptionId)
          .filter((o) => !q.createdFrom || o.createdAt >= q.createdFrom)
          .filter((o) => !q.createdTo || o.createdAt <= q.createdTo)
          .filter((o) => !needle || [o.no, o.payRef, o.user.email, o.id].some((x) => x.toLowerCase().includes(needle)))
          .sort((a, b) => b.createdAt.localeCompare(a.createdAt))
          .map(orderView)
        return paginate(items, q)
      }),
    adminCreateOrder: ({ body, idempotencyKey }) =>
      wait(() => {
        writer('order.create')
        return once('createOrder', idempotencyKey, body, () => {
          const u = findUser(body.userId)
          const input = body.input
          if (input.kind === 'new') fail('invalid', '新购请用“代建订阅”')
          const s = findSub((input as { subscriptionId: string }).subscriptionId)
          if (s.userId !== u.id) fail('not_found', '没有找到这个订阅')
          const lines: AdminOrder['lines'] = []
          if (input.kind === 'renew_time') {
            const plan = db.plans.find((p) => p.id === s.plan.planId)!.versions[0]!
            lines.push({ label: '续时长', detail: `${input.months} 个月`, amount: { amount: plan.price.amount * input.months, currency: 'CNY' } })
          }
          const packId = input.kind === 'add_traffic' ? input.packId : input.kind === 'renew_time' ? input.packId : undefined
          if (packId) {
            const pack = db.packs.find((p) => p.id === packId || p.code === packId) ?? fail('not_found', '没有找到这个流量包')
            lines.push({ label: '流量包', detail: `${pack.versions[0]!.bytes / GB} GB`, amount: pack.versions[0]!.price })
          }
          const id = nextId('ord')
          const o: AdminOrder = {
            id,
            no: `ORD-20261107-${String(200 + db.seq).padStart(4, '0')}`,
            kind: input.kind,
            createdAt: iso(now),
            holdUntil: iso(now + 24 * HOUR),
            amount: { amount: lines.reduce((a, l) => a + l.amount.amount, 0), currency: 'CNY' },
            payment: 'awaiting',
            fulfillment: 'not_started',
            userClaimedPaidAt: null,
            payRef: payRefOf(id),
            subscriptionId: s.id,
            subscriptionName: s.name,
            lines,
            steps: [],
            rulesSnapshot: db.rules[0]!.snapshotLines,
            user: { id: u.id, email: u.email },
            paymentMethod: 'manual_transfer',
            paymentRecord: null,
            ledgerEntries: [],
            refunds: [],
            rulesVersion: db.rules[0]!.version,
            planVersionId: null,
            fulfillmentError: null,
            cancelReason: null,
            confirmedAt: null,
            deliveredAt: null,
            createdByAdmin: adminRef(),
          }
          db.orders.unshift(o)
          audit('order.create', 'order', id, { kind: input.kind, subscriptionId: s.id })
          return orderView(o)
        })
      }),
    adminGetOrder: ({ orderId }) =>
      wait(() => {
        me()
        return orderView(findOrder(orderId))
      }),
    adminConfirmPayment: ({ orderId, body }) =>
      wait(() => {
        writer('order.confirm_payment', 'order', orderId)
        const o = findOrder(orderId)
        if (o.paymentRecord) {
          const r = o.paymentRecord
          if (r.amount.amount === body.amount.amount && r.externalRef === body.externalRef.trim() && r.method === body.method) return orderView(o)
          fail('conflict', `订单已于 ${r.confirmedAt} 确认收款（流水 ${r.externalRef}），本次填写的内容与已有记录不同`)
        }
        if (!body.externalRef.trim()) fail('invalid', '请填写外部收款参考（流水号）')
        if (body.amount.amount !== o.amount.amount) fail('invalid', `金额必须等于订单金额 ¥${(o.amount.amount / 100).toFixed(2)}`)
        if (o.payment === 'cancelled' && !body.lateArrival) fail('precondition', '订单已取消；确认晚到款需要勾选“晚到款”，确认后进入人工处置')
        if (o.payment !== 'awaiting' && o.payment !== 'cancelled') fail('precondition', '当前收款状态不能确认')
        o.payment = 'confirmed'
        o.confirmedAt = iso(now)
        o.paymentRecord = { id: nextId('pay'), amount: body.amount, method: body.method, externalRef: body.externalRef.trim(), lateArrival: Boolean(body.lateArrival), note: body.note ?? '', confirmedBy: adminRef(), confirmedAt: iso(now) }
        audit('order.confirm_payment', 'order', o.id, { no: o.no, amount: body.amount.amount, externalRef: body.externalRef, lateArrival: Boolean(body.lateArrival) })
        if (body.lateArrival) {
          o.fulfillment = 'failed'
          o.fulfillmentError = '晚到款：订单已取消后到账，请选择重新履约或退款'
          return orderView(o)
        }
        if (o.kind === 'new') {
          const plan = db.plans.find((p) => p.versions.some((v) => v.id === o.planVersionId)) ?? db.plans[0]!
          const pv = plan.versions.find((v) => v.id === o.planVersionId) ?? plan.versions[0]!
          const months = Math.max(1, Math.round(o.amount.amount / pv.price.amount))
          const s = blankSub({ name: pv.name, userId: o.user.id, planId: plan.id, orderId: o.id })
          db.subs.unshift(s)
          o.subscriptionId = s.id
          addLedger(s, { entitlementKind: 'time', amount: months * MONTH_S, sourceType: 'order', sourceId: o.id, orderId: o.id, refundId: null, transferId: null, reason: '新购', admin: null })
          addLedger(s, { entitlementKind: 'traffic', amount: pv.includedBytes, sourceType: 'order', sourceId: o.id, orderId: o.id, refundId: null, transferId: null, reason: '新购', admin: null })
          fulfill(s, o)
        } else {
          const s = findSub(o.subscriptionId!)
          for (const line of o.lines) {
            if (line.label === '续时长') {
              const months = Number(line.detail.match(/\d+/)?.[0] ?? 1)
              addLedger(s, { entitlementKind: 'time', amount: months * MONTH_S, sourceType: 'order', sourceId: o.id, orderId: o.id, refundId: null, transferId: null, reason: '续时长', admin: null })
              event(s, 'time_added', `续时长 +${months * 30} 天`, { orderId: o.id })
            } else {
              const gb = Number(line.detail.match(/\d+/)?.[0] ?? 0)
              addLedger(s, { entitlementKind: 'traffic', amount: gb * GB, sourceType: 'order', sourceId: o.id, orderId: o.id, refundId: null, transferId: null, reason: `流量包 ${gb} GB`, admin: null })
              event(s, 'traffic_added', `买流量 +${gb} GB`, { orderId: o.id })
            }
          }
          o.fulfillment = 'delivered'
          o.deliveredAt = iso(now)
        }
        return orderView(o)
      }),
    adminCancelOrder: ({ orderId, body }) =>
      wait(() => {
        writer('order.cancel', 'order', orderId)
        const o = findOrder(orderId)
        if (!body.reason.trim()) fail('invalid', '请填写取消原因')
        if (o.payment !== 'awaiting') fail('precondition', '只能取消待核款的订单')
        o.payment = 'cancelled'
        o.cancelReason = 'admin'
        o.holdUntil = null
        for (const r of db.upstreams) if (r.u.reservedOrderId === o.id) Object.assign(r.u, { status: 'available', reservedOrderId: null, reservedUntil: null })
        audit('order.cancel', 'order', o.id, { reason: body.reason })
        return orderView(o)
      }),
    adminRetryOrderFulfillment: ({ orderId, body }) =>
      wait(() => {
        writer('order.fulfillment_retry', 'order', orderId)
        const o = findOrder(orderId)
        if (o.fulfillment !== 'failed') fail('precondition', '只有履约失败的订单可以重试')
        if (o.payment !== 'confirmed') fail('precondition', '订单尚未确认收款')
        const s = o.subscriptionId ? db.subs.find((x) => x.id === o.subscriptionId) : undefined
        if (s) {
          if (activeTask(s.id)) fail('conflict', '已有进行中的开通任务')
          fulfill(s, o, body?.resourceId)
        } else {
          o.fulfillment = 'processing'
          o.fulfillmentError = null
        }
        audit('order.fulfillment_retry', 'order', o.id, { resourceId: body?.resourceId ?? null })
        return orderView(o)
      }),
    adminRequestRefund: ({ orderId, body }) =>
      wait(() => {
        writer('refund.request', 'order', orderId)
        const o = findOrder(orderId)
        if (o.payment !== 'confirmed') fail('precondition', '只有已确认收款的订单可以申请退款')
        if (db.refunds.some((r) => r.orderId === o.id && r.status === 'review')) fail('conflict', '这个订单已有待复核的退款')
        if (!body.reason.trim()) fail('invalid', '请填写退款原因')
        if (body.amount.amount <= 0 || body.amount.amount > o.amount.amount) fail('invalid', '退款金额须大于 0 且不超过订单金额')
        if (o.subscriptionId) {
          const s = findSub(o.subscriptionId)
          const remaining = Math.max(0, sums(s.id).bytes - s.usedBytes)
          const remainingSeconds = s.expiresAt && s.lifecycle === 'active' ? Math.max(0, Math.floor((Date.parse(s.expiresAt) - now) / 1000)) : 0
          if (body.reclaimBytes > remaining) fail('precondition', `回收流量不能超过订阅当前剩余 ${(remaining / GB).toFixed(2)} GB`)
          if (body.reclaimSeconds > remainingSeconds) fail('precondition', '回收时长不能超过订阅当前剩余时长')
        }
        const r = { id: nextId('rfd'), orderId: o.id, status: 'review' as const, amount: body.amount, reclaimBytes: body.reclaimBytes, reclaimSeconds: body.reclaimSeconds, reason: body.reason.trim(), externalRef: null, requestedBy: adminRef(), requestedAt: iso(now), decidedBy: null, decidedAt: null, decisionNote: null }
        db.refunds.unshift(r)
        o.payment = 'refund_review'
        audit('refund.request', 'refund', r.id, { orderId: o.id, amount: body.amount.amount })
        return r
      }),
    adminListRefunds: (input) =>
      wait(() => {
        me()
        const q = input?.query ?? {}
        const st = asArray(q.status as never) as string[] | undefined
        return paginate(
          db.refunds.filter((r) => !st?.length || st.includes(r.status)).sort((a, b) => b.requestedAt.localeCompare(a.requestedAt)),
          q,
        )
      }),
    adminDecideRefund: ({ refundId, body }) =>
      wait(() => {
        writer('refund.decide', 'refund', refundId)
        const r = db.refunds.find((x) => x.id === refundId) ?? fail('not_found', '没有找到这个退款')
        if (r.status !== 'review') fail('conflict', '这个退款已经处理过')
        const o = findOrder(r.orderId)
        if (body.approve) {
          if (!body.externalRef?.trim()) fail('invalid', '批准退款需要填写退款流水')
          const s = o.subscriptionId ? db.subs.find((x) => x.id === o.subscriptionId) : undefined
          if (s) {
            const remaining = Math.max(0, sums(s.id).bytes - s.usedBytes)
            if (r.reclaimBytes > remaining) fail('precondition', '订阅剩余流量已少于要回收的数量，请驳回后重新发起')
            if (r.reclaimBytes > 0) addLedger(s, { entitlementKind: 'traffic', amount: -r.reclaimBytes, sourceType: 'refund', sourceId: r.id, orderId: o.id, refundId: r.id, transferId: null, reason: `退款回收：${r.reason}`, admin: adminRef() })
            if (r.reclaimSeconds > 0) addLedger(s, { entitlementKind: 'time', amount: -r.reclaimSeconds, sourceType: 'refund', sourceId: r.id, orderId: o.id, refundId: r.id, transferId: null, reason: `退款回收：${r.reason}`, admin: adminRef() })
            event(s, 'refunded', `退款 ¥${(r.amount.amount / 100).toFixed(2)}，回收剩余权益`, { orderId: o.id })
          }
          r.status = 'approved'
          r.externalRef = body.externalRef!.trim()
          o.payment = 'refunded'
        } else {
          r.status = 'rejected'
          o.payment = 'confirmed'
        }
        r.decidedBy = adminRef()
        r.decidedAt = iso(now)
        r.decisionNote = body.note
        audit('refund.decide', 'refund', r.id, { approve: body.approve, note: body.note })
        return r
      }),

    // ---------- 换出口申请
    adminListExitRequests: (input) =>
      wait(() => {
        me()
        const q = input?.query ?? {}
        const st = asArray(q.status)
        const items = db.exitRequests
          .filter((r) => !st?.length || st.includes(r.status))
          .filter((r) => !q.feeStatus || r.feeStatus === q.feeStatus)
          .filter((r) => !q.subscriptionId || r.subscriptionId === q.subscriptionId)
          .filter((r) => !q.userId || r.user.id === q.userId)
          .sort((a, b) => b.createdAt.localeCompare(a.createdAt))
        return paginate(items, q)
      }),
    adminGetExitRequest: ({ exitRequestId }) =>
      wait(() => {
        me()
        return findReq(exitRequestId)
      }),
    adminUpdateExitRequestNote: ({ exitRequestId, body }) =>
      wait(() => {
        writer('exit_request.note', 'exit_request', exitRequestId)
        const r = findReq(exitRequestId)
        r.internalNote = body.internalNote
        r.updatedAt = iso(now)
        return r
      }),
    adminAcceptExitRequest: ({ exitRequestId, body }) =>
      wait((): AdminExitRequest => {
        writer('exit_request.accept', 'exit_request', exitRequestId)
        const r = findReq(exitRequestId)
        if (r.status !== 'pending') fail('precondition', '只能受理待处理的申请')
        if (body.fee === 'free' && r.charge !== 'free') fail('invalid', '超出免费次数的申请必须选择“收费”或“免收”')
        if (body.fee === 'charged') {
          if (!body.feeAmount || body.feeAmount.amount <= 0) fail('invalid', '收费时须填写金额')
          if (!body.feeExternalRef?.trim()) fail('invalid', '收费时须填写收款参考')
        }
        r.status = 'processing'
        r.feeStatus = body.fee === 'free' ? 'not_applicable' : body.fee
        r.feeAmount = body.fee === 'charged' ? body.feeAmount! : null
        r.feeExternalRef = body.fee === 'charged' ? body.feeExternalRef!.trim() : null
        if (body.internalNote !== undefined) r.internalNote = body.internalNote
        r.handledBy = adminRef()
        r.acceptedAt = iso(now)
        r.updatedAt = iso(now)
        audit('exit_request.accept', 'exit_request', r.id, { fee: body.fee, amount: r.feeAmount?.amount ?? null })
        return r
      }),
    adminRejectExitRequest: ({ exitRequestId, body }) =>
      wait(() => {
        writer('exit_request.reject', 'exit_request', exitRequestId)
        const r = findReq(exitRequestId)
        if (r.status !== 'pending' && r.status !== 'processing') fail('precondition', '只能驳回未完成的申请')
        if (!body.failureReason.trim()) fail('invalid', '请填写给客户看的驳回原因')
        r.status = 'failed'
        r.failureReason = body.failureReason.trim()
        if (body.internalNote !== undefined) r.internalNote = body.internalNote
        r.handledBy = adminRef()
        r.finishedAt = iso(now)
        r.updatedAt = iso(now)
        audit('exit_request.reject', 'exit_request', r.id, { failureReason: r.failureReason })
        return r
      }),
    adminCompleteExitRequest: ({ exitRequestId, body }) =>
      wait(() => {
        writer('exit_request.complete', 'exit_request', exitRequestId)
        const r = findReq(exitRequestId)
        if (r.status !== 'processing') fail('precondition', '只能收尾处理中的申请')
        if (body.outcome === 'succeeded') {
          const t = db.tasks.find((x) => x.t.id === body.taskId)
          if (!t || t.t.subscriptionId !== r.subscriptionId || t.t.state !== 'succeeded' || (t.t.kind !== 'exit_change' && t.t.kind !== 'combined')) fail('precondition', '需要指向该订阅已成功的换出口或组合更换任务')
          r.status = 'succeeded'
          r.taskId = body.taskId!
          r.newIp = db.subs.find((s) => s.id === r.subscriptionId)?.exit?.verifiedIp ?? null
        } else {
          if (!body.failureReason?.trim()) fail('invalid', '请填写给客户看的失败原因')
          r.status = 'failed'
          r.failureReason = body.failureReason!.trim()
        }
        if (body.internalNote !== undefined) r.internalNote = body.internalNote
        r.finishedAt = iso(now)
        r.updatedAt = iso(now)
        audit('exit_request.complete', 'exit_request', r.id, { outcome: body.outcome })
        return r
      }),

    // ---------- 节点、告警、审计
    adminListNodes: () =>
      wait(() => {
        me()
        return db.nodes.map(nodeView)
      }),
    adminCreateNode: ({ body }) =>
      wait(() => {
        writer('node.create')
        if (db.nodes.some((n) => n.name === body.name)) fail('conflict', '节点名称已存在')
        const range = body.portRange ?? { start: 20000, end: 29999 }
        if (range.end <= range.start) fail('invalid', '端口范围不正确')
        const n: Node = { id: nextId('nod'), name: body.name, publicHost: body.publicHost, portRange: range, status: 'active', online: false, lastSeenAt: null, lastReportAt: null, desiredRevision: 0, appliedRevision: 0, agentVersion: null, persistOk: null, clockSkewMs: null, capabilities: {}, ports: { total: range.end - range.start + 1, free: range.end - range.start + 1, allocated: 0, cooldown: 0 }, subscriptions: 0, activeConns: null, createdAt: iso(now) }
        db.nodes.push(n)
        audit('node.create', 'node', n.id, { name: n.name })
        return nodeView(n)
      }),
    adminGetNode: ({ nodeId }) =>
      wait(() => {
        me()
        return nodeView(db.nodes.find((n) => n.id === nodeId) ?? fail('not_found', '没有找到这个节点'))
      }),
    adminUpdateNode: ({ nodeId, body }) =>
      wait(() => {
        writer('node.update', 'node', nodeId)
        const n = db.nodes.find((x) => x.id === nodeId) ?? fail('not_found', '没有找到这个节点')
        Object.assign(n, Object.fromEntries(Object.entries(body).filter(([, v]) => v !== undefined)))
        audit('node.update', 'node', n.id, body)
        return nodeView(n)
      }),
    adminListAlerts: (input) =>
      wait(() => {
        me()
        const q = input?.query ?? {}
        const st = asArray(q.status)
        const sev = { critical: 0, warning: 1, info: 2 }
        const items = db.alerts
          .filter((a) => !st?.length || st.includes(a.status))
          .filter((a) => !q.kind || a.kind === q.kind)
          .filter((a) => !q.severity || a.severity === q.severity)
          .filter((a) => !q.nodeId || a.nodeId === q.nodeId)
          .filter((a) => !q.subscriptionId || a.subscriptionId === q.subscriptionId)
          .sort((a, b) => (a.status === 'open' ? 0 : 1) - (b.status === 'open' ? 0 : 1) || sev[a.severity] - sev[b.severity] || b.lastSeenAt.localeCompare(a.lastSeenAt))
        return paginate(items, q)
      }),
    adminAckAlert: ({ alertId, body }) =>
      wait(() => {
        writer('alert.ack', 'alert', alertId)
        const a = db.alerts.find((x) => x.id === alertId) ?? fail('not_found', '没有找到这条告警')
        if (a.status === 'open') {
          a.status = 'acked'
          a.ackedBy = adminRef()
          a.ackedAt = iso(now)
          a.ackNote = body?.note ?? null
          audit('alert.ack', 'alert', a.id, { note: a.ackNote })
        }
        return a
      }),
    adminListAuditLogs: (input) =>
      wait(() => {
        me()
        const q = input?.query ?? {}
        const items = db.audit
          .filter((l) => !q.actorKind || l.actor.kind === q.actorKind)
          .filter((l) => !q.adminId || l.actor.id === q.adminId)
          .filter((l) => !q.userId || l.targetId === q.userId || l.actor.id === q.userId)
          .filter((l) => !q.action || (q.action.endsWith('.') ? l.action.startsWith(q.action) : l.action === q.action || l.action.startsWith(`${q.action}.`)))
          .filter((l) => !q.targetType || l.targetType === q.targetType)
          .filter((l) => !q.targetId || l.targetId === q.targetId)
          .filter((l) => !q.from || l.at >= q.from)
          .filter((l) => !q.to || l.at <= q.to)
          .sort((a, b) => b.at.localeCompare(a.at) || b.id - a.id)
        return paginate(items, q)
      }),
  }

  void MIN
  void HOUR
  const controls: MockAdminControls = {
    expireSession() {
      session.stage = 'none'
      session.adminId = null
    },
    signIn(role = 'admin') {
      session.stage = 'ok'
      session.adminId = db.admins.find((a) => a.admin.role === role)!.admin.id
    },
    now,
  }
  return Object.assign(api, { mock: controls })
}

export type { Upstream }
