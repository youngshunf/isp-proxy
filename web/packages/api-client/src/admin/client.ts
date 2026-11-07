// AdminApi 的真实实现：按 ADMIN_ROUTES 生成每个 operationId 对应的方法。
import { createHttp, type HttpOptions, type QueryValue } from '../http'
import { ADMIN_ROUTES } from './routes'
import type { AdminApi, AdminOp } from './types'

type AnyInput = {
  query?: Record<string, QueryValue>
  body?: unknown
  idempotencyKey?: string
  [pathParam: string]: unknown
}

export function fillPath(template: string, params: Record<string, unknown>): string {
  return template.replace(/\{(\w+)\}/g, (_, k: string) => {
    const v = params[k]
    if (typeof v !== 'string' || v === '') throw new Error(`缺少路径参数 ${k}`)
    return encodeURIComponent(v)
  })
}

export function createAdminApi(opts: HttpOptions = {}): AdminApi {
  const request = createHttp({ csrfCookie: 'isp_csrf', csrfBootstrap: '/admin-api/me', ...opts })
  const api: Partial<Record<AdminOp, (input?: AnyInput) => Promise<unknown>>> = {}
  for (const op of Object.keys(ADMIN_ROUTES) as AdminOp[]) {
    const [method, path] = ADMIN_ROUTES[op]
    api[op] = (input: AnyInput = {}) => {
      const { query, body, idempotencyKey, ...pathParams } = input
      return request(method, fillPath(path, pathParams), { query, body, idempotencyKey })
    }
  }
  return api as unknown as AdminApi
}
