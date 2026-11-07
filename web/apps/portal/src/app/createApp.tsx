// 组装路由、查询缓存与全局会话处理。main.tsx 与组件测试共用，测试传入内存路由和新的 mock。
import { MutationCache, QueryCache, QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { App as AntApp, ConfigProvider } from 'antd'
import zhCN from 'antd/locale/zh_CN'
import { createBrowserRouter, createMemoryRouter } from 'react-router'
import { RouterProvider } from 'react-router/dom'
import { ApiError, type PortalApi } from '@isp/api-client'
import { theme } from '@isp/ui'
import { configureApi, setQueryUser } from './api'
import { routes } from './router'

export interface AppOptions {
  /** 不传则用 api.ts 的默认数据源（真实客户端或 VITE_PORTAL_API=mock）。 */
  api?: PortalApi
  now?: () => number
  /** 测试用内存路由的初始地址。 */
  initialPath?: string
}

/** 不需要登录的页面：这些页面上的 401/403 由页面自己展示，不做全局跳转。 */
export const PUBLIC_PATHS = ['/login', '/register', '/verify-email', '/forgot-password', '/reset-password']

export function createApp({ api, now, initialPath }: AppOptions = {}) {
  if (api) configureApi(api, now ?? (() => Date.now()))
  const router = initialPath ? createMemoryRouter(routes, { initialEntries: [initialPath] }) : createBrowserRouter(routes)

  // docs/05 §6 第 3 步：会话失效（401）清空缓存并带着原路径去登录；邮箱未验证（403 unverified）去验证页。
  let redirecting = false
  const onError = (e: unknown) => {
    if (!(e instanceof ApiError) || redirecting) return
    if (e.code !== 'unauthorized' && e.code !== 'unverified') return
    const loc = router.state.location
    if (PUBLIC_PATHS.includes(loc.pathname)) return
    redirecting = true
    let to = '/verify-email'
    if (e.code === 'unauthorized') {
      queryClient.clear()
      setQueryUser(null)
      to = `/login?next=${encodeURIComponent(loc.pathname + loc.search)}&expired=1`
    }
    void router.navigate(to, { replace: true }).finally(() => {
      redirecting = false
    })
  }

  const queryClient = new QueryClient({
    queryCache: new QueryCache({ onError }),
    mutationCache: new MutationCache({ onError }),
    defaultOptions: {
      queries: {
        staleTime: 15_000,
        // 读请求有限重试；契约错误（会话失效、不存在、前置条件）不重试。写请求不自动重放。
        retry: (count, e) => !(e instanceof ApiError) && count < 2,
        refetchOnWindowFocus: true,
      },
      mutations: { retry: false },
    },
  })

  function Root() {
    return (
      // 两字按钮（登录、验证）不插空格：读屏与测试按原文匹配
      <ConfigProvider theme={theme} locale={zhCN} button={{ autoInsertSpace: false }}>
        <AntApp>
          <QueryClientProvider client={queryClient}>
            <RouterProvider router={router} />
          </QueryClientProvider>
        </AntApp>
      </ConfigProvider>
    )
  }

  return { Root, router, queryClient }
}
