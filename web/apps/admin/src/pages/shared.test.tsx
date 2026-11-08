// 共享出口套餐（D-24）：套餐出口类型、资源绑定上限与占用 x/N、受影响订阅、隔离时逐订阅告警、换出口候选按出口类型过滤。
import { screen, waitFor, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { newMock, renderApp, subIdByName } from '../test/renderApp'

const T = { timeout: 5000 }
const rowOf = async (text: string | RegExp) => (await screen.findByText(text, {}, T)).closest('tr')!

describe('共享出口', () => {
  it('套餐：列表标出独享 / 共享；新建时选择出口类型并提交', async () => {
    const api = newMock()
    const { user } = renderApp('/plans', api)
    expect(within(await rowOf('美国 · 洛杉矶 共享包月')).getByText('共享')).toBeTruthy()
    expect(within(await rowOf('日本 · 东京 包月')).getByText('独享')).toBeTruthy()

    await user.click(screen.getByRole('button', { name: /新建套餐/ }))
    const dlg = await screen.findByRole('dialog')
    await user.type(within(dlg).getByLabelText('套餐代码'), 'us-sea-shared')
    await user.type(within(dlg).getByLabelText('套餐名称'), '美国 · 西雅图 共享包月')
    await user.type(within(dlg).getByLabelText('城市'), '西雅图')
    await user.click(within(dlg).getByText('共享'))
    expect(within(dlg).getByText(/库存按剩余名额计/)).toBeTruthy()
    await user.click(within(dlg).getByRole('button', { name: '创建（草稿）' }))
    await waitFor(async () => {
      const p = (await api.adminListPlans()).find((x) => x.code === 'us-sea-shared')
      expect(p?.exitMode).toBe('shared')
    })
  })

  it('上游库存：占用 x/N，查看受影响订阅；隔离时为每个订阅各开一条告警', async () => {
    const api = newMock()
    const { user } = renderApp('/upstreams?q=gw-s1', api)
    const row = await rowOf(/gw-s1/)
    expect(within(row).getByText('2/3')).toBeTruthy()
    await user.click(within(row).getByRole('button', { name: '受影响订阅' }))
    const dlg = await screen.findByRole('dialog')
    const list = await within(dlg).findByRole('list', { name: '受影响订阅' })
    expect(within(list).getByRole('link', { name: '养号 · 共享 1' })).toBeTruthy()
    expect(within(list).getByRole('link', { name: '养号 · 共享 2' })).toBeTruthy()
    await user.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())

    await user.click(within(row).getByRole('button', { name: '状态' }))
    const st = await screen.findByRole('dialog')
    expect(within(st).getByText(/会为每个订阅各开一条告警/)).toBeTruthy()
    await user.type(within(st).getByLabelText('原因'), '供应商通知线路故障')
    await user.click(within(st).getByRole('button', { name: '确认修改' }))
    await waitFor(async () => {
      const res = (await api.adminListUpstreams({ query: { q: 'gw-s1' } })).items[0]!
      const alerts = (await api.adminListAlerts({ query: { status: ['open'] } })).items.filter((a) => a.resourceId === res.id)
      expect(alerts.map((a) => a.subscriptionId).length).toBe(2)
    })
  })

  it('编辑上游：有共享占用时绑定上限不能改为 1', async () => {
    const api = newMock()
    const { user } = renderApp('/upstreams?q=gw-s1', api)
    await user.click(await screen.findByRole('button', { name: /编辑 gw-s1/ }, T))
    const dlg = await screen.findByRole('dialog')
    expect(within(dlg).getByText(/资源上有共享占用 2\/3/)).toBeTruthy()
    const n = within(dlg).getByLabelText('绑定上限')
    await user.clear(n)
    await user.type(n, '1')
    await user.click(within(dlg).getByRole('button', { name: '保存' }))
    expect(await within(dlg).findByText(/绑定上限不能改为 1/)).toBeTruthy()
  })

  it('换出口：共享订阅只列有空位的共享资源；独享订阅不列有共享占用的资源', async () => {
    const api = newMock()
    const id = await subIdByName(api, '养号 · 共享 1')
    const { unmount } = renderApp(`/subscriptions/${id}/exit-change`, api)
    expect(await screen.findByLabelText(/选择 .*gw-s2/, {}, T)).toBeTruthy()
    expect(screen.queryByLabelText(/选择 .*gw-s1/)).toBeNull()
    expect(screen.queryByLabelText(/选择 .*gw-c1/)).toBeNull()
    expect(screen.getByText(/只列出绑定上限 ≥2、没有独享订阅且还有空位的资源/)).toBeTruthy()
    unmount()

    const tik = await subIdByName(api, 'TikTok 美区 1 号')
    renderApp(`/subscriptions/${tik}/exit-change`, api)
    expect(await screen.findByLabelText(/选择 .*gw-c1/, {}, T)).toBeTruthy()
    expect(screen.queryByLabelText(/选择 .*gw-s2/)).toBeNull()
  })
})
