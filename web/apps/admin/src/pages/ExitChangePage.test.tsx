// 换出口页面：被拒的候选写明原因且不能提交；通过的候选走完提交与查结果。
import { screen, waitFor } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { newMock, renderApp, subIdByName } from '../test/renderApp'

const isDisabled = (el: HTMLElement) => (el as HTMLButtonElement).disabled

describe('换出口流程', () => {
  it('候选 IP 已被其他订阅占用 → 被拒；换一个通过的候选 → 提交并查到结果', async () => {
    const api = newMock()
    const id = await subIdByName(api, 'TikTok 美区 1 号')
    const { user } = renderApp(`/subscriptions/${id}/exit-change`, api)

    // 1. 候选录入：gw-c4 的出口与“纽约 2 号”相同
    await user.click(await screen.findByLabelText(/选择 gw-c4/))
    await user.click(screen.getByRole('button', { name: '开始验证' }))
    // 2~3. 验证完成后进入比较，列出被拒原因
    expect(await screen.findByText(/已被平台内其他订阅使用/, {}, { timeout: 4000 })).toBeTruthy()
    expect(screen.getByText('候选被拒绝，不能提交')).toBeTruthy()
    expect(screen.queryByRole('button', { name: '下一步：提交' })).toBeNull()

    // 回到候选录入，选一个合格的
    await user.click(screen.getByRole('button', { name: /换一个候选/ }))
    await user.click(await screen.findByLabelText(/选择 gw-c1/))
    await user.click(screen.getByRole('button', { name: '开始验证' }))
    await user.click(await screen.findByRole('button', { name: '下一步：提交' }, { timeout: 4000 }))

    // 4. 提交：原因必填
    const submit = screen.getByRole('button', { name: '提交换出口' })
    expect(isDisabled(submit)).toBe(true)
    await user.type(screen.getByLabelText('换出口原因'), '目标站封禁当前出口')
    expect(isDisabled(submit)).toBe(false)
    await user.click(submit)

    // 5. 查结果
    expect(await screen.findByText(/换出口完成/, {}, { timeout: 5000 })).toBeTruthy()
    await waitFor(async () => {
      const s = await api.adminGetSubscription({ subscriptionId: id })
      expect(s.exit?.verifiedIp).toBe('203.0.113.77')
    })
  })

  it('候选与当前出口相同（上游更新了但出口没变）→ 被拒', async () => {
    const api = newMock()
    const id = await subIdByName(api, 'TikTok 美区 1 号')
    const { user } = renderApp(`/subscriptions/${id}/exit-change`, api)
    await user.click(await screen.findByLabelText(/选择 gw-c3/))
    await user.click(screen.getByRole('button', { name: '开始验证' }))
    expect(await screen.findByText(/与当前出口相同/, {}, { timeout: 4000 })).toBeTruthy()
    expect(screen.queryByRole('button', { name: '下一步：提交' })).toBeNull()
  })
})
