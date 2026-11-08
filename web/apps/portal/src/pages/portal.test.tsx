// 前台转正的组件测试（docs/05 §6 第 7 步）：双阻断同时可见、换接入确认、重复提交、失败反馈、会话失效，
// 以及注册验证即登录、找回密码设新密码、数量固定为 1、换出口申请冲突展示现有申请。
import { screen, waitFor, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { ApiError } from '@isp/api-client'
import { createMockApi } from '@isp/api-client/mock'
import { newMock, renderApp } from '../test/renderApp'

describe('IP 详情', () => {
  it('两个阻断原因同时可见，各自给出对应的补救入口', async () => {
    const api = newMock()
    const base = await api.getSubscription('sub_s4wp')
    api.getSubscription = async () => ({ ...base, blockers: ['expired', 'traffic_exhausted'] })
    renderApp('/ips/sub_s4wp', api)
    expect(await screen.findByText(/原出口与接入保留到/)).toBeTruthy()
    expect(screen.getByText(/流量已用尽，全部连接已断开/)).toBeTruthy()
    expect(screen.getAllByRole('button', { name: '续时长' }).length).toBeGreaterThan(0)
    expect(screen.getAllByRole('button', { name: '买流量' }).length).toBeGreaterThan(0)
  })

  it('换接入：确认框写明影响，确认后高亮新密码并要求确认已更新程序', async () => {
    const { user, api } = renderApp('/ips/sub_7k2m')
    await user.click(await screen.findByRole('button', { name: /重置密码/ }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText('旧连接会立即失效，正在使用的程序需要更新配置。')).toBeTruthy()
    const before = (await api.getSubscription('sub_7k2m')).access!
    await user.click(within(dialog).getByRole('button', { name: '确认重置密码' }))
    expect(await screen.findByText(/接入已更新到 v3：/)).toBeTruthy()
    const after = (await api.getSubscription('sub_7k2m')).access!
    expect(after.password).not.toBe(before.password)
    expect(after.ports).toEqual(before.ports)
    await user.click(screen.getByRole('button', { name: '我已更新程序' }))
    await waitFor(() => expect(screen.queryByText(/接入已更新到 v3：/)).toBeNull())
  })

  it('换接入重复点击确认只换一次（按钮提交中禁用 + 同一幂等键）', async () => {
    const { user, api } = renderApp('/ips/sub_7k2m')
    const left = (await api.getSubscription('sub_7k2m')).quotas.accessChangesLeft
    await user.click(await screen.findByRole('button', { name: /更换接入/ }))
    const ok = within(await screen.findByRole('dialog')).getByRole('button', { name: '确认更换端口与密码' })
    await user.dblClick(ok)
    expect(await screen.findByText(/接入已更新到 v3：/)).toBeTruthy()
    expect((await api.getSubscription('sub_7k2m')).quotas.accessChangesLeft).toBe(left - 1)
    expect((await api.getSubscription('sub_7k2m')).access!.version).toBe(3)
  })

  it('换出口申请冲突：展示服务端带回的现有申请', async () => {
    const api = newMock()
    const base = await api.getSubscription('sub_n2ry')
    // 前台缓存认为没有申请（例如另一台设备刚提交），服务端以 409 带回现有申请。
    api.getSubscription = async () => ({ ...base, openExitRequestId: null })
    const { user } = renderApp('/ips/sub_n2ry', api)
    await user.click(await screen.findByRole('button', { name: /申请换出口/ }))
    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByLabelText('速度或稳定性不佳'))
    await user.click(within(dialog).getByRole('button', { name: '提交申请' }))
    expect(await within(dialog).findByText(/REQ-1105-0007/)).toBeTruthy()
  })
})

describe('购买', () => {
  it('没有数量步进器；下单数量固定为 1，重复点击只生成一个订单', async () => {
    // 带一点延迟：第二次点击发生在第一次请求返回之前（真实网络下的双击）。
    const { user, api } = renderApp('/buy', createMockApi({ latency: [40, 40] }))
    await screen.findByText('选择地区')
    expect(screen.queryByRole('group', { name: '数量' })).toBeNull()
    expect(screen.queryByRole('button', { name: '增加' })).toBeNull()
    await user.click(screen.getByRole('radio', { name: '美国 洛杉矶' }))
    const before = (await api.listOrders()).length
    await user.dblClick(screen.getByRole('button', { name: /提交订单/ }))
    expect(await screen.findByText('付款方式')).toBeTruthy()
    const orders = await api.listOrders()
    expect(orders.length).toBe(before + 1)
    expect(orders[0]!.lines[0]!.label).toBe('美国 · 洛杉矶 包月')
  })

  it('下单失败：展示服务端给出的说明，不跳转', async () => {
    const { user, api } = renderApp('/buy')
    await screen.findByText('选择地区')
    api.failNext('createOrder', new ApiError('precondition', '平台尚未配置收款说明，暂时不能下单'))
    await user.click(screen.getByRole('radio', { name: '美国 洛杉矶' }))
    await user.click(screen.getByRole('button', { name: /提交订单/ }))
    expect(await screen.findByText('平台尚未配置收款说明，暂时不能下单')).toBeTruthy()
    expect(screen.getByText('选择地区')).toBeTruthy()
  })
})

describe('会话', () => {
  it('会话失效：清空缓存并带着原路径去登录，登录后回到原页面', async () => {
    const { user, api, router, queryClient } = renderApp('/ips')
    await screen.findByText('TikTok 美区 1 号')
    api.expireSession()
    // 申请列表还没有缓存：进入页面即请求，服务端返回 401。
    await router.navigate('/requests')
    await waitFor(() => expect(router.state.location.pathname).toBe('/login'))
    expect(new URLSearchParams(router.state.location.search).get('next')).toBe('/requests')
    // 失败的申请页与登录页都写着“登录已失效”：先等登录表单（懒加载）渲染出来，再确认提示在登录页上
    const email = await screen.findByLabelText('邮箱')
    expect(screen.getByText('登录已失效，请重新登录')).toBeTruthy()
    expect(queryClient.getQueryData(['u', 'demo@example.com', 'subs'])).toBeUndefined()
    await user.type(email, 'demo@example.com')
    await user.type(screen.getByLabelText('密码'), 'password-123')
    await user.click(screen.getByRole('button', { name: '登录' }))
    await waitFor(() => expect(router.state.location.pathname).toBe('/requests'))
  })

  it('注册不登录：带邮箱去验证页，验证成功即登录', async () => {
    const api = newMock({ signedIn: false })
    const { user, router } = renderApp('/register', api)
    await user.type(await screen.findByLabelText('邮箱'), 'new@example.com')
    await user.type(screen.getByLabelText('登录密码'), 'long-password-1')
    await user.click(screen.getByRole('button', { name: '注册并发送验证码' }))
    await waitFor(() => expect(router.state.location.pathname).toBe('/verify-email'))
    expect(await api.session()).toBeNull()
    expect(((await screen.findByLabelText('注册邮箱')) as HTMLInputElement).value).toBe('new@example.com')
    await user.type(screen.getByLabelText('验证码'), '123456')
    await user.click(screen.getByRole('button', { name: '验证并登录' }))
    await waitFor(() => expect(router.state.location.pathname).toBe('/ips'))
    expect(await api.session()).toMatchObject({ email: 'new@example.com', verified: true })
  })

  it('找回密码：发送验证码后用它设置新密码，然后去登录', async () => {
    const api = newMock({ signedIn: false })
    const { user, router } = renderApp('/forgot-password', api)
    await user.type(await screen.findByLabelText('邮箱'), 'demo@example.com')
    await user.click(screen.getByRole('button', { name: '发送验证码' }))
    await waitFor(() => expect(router.state.location.pathname).toBe('/reset-password'))
    await user.type(await screen.findByLabelText('验证码'), '123456')
    expect((screen.getByLabelText('邮箱') as HTMLInputElement).value).toBe('demo@example.com')
    await user.type(screen.getByLabelText('新密码'), 'brand-new-pass-1')
    await user.type(screen.getByLabelText('再次输入新密码'), 'brand-new-pass-1')
    await user.click(screen.getByRole('button', { name: '设置新密码' }))
    expect(await screen.findByText('请用新密码登录')).toBeTruthy()
    expect(api.calls.confirmPasswordReset).toBe(1)
  })
})
