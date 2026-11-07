import { describe, expect, it, vi } from 'vitest'
import type { operations, paths } from '../generated/schema'
import { ApiError, type PortalApi } from '../index'
import type { MockPortalApi } from '../mock'
import { PORTAL_ROUTES, createPortalApi, type PortalOp } from './client'

// ---------------------------------------------------------------- 类型层（tsc 检查）
type Equal<A, B> = (<T>() => T extends A ? 1 : 2) extends <T>() => T extends B ? 1 : 2 ? true : false
function assert<T extends true>(): T | undefined {
  return undefined
}
type RouteOk<O extends PortalOp> = (typeof PORTAL_ROUTES)[O] extends readonly [infer M extends string, infer P extends keyof paths]
  ? Lowercase<M> extends keyof paths[P]
    ? paths[P][Lowercase<M>] extends operations[O]
      ? true
      : false
    : false
  : false
assert<Equal<{ [O in PortalOp]: RouteOk<O> }[PortalOp], true>>()
// PortalApi 的方法与契约操作一一对应（方法名即 operationId）；mock 实现同一接口。
assert<Equal<keyof PortalApi, PortalOp>>()
assert<MockPortalApi extends PortalApi ? true : false>()

function fakeFetch(...responses: { status: number; body?: unknown; headers?: Record<string, string> }[]) {
  let i = 0
  return vi.fn(async (_url: string, _init?: RequestInit) => {
    const r = responses[Math.min(i++, responses.length - 1)]!
    return new Response(r.status === 204 || r.body === undefined ? null : JSON.stringify(r.body), { status: r.status, headers: r.headers })
  })
}

describe('createPortalApi', () => {
  it('未登录时 session 返回 null，不当作错误', async () => {
    const f = fakeFetch({ status: 401, body: { code: 'unauthorized', message: '请先登录' } })
    const api = createPortalApi({ fetch: f as unknown as typeof fetch })
    expect(await api.session()).toBeNull()
    expect(f.mock.calls[0]![0]).toBe('/api/me')
  })

  it('写请求带 CSRF 与幂等键；没有 CSRF Cookie 时先 GET /api/me 取得', async () => {
    let cookie: string | null = null
    const f = vi.fn(async (url: string, _init?: RequestInit) => {
      if (url === '/api/me') {
        cookie = 'tok-1'
        return new Response(JSON.stringify({ code: 'unauthorized', message: 'x' }), { status: 401 })
      }
      return new Response(JSON.stringify({ id: 'ord_1' }), { status: 201 })
    })
    const api = createPortalApi({ fetch: f as unknown as typeof fetch, readCookie: () => cookie })
    await api.createOrder({ kind: 'new', planId: 'pln_1', months: 1, quantity: 1 }, 'key-1')
    expect(f.mock.calls.map((c) => c[0])).toEqual(['/api/me', '/api/orders'])
    expect(f.mock.calls[1]![1]?.headers).toMatchObject({ 'X-CSRF-Token': 'tok-1', 'Idempotency-Key': 'key-1' })
  })

  it('未登录验证邮箱时带 email；路径参数编码', async () => {
    const f = fakeFetch({ status: 200, body: { email: 'a@b.c', verified: true, status: 'active', createdAt: '' } }, { status: 204 })
    const api = createPortalApi({ fetch: f as unknown as typeof fetch, readCookie: () => 't' })
    await api.verifyEmail('123456', 'a@b.c')
    expect(JSON.parse(String(f.mock.calls[0]![1]?.body))).toEqual({ code: '123456', email: 'a@b.c' })
    await api.acknowledgeAccess('sub_x/y')
    expect(f.mock.calls[1]![0]).toBe('/api/subscriptions/sub_x%2Fy/access/ack')
  })

  it('换出口申请冲突：409 带回现有申请（details.existing）', async () => {
    const existing = { id: 'req_1', no: 'REQ-1' }
    const f = fakeFetch({ status: 409, body: { code: 'conflict', message: '已有申请', existing } })
    const api = createPortalApi({ fetch: f as unknown as typeof fetch, readCookie: () => 't' })
    const err = await api
      .createExitRequest({ subscriptionId: 'sub_1', reason: 'quality', note: '', alsoChangeAccess: false }, 'k')
      .catch((e: unknown) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect((err as ApiError).details).toEqual({ existing })
  })

  it('响应头 Date 交给服务端时钟', async () => {
    const seen: number[] = []
    const f = fakeFetch({ status: 200, body: [], headers: { Date: 'Sun, 08 Nov 2026 00:00:00 GMT' } })
    const api = createPortalApi({ fetch: f as unknown as typeof fetch, onServerTime: (ms) => seen.push(ms) })
    await api.listOrders()
    expect(seen).toEqual([Date.parse('2026-11-08T00:00:00Z')])
  })
})
