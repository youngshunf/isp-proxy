// 封禁确认：先列出受影响的会话与订阅，原因必填；取消不发请求，确认后账号与订阅都被阻断。
import { screen, waitFor, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { newMock, renderApp } from '../test/renderApp'

const isDisabled = (el: HTMLElement) => (el as HTMLButtonElement).disabled

async function alice() {
  const api = newMock()
  const u = (await api.adminListUsers({ query: { q: 'alice@example.com' } })).items[0]!
  const detail = await api.adminGetUser({ userId: u.id })
  return { api, u, detail }
}

describe('封禁用户', () => {
  it('确认框列出影响，原因必填；确认后只发一次请求', async () => {
    const { api, u, detail } = await alice()
    const spy = vi.spyOn(api, 'adminBanUser')
    const { user } = renderApp(`/users/${u.id}`, api)

    await user.click(await screen.findByRole('button', { name: /封禁/ }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText('将会发生的变化')).toBeTruthy()
    // 每个在用订阅的名称都写进影响说明
    const live = detail.subscriptions.filter((s) => s.lifecycle !== 'deleted' && s.lifecycle !== 'released')
    expect(live.length).toBeGreaterThan(0)
    for (const s of live) expect(dialog.textContent).toContain(s.name)
    expect(dialog.textContent).toContain(`撤销 ${detail.activeSessions} 个登录会话`)

    const ok = within(dialog).getByRole('button', { name: '封禁' })
    expect(isDisabled(ok)).toBe(true)
    await user.type(within(dialog).getByLabelText('封禁原因'), '批量注册滥用')
    expect(isDisabled(ok)).toBe(false)
    await user.click(ok)

    // 成功提示与页头状态都会显示“已封禁”
    expect((await screen.findAllByText('已封禁', {}, { timeout: 3000 })).length).toBeGreaterThan(0)
    expect(spy).toHaveBeenCalledTimes(1)
    expect(spy.mock.calls[0]![0]).toEqual({ userId: u.id, body: { reason: '批量注册滥用' } })
    await waitFor(async () => expect((await api.adminGetUser({ userId: u.id })).status).toBe('banned'))
    const subs = await api.adminListSubscriptions({ query: { userId: u.id } })
    expect(subs.items.every((s) => s.blockers.includes('account_banned'))).toBe(true)
  })

  it('取消不会发请求', async () => {
    const { api, u } = await alice()
    const spy = vi.spyOn(api, 'adminBanUser')
    const { user } = renderApp(`/users/${u.id}`, api)
    await user.click(await screen.findByRole('button', { name: /封禁/ }))
    const dialog = await screen.findByRole('dialog')
    await user.type(within(dialog).getByLabelText('封禁原因'), '测试')
    await user.click(within(dialog).getByRole('button', { name: '取消' }))
    expect(spy).not.toHaveBeenCalled()
    expect((await api.adminGetUser({ userId: u.id })).status).toBe('active')
  })
})
