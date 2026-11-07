// 测试用：用新的内存 mock 和内存路由渲染整个后台。
import { render } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MOCK_NOW, createMockAdminApi, type MockAdminApi } from '@isp/api-client/admin/mock'
import { createApp } from '../app/createApp'

export function newMock(opts: { signedIn?: boolean | 'viewer'; latency?: [number, number] } = {}): MockAdminApi {
  return createMockAdminApi({ latency: opts.latency ?? [0, 0], signedIn: opts.signedIn ?? true, jobSteps: 1 })
}

export function renderApp(path: string, api: MockAdminApi = newMock()) {
  const app = createApp({ api, now: () => MOCK_NOW, initialPath: path })
  const user = userEvent.setup()
  const view = render(<app.Root />)
  return { api, user, router: app.router, ...view }
}

/** 按名称找订阅 ID（seed 的 ID 是确定的，但用名称查更易读）。 */
export async function subIdByName(api: MockAdminApi, name: string): Promise<string> {
  const page = await api.adminListSubscriptions({ query: { q: name, limit: 5 } })
  const s = page.items.find((x) => x.name === name)
  if (!s) throw new Error(`seed 里没有订阅 ${name}`)
  return s.id
}
