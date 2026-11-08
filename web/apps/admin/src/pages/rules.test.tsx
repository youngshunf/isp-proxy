// 规则版本：默认立即生效；定时生效的版本显示“待生效”，生效前可以撤回。
import { screen, waitFor, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { newMock, renderApp } from '../test/renderApp'

const T = { timeout: 5000 }

describe('规则版本', () => {
  it('发布默认立即生效，马上成为当前版本', async () => {
    const api = newMock()
    const { user } = renderApp('/rules', api)
    await user.click(await screen.findByRole('button', { name: /发布新版本/ }, T))
    const dlg = await screen.findByRole('dialog')
    expect(within(dlg).getByText(/发布后立即生效/)).toBeTruthy()
    const version = (within(dlg).getByLabelText('版本号') as HTMLInputElement).value
    await user.click(within(dlg).getByRole('button', { name: '发布' }))
    expect(await screen.findByText(`当前生效：${version}`, {}, T)).toBeTruthy()
    expect(screen.queryByText(/待生效/)).toBeNull()
  })

  it('定时生效的版本显示待生效，可以撤回', async () => {
    const api = newMock()
    const { user } = renderApp('/rules', api)
    const before = await screen.findByText(/^当前生效：/, {}, T)
    const was = before.textContent
    await user.click(screen.getByRole('button', { name: /发布新版本/ }))
    const dlg = await screen.findByRole('dialog')
    const version = (within(dlg).getByLabelText('版本号') as HTMLInputElement).value
    await user.click(within(dlg).getByLabelText('定时生效'))
    expect(within(dlg).getByLabelText('生效时间')).toBeTruthy()
    await user.click(within(dlg).getByRole('button', { name: '发布' }))

    const row = (await screen.findByText(version, {}, T)).closest('tr')!
    expect(within(row).getByText(/待生效 · /)).toBeTruthy()
    expect(screen.getByText(was!)).toBeTruthy() // 当前生效版本不变
    await user.click(within(row).getByRole('button', { name: '撤回' }))
    const confirm = await screen.findByRole('dialog')
    await user.click(within(confirm).getByRole('button', { name: '撤回' }))
    await waitFor(async () => {
      expect((await api.adminListRules()).some((r) => r.version === version)).toBe(false)
    })
  })
})
