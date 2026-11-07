// 组装路由、查询缓存与会话失效处理。main.tsx 与测试共用，测试传入内存路由和新的 mock。
import { MutationCache, QueryCache, QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { App as AntApp, ConfigProvider } from 'antd'
import zhCN from 'antd/locale/zh_CN'
import { createBrowserRouter, createMemoryRouter } from 'react-router'
import { RouterProvider } from 'react-router/dom'
import { ApiError, type AdminApi } from '@isp/api-client/admin'
import { theme } from '@isp/ui'
import { ApiContext, ServerNowContext, isUnauthorized, qk } from './api'
import { routes } from './routes'

export interface AppOptions {
  api: AdminApi
  now: () => number
  /** 测试用内存路由的初始地址。 */
  initialPath?: string
}

export function createApp({ api, now, initialPath }: AppOptions) {
  const router = initialPath ? createMemoryRouter(routes, { initialEntries: [initialPath] }) : createBrowserRouter(routes)

  // 会话过期或被撤销：清空缓存，带着原路径回登录页。登录页本身的 401 不处理。
  let redirecting = false
  const onError = (e: unknown) => {
    if (!isUnauthorized(e) || redirecting) return
    const loc = router.state.location
    if (loc.pathname === '/login') return
    // 本标签页从未建立过会话（首次访问）：交给 Shell 的守卫普通跳转，不提示“会话已过期”。
    // 两条路径谁先触发取决于请求时序，这里必须按“是否有过会话”区分，而不是按先后。
    if (!queryClient.getQueryData(qk.me)) return
    redirecting = true
    queryClient.clear()
    const from = loc.pathname + loc.search
    void router.navigate(`/login?expired=1&from=${encodeURIComponent(from)}`, { replace: true }).finally(() => {
      redirecting = false
    })
  }

  const queryClient = new QueryClient({
    queryCache: new QueryCache({ onError }),
    mutationCache: new MutationCache({ onError }),
    defaultOptions: {
      queries: {
        staleTime: 10_000,
        // 读请求有限重试；契约错误（权限、不存在、会话失效）不重试。写请求不自动重放。
        retry: (count, e) => !(e instanceof ApiError) && count < 2,
        refetchOnWindowFocus: true,
      },
      mutations: { retry: false },
    },
  })

  function Root() {
    return (
      <ConfigProvider
        theme={{
          ...theme,
          components: {
            ...theme.components,
            Table: { headerBg: '#f5f8f7', headerColor: '#43524d', rowHoverBg: '#f5f8f7', cellPaddingBlockSM: 7, headerBorderRadius: 0 },
            Tabs: { horizontalMargin: '0 0 12px 0' },
          },
        }}
        locale={zhCN}
        // 两字按钮（登录、暂停、受理）不插空格：读屏与测试按原文匹配
        button={{ autoInsertSpace: false }}
      >
        <AntApp>
          <ApiContext.Provider value={api}>
            <ServerNowContext.Provider value={now}>
              <QueryClientProvider client={queryClient}>
                <RouterProvider router={router} />
              </QueryClientProvider>
            </ServerNowContext.Provider>
          </ApiContext.Provider>
        </AntApp>
      </ConfigProvider>
    )
  }

  return { Root, router, queryClient }
}
