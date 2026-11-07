// 轻量 fetch 客户端：两端共用（用户前台转正时同样使用，docs/05 §6）。
// - 会话在 HttpOnly Cookie 里，请求一律 credentials: 'include'。
// - CSRF 双提交：非 GET 请求带 X-CSRF-Token，值取自非 HttpOnly 的 isp_csrf Cookie（docs/06 §4）。
// - 契约错误（带 {code, message, action}）转成 ApiError；网络错误与 5xx 抛 NetworkError，界面统一提示稍后重试。
// - 不自动重放写请求；读请求的有限重试交给 TanStack Query。
import { ApiError, type ApiErrorCode } from './index'

export type QueryValue = string | number | boolean | null | undefined | readonly (string | number | boolean)[]

export interface RequestOptions {
  query?: Record<string, QueryValue> | undefined
  body?: unknown
  idempotencyKey?: string | undefined
}

export interface HttpOptions {
  /** API 前缀所在的源，默认同源（反向代理把 /admin-api 接到控制面）。 */
  baseUrl?: string
  fetch?: typeof fetch
  /** CSRF Cookie 名，默认 isp_csrf。 */
  csrfCookie?: string
  /** 读取 Cookie；测试时注入。 */
  readCookie?: (name: string) => string | null
  /** 每个响应的服务端时间（响应头 Date），用于“几分钟前”这类展示。 */
  onServerTime?: (serverMs: number) => void
  /**
   * 非 GET 请求发出前若还没有 CSRF Cookie，先 GET 这个路径取得它（任何缺少 isp_csrf 的响应都会下发，docs/06 §8.2）。
   * 例如直接打开登录页时，登录请求之前没有任何 GET。不设则不补取。
   */
  csrfBootstrap?: string
}

export class NetworkError extends Error {
  readonly status: number | null
  constructor(message: string, status: number | null = null) {
    super(message)
    this.name = 'NetworkError'
    this.status = status
  }
}

const CODES: readonly ApiErrorCode[] = [
  'unauthorized',
  'unverified',
  'forbidden',
  'not_found',
  'conflict',
  'precondition',
  'invalid',
  'quota_exhausted',
  'rate_limited',
]

export function readDocumentCookie(name: string): string | null {
  if (typeof document === 'undefined') return null
  for (const part of document.cookie.split(';')) {
    const [k, ...v] = part.trim().split('=')
    if (k === name) return decodeURIComponent(v.join('='))
  }
  return null
}

/** 查询串：数组按 OpenAPI 的 style=form, explode=false 用逗号连接；空值省略。 */
export function toQueryString(query: Record<string, QueryValue> | undefined): string {
  if (!query) return ''
  const sp = new URLSearchParams()
  for (const [k, v] of Object.entries(query)) {
    if (v === undefined || v === null || v === '') continue
    if (Array.isArray(v)) {
      if (v.length === 0) continue
      sp.set(k, v.join(','))
    } else sp.set(k, String(v))
  }
  const s = sp.toString()
  return s ? `?${s}` : ''
}

export type Requester = <T>(method: string, path: string, opts?: RequestOptions) => Promise<T>

export function createHttp(opts: HttpOptions = {}): Requester {
  const base = opts.baseUrl ?? ''
  const doFetch = opts.fetch ?? ((...a: Parameters<typeof fetch>) => fetch(...a))
  const csrfName = opts.csrfCookie ?? 'isp_csrf'
  const readCookie = opts.readCookie ?? readDocumentCookie

  return async function request<T>(method: string, path: string, ro: RequestOptions = {}): Promise<T> {
    const headers: Record<string, string> = { Accept: 'application/json' }
    if (ro.body !== undefined) headers['Content-Type'] = 'application/json'
    if (method !== 'GET') {
      let token = readCookie(csrfName)
      if (!token && opts.csrfBootstrap) {
        try {
          await doFetch(base + opts.csrfBootstrap, { method: 'GET', headers: { Accept: 'application/json' }, credentials: 'include' })
        } catch {
          /* 取不到时照常发出，由服务端返回 403 forbidden */
        }
        token = readCookie(csrfName)
      }
      if (token) headers['X-CSRF-Token'] = token
    }
    if (ro.idempotencyKey) headers['Idempotency-Key'] = ro.idempotencyKey

    let res: Response
    try {
      res = await doFetch(base + path + toQueryString(ro.query), {
        method,
        headers,
        credentials: 'include',
        body: ro.body === undefined ? undefined : JSON.stringify(ro.body),
      })
    } catch {
      throw new NetworkError('网络连接失败，请检查网络后重试')
    }

    const date = res.headers.get('Date')
    if (date && opts.onServerTime) {
      const ms = Date.parse(date)
      if (!Number.isNaN(ms)) opts.onServerTime(ms)
    }

    if (res.status === 204) return undefined as T
    const text = await res.text()
    let data: unknown = undefined
    if (text) {
      try {
        data = JSON.parse(text)
      } catch {
        data = undefined
      }
    }
    if (res.ok) return data as T

    const err = data as { code?: string; message?: string; action?: { label: string; href: string } } | undefined
    if (err && typeof err.code === 'string' && (CODES as readonly string[]).includes(err.code)) {
      const { code, message, action, ...rest } = err as Record<string, unknown>
      const retry = Number(res.headers.get('Retry-After'))
      throw new ApiError(code as ApiErrorCode, typeof message === 'string' ? message : '请求没有完成', {
        action: (action as { label: string; href: string } | undefined) ?? null,
        status: res.status,
        retryAfter: Number.isFinite(retry) && retry > 0 ? retry : null,
        details: Object.keys(rest).length ? rest : null,
      })
    }
    throw new NetworkError(res.status >= 500 ? '服务暂时不可用，请稍后重试' : `请求失败（HTTP ${res.status}）`, res.status)
  }
}

/** 服务端时钟：记录响应头 Date 与本机时钟的偏移，只用于展示（“几分钟前”），不用于判断可用性。 */
export function createServerClock(initial?: number) {
  let offset = initial === undefined ? 0 : initial - Date.now()
  return {
    observe(serverMs: number) {
      offset = serverMs - Date.now()
    },
    now(): number {
      return Date.now() + offset
    },
  }
}
