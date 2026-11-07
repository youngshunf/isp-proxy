import { describe, expect, it } from 'vitest'
import { connectionString } from '../index'
import { createMockApi } from './index'

const api = () => createMockApi({ latency: [0, 0] })

describe('mock 服务端规则', () => {
  it('阻断原因按优先级推导：耗尽、到期、待履约分别可见', async () => {
    const subs = await api().listSubscriptions()
    const by = Object.fromEntries(subs.map((s) => [s.id, s.blockers]))
    expect(by.sub_7k2m).toEqual([])
    expect(by.sub_j9tx).toEqual(['traffic_exhausted'])
    expect(by.sub_s4wp).toEqual(['expired'])
    expect(by.sub_g5ln).toEqual(['pending'])
  })

  it('换接入只改接入，不改出口与余量，并扣次数', async () => {
    const a = api()
    const before = await a.getSubscription('sub_7k2m')
    const acc = await a.changeAccess('sub_7k2m', 'password', 'k-1')
    const after = await a.getSubscription('sub_7k2m')
    expect(acc.version).toBe(before.access!.version + 1)
    expect(acc.ports).toEqual(before.access!.ports)
    expect(acc.password).not.toBe(before.access!.password)
    expect(acc.unseenChanges).toEqual(['password'])
    expect(after.exit).toEqual(before.exit)
    expect(after.traffic).toEqual(before.traffic)
    expect(after.time.expiresAt).toBe(before.time.expiresAt)
    expect(after.quotas.accessChangesLeft).toBe(before.quotas.accessChangesLeft - 1)
  })

  it('同一订阅已有换出口申请时拒绝重复提交，并带回现有申请', async () => {
    await expect(
      api().createExitRequest({ subscriptionId: 'sub_n2ry', reason: 'quality', note: '', alsoChangeAccess: false }, 'k-1'),
    ).rejects.toMatchObject({ code: 'conflict', details: { existing: { id: 'req_0007' } } })
  })

  it('同一幂等键重复提交返回首次结果，不重复下单或扣次数', async () => {
    const a = api()
    const o1 = await a.createOrder({ kind: 'renew_time', subscriptionId: 'sub_7k2m', months: 1 }, 'same')
    const o2 = await a.createOrder({ kind: 'renew_time', subscriptionId: 'sub_7k2m', months: 1 }, 'same')
    expect(o2.id).toBe(o1.id)
    expect((await a.listOrders()).filter((o) => o.id === o1.id)).toHaveLength(1)
    const before = (await a.getSubscription('sub_7k2m')).quotas.accessChangesLeft
    await a.changeAccess('sub_7k2m', 'password', 'acc')
    await a.changeAccess('sub_7k2m', 'password', 'acc')
    expect((await a.getSubscription('sub_7k2m')).quotas.accessChangesLeft).toBe(before - 1)
  })

  it('新购数量只能为 1（D-13）', async () => {
    await expect(api().createOrder({ kind: 'new', planId: 'us-lax', months: 1, quantity: 2 }, 'k')).rejects.toMatchObject({ code: 'invalid' })
  })

  it('注册不登录；带邮箱验证即登录', async () => {
    const a = createMockApi({ latency: [0, 0], signedIn: false })
    await a.register('New@Example.com', 'long-password-1')
    expect(await a.session()).toBeNull()
    await expect(a.verifyEmail('000000', 'new@example.com')).rejects.toMatchObject({ code: 'invalid' })
    const acc = await a.verifyEmail('123456', 'new@example.com')
    expect(acc).toMatchObject({ email: 'new@example.com', verified: true })
    expect(await a.session()).toMatchObject({ email: 'new@example.com' })
  })

  it('用户反馈“已付款”不改变收款状态', async () => {
    const a = api()
    const o = await a.createOrder({ kind: 'add_traffic', subscriptionId: 'sub_j9tx', packId: 't10' }, 'k-2')
    const claimed = await a.claimPaid(o.id)
    expect(claimed.payment).toBe('awaiting')
    expect(claimed.userClaimedPaidAt).not.toBeNull()
  })

  it('连接串对凭据做百分号编码', () => {
    const s = connectionString(
      { version: 1, host: 'proxy.example.com', username: 'u@1', password: 'p:w/d', ports: { http: 1, https: 2, socks5: 3 }, issuedAt: '', unseenChanges: [] },
      'socks5',
    )
    expect(s).toBe('socks5://u%401:p%3Aw%2Fd@proxy.example.com:3')
  })
})
