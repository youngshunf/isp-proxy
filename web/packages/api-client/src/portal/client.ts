// PortalApi 的真实实现：与 AdminApi 共用 fetch 层（../http），路由表与入参/结果类型都从 schema.ts 推导。
import { ApiError, type PortalApi } from '../index'
import { createHttp, type HttpOptions, type QueryValue } from '../http'
import type { operations, paths } from '../generated/schema'
import type { OpInput, OpResult } from '../ops'

/** 用户端全部操作（operationId 不以 admin 开头）。 */
export type PortalOp = Exclude<keyof operations, `admin${string}`>
export type PortalPath = Extract<keyof paths, `/api/${string}`>
type HttpMethod = 'GET' | 'POST' | 'PATCH' | 'DELETE'

// operationId → HTTP 方法与路径。portal.test.ts 在类型层校验每一项都与 schema.ts 的 paths 对得上。
export const PORTAL_ROUTES = {
  session: ['GET', '/api/me'],
  login: ['POST', '/api/sessions'],
  logout: ['DELETE', '/api/sessions/current'],
  register: ['POST', '/api/account/register'],
  verifyEmail: ['POST', '/api/account/verify-email'],
  resendVerification: ['POST', '/api/account/resend-verification'],
  requestPasswordReset: ['POST', '/api/account/password-reset'],
  confirmPasswordReset: ['POST', '/api/account/password-reset/confirm'],
  changePassword: ['POST', '/api/account/change-password'],
  listSubscriptions: ['GET', '/api/subscriptions'],
  getSubscription: ['GET', '/api/subscriptions/{subscriptionId}'],
  renameSubscription: ['PATCH', '/api/subscriptions/{subscriptionId}'],
  listSubscriptionEvents: ['GET', '/api/subscriptions/{subscriptionId}/events'],
  changeAccess: ['POST', '/api/subscriptions/{subscriptionId}/access-changes'],
  acknowledgeAccess: ['POST', '/api/subscriptions/{subscriptionId}/access/ack'],
  getCatalog: ['GET', '/api/catalog'],
  listOrders: ['GET', '/api/orders'],
  createOrder: ['POST', '/api/orders'],
  getOrder: ['GET', '/api/orders/{orderId}'],
  claimPaid: ['POST', '/api/orders/{orderId}/claim-paid'],
  cancelOrder: ['POST', '/api/orders/{orderId}/cancel'],
  listExitRequests: ['GET', '/api/exit-requests'],
  createExitRequest: ['POST', '/api/exit-requests'],
} as const satisfies Record<PortalOp, readonly [HttpMethod, PortalPath]>

type AnyInput = { query?: Record<string, QueryValue>; body?: unknown; idempotencyKey?: string; [pathParam: string]: unknown }

function fillPath(template: string, params: Record<string, unknown>): string {
  return template.replace(/\{(\w+)\}/g, (_, k: string) => {
    const v = params[k]
    if (typeof v !== 'string' || v === '') throw new Error(`缺少路径参数 ${k}`)
    return encodeURIComponent(v)
  })
}

/** 按 operationId 调用的底层函数：入参与结果类型由契约推导。 */
export type PortalCall = <O extends PortalOp>(op: O, input: OpInput<O>) => Promise<OpResult<O>>

export function createPortalCall(opts: HttpOptions = {}): PortalCall {
  const request = createHttp({ csrfCookie: 'isp_csrf', csrfBootstrap: '/api/me', ...opts })
  return (async (op: PortalOp, input: AnyInput = {}) => {
    const [method, path] = PORTAL_ROUTES[op]
    const { query, body, idempotencyKey, ...pathParams } = input
    return request(method, fillPath(path, pathParams), { query, body, idempotencyKey })
  }) as PortalCall
}

/** 用户前台的真实客户端（同源 /api，反向代理或 Vite 代理接到 isp-control）。 */
export function createPortalApi(opts: HttpOptions = {}): PortalApi {
  const call = createPortalCall(opts)
  const sub = (subscriptionId: string) => ({ subscriptionId })
  return {
    session: async () => {
      try {
        return await call('session', {})
      } catch (e) {
        if (e instanceof ApiError && e.code === 'unauthorized') return null
        throw e
      }
    },
    login: (email, password) => call('login', { body: { email, password } }),
    register: (email, password) => call('register', { body: { email, password } }),
    verifyEmail: (code, email) => call('verifyEmail', { body: email ? { code, email } : { code } }),
    resendVerification: async (email) => {
      await call('resendVerification', email ? { body: { email } } : {})
    },
    requestPasswordReset: async (email) => {
      await call('requestPasswordReset', { body: { email } })
    },
    confirmPasswordReset: async (email, code, newPassword) => {
      await call('confirmPasswordReset', { body: { email, code, newPassword } })
    },
    changePassword: async (current, next) => {
      await call('changePassword', { body: { current, next } })
    },
    logout: async () => {
      await call('logout', {})
    },

    listSubscriptions: () => call('listSubscriptions', {}),
    getSubscription: (id) => call('getSubscription', sub(id)),
    listSubscriptionEvents: (id) => call('listSubscriptionEvents', sub(id)),
    renameSubscription: (id, name) => call('renameSubscription', { ...sub(id), body: { name } }),
    changeAccess: (id, mode, idempotencyKey) => call('changeAccess', { ...sub(id), body: { mode }, idempotencyKey }),
    acknowledgeAccess: async (id) => {
      await call('acknowledgeAccess', sub(id))
    },

    getCatalog: () => call('getCatalog', {}),
    createOrder: (input, idempotencyKey) => call('createOrder', { body: input, idempotencyKey }),
    listOrders: () => call('listOrders', {}),
    getOrder: (orderId) => call('getOrder', { orderId }),
    claimPaid: (orderId) => call('claimPaid', { orderId }),
    cancelOrder: (orderId) => call('cancelOrder', { orderId }),

    listExitRequests: () => call('listExitRequests', {}),
    createExitRequest: (input, idempotencyKey) => call('createExitRequest', { body: input, idempotencyKey }),
  }
}
