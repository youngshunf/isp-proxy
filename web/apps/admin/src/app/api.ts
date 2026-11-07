// 管理后台的数据入口：页面通过 useApi() 取得 AdminApi，测试注入新的 mock 实例。
// 原型默认用内存 mock；设置 VITE_ADMIN_API=http 时改用真实客户端（同源 /admin-api，反向代理接控制面）。
import { createContext, useContext } from 'react'
import { ApiError, NetworkError, createAdminApi, type AdminApi } from '@isp/api-client/admin'
import { MOCK_NOW, createMockAdminApi } from '@isp/api-client/admin/mock'

export const ApiContext = createContext<AdminApi | null>(null)

export function useApi(): AdminApi {
  const api = useContext(ApiContext)
  if (!api) throw new Error('ApiContext 未提供')
  return api
}

/** 服务端时间：原型固定为 mock 的“当前时间”；接真实后端时由响应头 Date 估算偏移。只用于展示。 */
export const ServerNowContext = createContext<() => number>(() => Date.now())
export const useServerNow = () => useContext(ServerNowContext)

export const USE_MOCK = import.meta.env.VITE_ADMIN_API !== 'http'

const MOCK_SESSION_KEY = 'isp-admin:mock-session'

export function createDefaultApi(): { api: AdminApi; now: () => number } {
  if (USE_MOCK) {
    // 原型里用 sessionStorage 模拟 HttpOnly 会话 Cookie：刷新页面后仍是登录状态（数据会回到初始示意数据）。
    let role: string | null = null
    try {
      role = sessionStorage.getItem(MOCK_SESSION_KEY)
    } catch {
      /* 存储不可用时每次刷新都需重新登录 */
    }
    const mock = createMockAdminApi({ signedIn: role === 'viewer' ? 'viewer' : role === 'admin' })
    const verify = mock.adminVerifyMfa
    const logout = mock.adminLogout
    const remember = (v: string | null) => {
      try {
        if (v) sessionStorage.setItem(MOCK_SESSION_KEY, v)
        else sessionStorage.removeItem(MOCK_SESSION_KEY)
      } catch {
        /* 忽略 */
      }
    }
    mock.adminVerifyMfa = async (input) => {
      const s = await verify(input)
      remember(s.admin.role)
      return s
    }
    mock.adminLogout = async (input) => {
      remember(null)
      return logout(input)
    }
    return { api: mock, now: () => MOCK_NOW }
  }
  let offset = 0
  const api = createAdminApi({ onServerTime: (ms) => (offset = ms - Date.now()) })
  return { api, now: () => Date.now() + offset }
}

export function errorText(e: unknown): string {
  if (e instanceof ApiError) return e.message
  if (e instanceof NetworkError) return e.message
  return '请求没有完成，请检查网络后重试'
}

export function isUnauthorized(e: unknown): boolean {
  return e instanceof ApiError && e.code === 'unauthorized'
}

/** 查询键：都以 'admin' 开头，退出与会话失效时整体清空。 */
export const qk = {
  me: ['admin', 'me'] as const,
  overview: ['admin', 'overview'] as const,
  subs: ['admin', 'subs'] as const,
  sub: (id: string) => ['admin', 'subs', id] as const,
  subPart: (id: string, part: string) => ['admin', 'subs', id, part] as const,
  usage: (id: string, range: string) => ['admin', 'subs', id, 'usage', range] as const,
  upstreams: ['admin', 'upstreams'] as const,
  plans: ['admin', 'plans'] as const,
  packs: ['admin', 'packs'] as const,
  rules: ['admin', 'rules'] as const,
  users: ['admin', 'users'] as const,
  user: (id: string) => ['admin', 'users', id] as const,
  orders: ['admin', 'orders'] as const,
  order: (id: string) => ['admin', 'orders', id] as const,
  refunds: ['admin', 'refunds'] as const,
  exitRequests: ['admin', 'exit-requests'] as const,
  nodes: ['admin', 'nodes'] as const,
  alerts: ['admin', 'alerts'] as const,
  audit: ['admin', 'audit'] as const,
  task: (id: string) => ['admin', 'task', id] as const,
  probe: (id: string) => ['admin', 'probe', id] as const,
}

/** 每次用户意图一个幂等键（打开对话框时生成，重试复用）。 */
export function newIdempotencyKey(): string {
  if (typeof crypto !== 'undefined' && 'randomUUID' in crypto) return crypto.randomUUID()
  return `k-${Date.now()}-${Math.random().toString(36).slice(2)}`
}
