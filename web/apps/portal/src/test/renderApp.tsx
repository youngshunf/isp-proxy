// 测试用：用新的内存 mock 和内存路由渲染整个前台（与 main.tsx 同一个 createApp）。
import { render } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MOCK_NOW, createMockApi, type MockPortalApi } from '@isp/api-client/mock'
import { createApp } from '../app/createApp'

export function newMock(opts: { signedIn?: boolean } = {}): MockPortalApi {
  return createMockApi({ latency: [0, 0], signedIn: opts.signedIn ?? true })
}

export function renderApp(path: string, api: MockPortalApi = newMock()) {
  const app = createApp({ api, now: () => MOCK_NOW, initialPath: path })
  const user = userEvent.setup()
  const view = render(<app.Root />)
  return { api, user, router: app.router, queryClient: app.queryClient, ...view }
}
