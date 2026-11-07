// 会话：未登录访问后台回登录页并带回原路径；使用中会话过期（401）→ 清空缓存、提示过期、登录后回到原页面。
import { screen, waitFor } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { MOCK_ADMIN_LOGIN } from '@isp/api-client/admin/mock'
import { newMock, renderApp } from '../test/renderApp'

describe('会话', () => {
  it('未登录打开订阅页 → 登录页；两步登录后回到订阅页', async () => {
    const api = newMock({ signedIn: false })
    const { user, router } = renderApp('/subscriptions?blocker=pending', api)
    await waitFor(() => expect(router.state.location.pathname).toBe('/login'))
    expect(new URLSearchParams(router.state.location.search).get('from')).toBe('/subscriptions?blocker=pending')

    await user.type(await screen.findByLabelText('邮箱'), MOCK_ADMIN_LOGIN.email)
    await user.type(screen.getByLabelText('密码'), MOCK_ADMIN_LOGIN.password)
    await user.click(screen.getByRole('button', { name: '下一步' }))
    await user.type(await screen.findByLabelText('验证码'), MOCK_ADMIN_LOGIN.totp)
    await user.click(screen.getByRole('button', { name: '登录' }))
    await waitFor(() => expect(router.state.location.pathname + router.state.location.search).toBe('/subscriptions?blocker=pending'))
    expect(await screen.findByRole('heading', { name: '订阅' })).toBeTruthy()
  })

  it('使用中会话过期 → 回登录页并提示“会话已过期”，带回原路径', async () => {
    const api = newMock()
    const { user, router } = renderApp('/subscriptions', api)
    await screen.findByRole('heading', { name: '订阅' })
    await waitFor(() => expect(screen.getByRole('table')).toBeTruthy())

    api.mock.expireSession()
    await user.click(screen.getByRole('button', { name: '刷新' }))

    await waitFor(() => expect(router.state.location.pathname).toBe('/login'))
    const sp = new URLSearchParams(router.state.location.search)
    expect(sp.get('expired')).toBe('1')
    expect(sp.get('from')).toBe('/subscriptions')
    expect(await screen.findByText(/会话已过期/)).toBeTruthy()
  })

  it('首次登录：显示密钥与二维码，绑定后一次性展示恢复码', async () => {
    const api = newMock({ signedIn: false })
    const { user, router } = renderApp('/login', api)
    await user.type(await screen.findByLabelText('邮箱'), MOCK_ADMIN_LOGIN.enrollEmail)
    await user.type(screen.getByLabelText('密码'), MOCK_ADMIN_LOGIN.enrollPassword)
    await user.click(screen.getByRole('button', { name: '下一步' }))
    expect(await screen.findByRole('heading', { name: '绑定验证器' })).toBeTruthy()
    expect(screen.getByLabelText('验证器绑定二维码')).toBeTruthy()
    await user.type(screen.getByLabelText('验证码'), MOCK_ADMIN_LOGIN.totp)
    await user.click(screen.getByRole('button', { name: /绑定并登录/ }))
    const codes = await screen.findByRole('list', { name: '恢复码' })
    expect(codes.querySelectorAll('li').length).toBeGreaterThanOrEqual(8)
    const enter = screen.getByRole('button', { name: '进入后台' }) as HTMLButtonElement
    expect(enter.disabled).toBe(true)
    await user.click(screen.getByRole('checkbox', { name: '我已保存这些恢复码' }))
    await user.click(enter)
    await waitFor(() => expect(router.state.location.pathname).toBe('/'))
  })
})
