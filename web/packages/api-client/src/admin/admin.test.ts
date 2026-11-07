import { describe, expect, it, vi } from 'vitest'
import type { operations, paths } from '../generated/schema'
import { ApiError } from '../index'
import { NetworkError, toQueryString } from '../http'
import { createAdminApi } from './client'
import { ADMIN_ROUTES } from './routes'
import type { AdminInput, AdminOp, AdminResult } from './types'
import { MOCK_ADMIN_LOGIN, createMockAdminApi } from './mock'

// ---------------------------------------------------------------- 类型层（tsc 检查）
type Equal<A, B> = (<T>() => T extends A ? 1 : 2) extends <T>() => T extends B ? 1 : 2 ? true : false
function assert<T extends true>(): T | undefined {
  return undefined
}

// 路由表里每一项的方法与路径都指向 schema.ts 中同一个 operation。
type RouteOk<O extends AdminOp> = (typeof ADMIN_ROUTES)[O] extends readonly [infer M extends string, infer P extends keyof paths]
  ? Lowercase<M> extends keyof paths[P]
    ? paths[P][Lowercase<M>] extends operations[O]
      ? true
      : false
    : false
  : false
assert<Equal<{ [O in AdminOp]: RouteOk<O> }[AdminOp], true>>()

// 入参推导：路径参数平铺、body 必填、幂等键只出现在契约要求的操作上。
assert<Equal<AdminInput<'adminPauseSubscription'>, { subscriptionId: string; body: { reason: string } }>>()
assert<Equal<keyof AdminInput<'adminConfirmPayment'>, 'orderId' | 'body'>>()
assert<Equal<keyof AdminInput<'adminSubmitExitChange'>, 'subscriptionId' | 'body' | 'idempotencyKey'>>()
assert<Equal<keyof AdminInput<'adminGetSubscriptionUsage'>, 'subscriptionId' | 'query'>>()
assert<Equal<AdminInput<'adminMe'>, {}>>()
assert<Equal<AdminResult<'adminLogout'>, void>>()
assert<Equal<AdminResult<'adminCheckExit'>, operations['adminCheckExit']['responses'][202]['content']['application/json']>>()

// ---------------------------------------------------------------- 真实客户端
function fakeFetch(status: number, body: unknown, headers: Record<string, string> = {}) {
  return vi.fn(async (_url: string, _init?: RequestInit) => new Response(status === 204 ? null : JSON.stringify(body), { status, headers }))
}

describe('createAdminApi', () => {
  it('带 Cookie、CSRF 头与幂等键，路径参数编码', async () => {
    const f = fakeFetch(200, { id: 'tsk_1' })
    const api = createAdminApi({ fetch: f as unknown as typeof fetch, readCookie: (n) => (n === 'isp_csrf' ? 'tok-123' : null) })
    await api.adminChangeAccess({ subscriptionId: 'sub_a/b', body: { mode: 'password', reason: '泄露' }, idempotencyKey: 'k-1' })
    const [url, init] = f.mock.calls[0]!
    expect(url).toBe('/admin-api/subscriptions/sub_a%2Fb/access-changes')
    expect(init?.method).toBe('POST')
    expect(init?.credentials).toBe('include')
    expect(init?.headers).toMatchObject({ 'X-CSRF-Token': 'tok-123', 'Idempotency-Key': 'k-1', 'Content-Type': 'application/json' })
    expect(JSON.parse(String(init?.body))).toEqual({ mode: 'password', reason: '泄露' })
  })

  it('GET 不带 CSRF 头；数组查询按逗号连接，空值省略', async () => {
    const f = fakeFetch(200, { items: [], nextCursor: null })
    const api = createAdminApi({ fetch: f as unknown as typeof fetch, readCookie: () => 'tok' })
    await api.adminListOrders({ query: { payment: ['awaiting', 'confirmed'], q: '', limit: 20 } })
    const [url, init] = f.mock.calls[0]!
    expect(url).toBe('/admin-api/orders?payment=awaiting%2Cconfirmed&limit=20')
    expect(init?.headers).not.toHaveProperty('X-CSRF-Token')
    expect(toQueryString({ a: [], b: undefined })).toBe('')
  })

  it('契约错误转为 ApiError，带恢复动作与 Retry-After', async () => {
    const f = fakeFetch(429, { code: 'rate_limited', message: '太频繁', action: { label: '稍后', href: '/x' } }, { 'Retry-After': '30' })
    const api = createAdminApi({ fetch: f as unknown as typeof fetch })
    const e = await api.adminMe().catch((x: unknown) => x)
    expect(e).toBeInstanceOf(ApiError)
    expect(e).toMatchObject({ code: 'rate_limited', message: '太频繁', status: 429, retryAfter: 30, action: { label: '稍后', href: '/x' } })
  })

  it('5xx 与非契约响应抛 NetworkError，204 返回 undefined', async () => {
    const bad = createAdminApi({ fetch: fakeFetch(502, 'oops') as unknown as typeof fetch })
    await expect(bad.adminMe()).rejects.toBeInstanceOf(NetworkError)
    const ok = createAdminApi({ fetch: fakeFetch(204, null) as unknown as typeof fetch })
    await expect(ok.adminLogout()).resolves.toBeUndefined()
  })
})

// ---------------------------------------------------------------- mock 规则
const mock = (o: Parameters<typeof createMockAdminApi>[0] = {}) => createMockAdminApi({ latency: [0, 0], signedIn: true, jobSteps: 1, ...o })

async function subByName(api: ReturnType<typeof mock>, name: string) {
  const page = await api.adminListSubscriptions({ query: { q: name, limit: 200 } })
  return page.items.find((s) => s.name === name)!
}

describe('mock：登录与权限', () => {
  it('密码 → TOTP 两步；未完成第二步前 /me 为 401', async () => {
    const api = createMockAdminApi({ latency: [0, 0] })
    const ch = await api.adminLogin({ body: { email: MOCK_ADMIN_LOGIN.email, password: MOCK_ADMIN_LOGIN.password } })
    expect(ch.stage).toBe('totp_required')
    await expect(api.adminMe()).rejects.toMatchObject({ code: 'unauthorized' })
    await expect(api.adminVerifyMfa({ body: { code: '000000' } })).rejects.toMatchObject({ code: 'invalid' })
    const s = await api.adminVerifyMfa({ body: { code: MOCK_ADMIN_LOGIN.totp } })
    expect(s.admin.email).toBe(MOCK_ADMIN_LOGIN.email)
    expect(s.recoveryCodes).toBeUndefined()
  })

  it('首次登录返回绑定密钥，绑定成功只这一次返回 10 个恢复码', async () => {
    const api = createMockAdminApi({ latency: [0, 0] })
    const ch = await api.adminLogin({ body: { email: MOCK_ADMIN_LOGIN.enrollEmail, password: MOCK_ADMIN_LOGIN.enrollPassword } })
    expect(ch.stage).toBe('enrollment_required')
    expect(ch.enrollment?.otpauthUri).toMatch(/^otpauth:\/\/totp\//)
    const s = await api.adminVerifyMfa({ body: { code: MOCK_ADMIN_LOGIN.totp } })
    expect(s.recoveryCodes).toHaveLength(10)
  })

  it('只读角色写操作 403，会话过期后 401', async () => {
    const api = mock({ signedIn: 'viewer' })
    const sub = await subByName(api, 'TikTok 美区 1 号')
    await expect(api.adminPauseSubscription({ subscriptionId: sub.id, body: { reason: 'x' } })).rejects.toMatchObject({ code: 'forbidden' })
    api.mock.expireSession()
    await expect(api.adminGetOverview()).rejects.toMatchObject({ code: 'unauthorized' })
  })
})

describe('mock：收款确认', () => {
  it('以订单为业务键只入账一次；内容不同 409；金额不符 422', async () => {
    const api = mock()
    const page = await api.adminListOrders({ query: { payment: ['awaiting'], kind: 'add_traffic' } })
    const o = page.items.find((x) => x.subscriptionName === '纽约 2 号')!
    const before = await api.adminListLedger({ subscriptionId: o.subscriptionId! })
    await expect(
      api.adminConfirmPayment({ orderId: o.id, body: { method: 'manual_transfer', amount: { amount: 1, currency: 'CNY' }, externalRef: 'B-1', lateArrival: false } }),
    ).rejects.toMatchObject({ code: 'invalid' })
    const body = { method: 'manual_transfer' as const, amount: o.amount, externalRef: 'BANK-0001', lateArrival: false }
    const [a, b] = await Promise.all([api.adminConfirmPayment({ orderId: o.id, body }), api.adminConfirmPayment({ orderId: o.id, body })])
    expect(a.payment).toBe('confirmed')
    expect(b.paymentRecord?.id).toBe(a.paymentRecord?.id)
    const after = await api.adminListLedger({ subscriptionId: o.subscriptionId! })
    expect(after.entitledBytesTotal - before.entitledBytesTotal).toBe(50_000_000_000)
    await expect(api.adminConfirmPayment({ orderId: o.id, body: { ...body, externalRef: 'BANK-0002' } })).rejects.toMatchObject({ code: 'conflict' })
  })

  it('已超时取消的订单须标记晚到款', async () => {
    const api = mock()
    const page = await api.adminListOrders({ query: { payment: ['cancelled'] } })
    const o = page.items[0]!
    const body = { method: 'manual_transfer' as const, amount: o.amount, externalRef: 'LATE-1', lateArrival: false }
    await expect(api.adminConfirmPayment({ orderId: o.id, body })).rejects.toMatchObject({ code: 'precondition' })
    const ok = await api.adminConfirmPayment({ orderId: o.id, body: { ...body, lateArrival: true } })
    expect(ok.paymentRecord?.lateArrival).toBe(true)
    expect(ok.fulfillment).toBe('failed')
  })
})

describe('mock：账本与划转', () => {
  it('赠送只能为正，扣回不能让合计为负', async () => {
    const api = mock()
    const s = await subByName(api, 'TikTok 美区 1 号')
    const k = (n: string) => `k-${n}`
    await expect(api.adminCreateLedgerEntry({ subscriptionId: s.id, idempotencyKey: k('1'), body: { sourceType: 'gift', entitlementKind: 'traffic', amount: -1, reason: 'x' } })).rejects.toMatchObject({ code: 'invalid' })
    await expect(api.adminCreateLedgerEntry({ subscriptionId: s.id, idempotencyKey: k('2'), body: { sourceType: 'adjust', entitlementKind: 'traffic', amount: -1e15, reason: 'x' } })).rejects.toMatchObject({ code: 'precondition' })
    const e = await api.adminCreateLedgerEntry({ subscriptionId: s.id, idempotencyKey: k('3'), body: { sourceType: 'gift', entitlementKind: 'traffic', amount: 5e9, reason: '补偿' } })
    const again = await api.adminCreateLedgerEntry({ subscriptionId: s.id, idempotencyKey: k('3'), body: { sourceType: 'gift', entitlementKind: 'traffic', amount: 5e9, reason: '补偿' } })
    expect(again.id).toBe(e.id)
  })

  it('划转只限同一用户，来源余额归零', async () => {
    const api = mock()
    const from = await subByName(api, '旧美区线路')
    const to = await subByName(api, '纽约 2 号')
    const other = await subByName(api, 'TikTok 美区 1 号')
    await expect(api.adminTransferBalance({ idempotencyKey: 't0', body: { fromSubscriptionId: from.id, toSubscriptionId: other.id, reason: 'x' } })).rejects.toMatchObject({ code: 'precondition' })
    const t = await api.adminTransferBalance({ idempotencyKey: 't1', body: { fromSubscriptionId: from.id, toSubscriptionId: to.id, reason: '合并' } })
    expect(t.bytes).toBe(from.traffic.remainingBytes)
    const after = await api.adminGetSubscription({ subscriptionId: from.id })
    expect(after.traffic.remainingBytes).toBe(0)
  })
})

describe('mock：换出口', () => {
  it('比较给出拒绝原因；未通过比较不能提交；通过后任务完成并更新出口', async () => {
    const api = mock()
    const sub = await subByName(api, 'TikTok 美区 1 号')
    const ups = await api.adminListUpstreams({ query: { status: ['available', 'pending_verify'], limit: 200 } })
    const byHost = (h: string) => ups.items.find((u) => u.host === h)!

    const same = await api.adminCompareExitCandidate({ subscriptionId: sub.id, body: { resourceId: byHost('gw-c3.isp-a.example.net').id } })
    const sameDone = await api.adminGetProbe({ probeId: same.id })
    expect(sameDone.comparison).toMatchObject({ differsFromCurrent: false, passed: false })
    await expect(
      api.adminSubmitExitChange({ subscriptionId: sub.id, idempotencyKey: 'x1', body: { resourceId: sameDone.resourceId!, compareProbeId: same.id, alsoChangeAccess: false, reason: 'r' } }),
    ).rejects.toMatchObject({ code: 'precondition' })

    const taken = await api.adminGetProbe({ probeId: (await api.adminCompareExitCandidate({ subscriptionId: sub.id, body: { resourceId: byHost('gw-c4.isp-c.example.net').id } })).id })
    expect(taken.comparison).toMatchObject({ uniqueOnPlatform: false, passed: false })
    const gb = await api.adminGetProbe({ probeId: (await api.adminCompareExitCandidate({ subscriptionId: sub.id, body: { resourceId: byHost('gw-c5.isp-b.example.net').id } })).id })
    expect(gb.comparison).toMatchObject({ countryMatches: false, passed: false })
    const dead = await api.adminGetProbe({ probeId: (await api.adminCompareExitCandidate({ subscriptionId: sub.id, body: { resourceId: byHost('gw-c6.isp-c.example.net').id } })).id })
    expect(dead.result).toBe('failed')
    expect(dead.error).toMatch(/认证失败/)

    const ok = await api.adminGetProbe({ probeId: (await api.adminCompareExitCandidate({ subscriptionId: sub.id, body: { resourceId: byHost('gw-c1.isp-b.example.net').id } })).id })
    expect(ok.comparison?.passed).toBe(true)
    const task = await api.adminSubmitExitChange({ subscriptionId: sub.id, idempotencyKey: 'x2', body: { resourceId: ok.resourceId!, compareProbeId: ok.id, alsoChangeAccess: true, reason: '目标站风控' } })
    expect(task.kind).toBe('combined')
    await expect(api.adminChangeAccess({ subscriptionId: sub.id, idempotencyKey: 'a1', body: { mode: 'password', reason: 'x' } })).rejects.toMatchObject({ code: 'conflict' })
    const done = await api.adminGetTask({ taskId: task.id })
    expect(done.state).toBe('succeeded')
    const after = await api.adminGetSubscription({ subscriptionId: sub.id })
    expect(after.exit?.verifiedIp).toBe('203.0.113.77')
    expect(after.access?.version).toBe(sub.access!.version + 1)
    expect(after.traffic.remainingBytes).toBe(sub.traffic.remainingBytes)
  })
})

describe('mock：用户封禁与趋势', () => {
  it('封禁后全部订阅出现 account_banned', async () => {
    const api = mock()
    const users = await api.adminListUsers({ query: { q: 'grace@' } })
    await api.adminBanUser({ userId: users.items[0]!.id, body: { reason: '测试' } })
    const d = await api.adminGetUser({ userId: users.items[0]!.id })
    expect(d.status).toBe('banned')
    expect(d.activeSessions).toBe(0)
    for (const s of d.subscriptions) expect(s.blockers[0]).toBe('account_banned')
  })

  it('趋势区分 fresh / stale / no_data，并保留真实零值与空桶', async () => {
    const api = mock()
    const range = { from: new Date(api.mock.now - 86_400_000).toISOString(), to: new Date(api.mock.now).toISOString() }
    const get = async (name: string) => api.adminGetSubscriptionUsage({ subscriptionId: (await subByName(api, name)).id, query: range })
    const fresh = await get('TikTok 美区 1 号')
    expect(fresh.freshness).toBe('fresh')
    const stale = await get('亚马逊德国店')
    expect(stale.freshness).toBe('stale')
    expect(stale.points.at(-1)!.bytes).toBeNull()
    const idle = await get('夜间任务')
    expect(idle.points.at(-1)!.bytes).toBe(0)
    const gappy = await get('纽约 2 号')
    expect(gappy.freshness).toBe('fresh')
    expect(gappy.points.some((p) => p.bytes === null)).toBe(true)
    const none = await api.adminGetSubscriptionUsage({ subscriptionId: (await api.adminListSubscriptions({ query: { lifecycle: ['pending'] } })).items[0]!.id, query: range })
    expect(none.freshness).toBe('no_data')
  })
})
