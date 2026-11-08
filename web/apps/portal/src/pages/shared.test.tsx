// 共享出口套餐（D-24）：购买页区分独享 / 共享；共享订阅的卡片与详情显示“共享 · 最多 N 人共用”，不显示同出口的其他订阅与人数。
import { screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { renderApp } from '../test/renderApp'

describe('共享出口', () => {
  it('购买页：同一地区的独享与共享套餐分开列出，可下共享单', async () => {
    const { user, api } = renderApp('/buy')
    await screen.findByText('选择地区')
    const shared = screen.getByRole('radio', { name: '美国 洛杉矶 共享' })
    const exclusive = screen.getByRole('radio', { name: '美国 洛杉矶' })
    expect(within(shared.closest('tr')!).getByText('共享')).toBeTruthy()
    expect(within(exclusive.closest('tr')!).getByText('独享')).toBeTruthy()
    await user.click(shared)
    expect(screen.getByText(/共享出口：与其他订阅共用一个出口 IP/)).toBeTruthy()
    await user.click(screen.getByRole('button', { name: /提交订单/ }))
    expect(await screen.findByText('付款方式')).toBeTruthy()
    const [o] = await api.listOrders()
    expect(o!.lines[0]!.label).toBe('美国 · 洛杉矶 共享包月')
  })

  it('我的 IP：卡片标出独享 / 共享', async () => {
    renderApp('/ips')
    const card = (await screen.findByRole('link', { name: '账号养号 · 共享' })).closest('article')!
    expect(within(card).getByText(/· 共享 ·/)).toBeTruthy()
    const other = screen.getByRole('link', { name: 'TikTok 美区 1 号' }).closest('article')!
    expect(within(other).getByText(/· 独享 ·/)).toBeTruthy()
  })

  it('IP 详情：共享订阅显示“最多 N 人共用”，独享订阅显示独享', async () => {
    const { unmount } = renderApp('/ips/sub_h8sc')
    expect(await screen.findByText('共享 · 最多 3 人')).toBeTruthy()
    expect(screen.getByText(/共享 · 最多 3 人共用/)).toBeTruthy()
    expect(screen.getByText(/你的接入、用量与余量单独计算/)).toBeTruthy()
    unmount()
    renderApp('/ips/sub_7k2m')
    expect(await screen.findByText('这个出口 IP 只分配给你一个订阅。')).toBeTruthy()
  })
})
