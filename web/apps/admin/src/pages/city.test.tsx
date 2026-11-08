// 城市可选（D-27）：套餐与上游的城市都可以不填；列表显示“不限城市” / “—”。
import { screen, waitFor, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { newMock, renderApp } from '../test/renderApp'

const T = { timeout: 5000 }
const rowOf = async (text: string | RegExp) => (await screen.findByText(text, {}, T)).closest('tr')!

describe('城市可选', () => {
  it('套餐：列表显示“不限城市”；新建时城市留空即不限城市', async () => {
    const api = newMock()
    const { user } = renderApp('/plans', api)
    expect(within(await rowOf('美国 · 不限城市 包月')).getByText('美国 · 不限城市')).toBeTruthy()
    expect(within(await rowOf('美国 · 洛杉矶 包月')).getByText('美国 · 洛杉矶')).toBeTruthy()

    await user.click(screen.getByRole('button', { name: /新建套餐/ }))
    const dlg = await screen.findByRole('dialog')
    const city = within(dlg).getByLabelText('城市')
    expect(city.getAttribute('placeholder')).toBe('留空 = 不限城市')
    await user.type(within(dlg).getByLabelText('套餐代码'), 'us-flex')
    await user.type(within(dlg).getByLabelText('套餐名称'), 'Flex · 美国')
    await user.click(within(dlg).getByRole('button', { name: '创建（草稿）' }))
    await waitFor(async () => {
      const p = (await api.adminListPlans()).find((x) => x.code === 'us-flex')
      expect(p).toBeTruthy()
      expect(p!.current.city).toBeNull()
    })
  })

  it('套餐：发布新版本时清空城市，改为不限城市', async () => {
    const api = newMock()
    const { user } = renderApp('/plans', api)
    const row = await rowOf('日本 · 东京 包月')
    await user.click(within(row).getByRole('button', { name: /发布新版本/ }))
    const dlg = await screen.findByRole('dialog')
    await user.clear(within(dlg).getByLabelText('城市'))
    expect(within(dlg).getByText('地区：日本 东京 → 日本 不限城市')).toBeTruthy()
    await user.click(within(dlg).getByRole('button', { name: /发布 v2/ }))
    await waitFor(async () => {
      const p = (await api.adminListPlans()).find((x) => x.code === 'jp-tyo')
      expect(p!.current.version).toBe(2)
      expect(p!.current.city).toBeNull()
    })
  })

  it('上游：不填城市也能录入；列表里城市显示“—”', async () => {
    const api = newMock()
    const { user } = renderApp('/upstreams', api)
    await user.click(await screen.findByRole('button', { name: /录入上游/ }, T))
    const dlg = await screen.findByRole('dialog')
    await user.type(within(dlg).getByLabelText('上游地址'), 'socks5://u:p@gw-nocity.example.net:1080')
    const ok = within(dlg).getByRole('button', { name: '录入' })
    expect(ok.hasAttribute('disabled')).toBe(false)
    await user.click(ok)
    await waitFor(async () => {
      const u = (await api.adminListUpstreams({ query: { q: 'gw-nocity' } })).items[0]
      expect(u?.city).toBeNull()
    })
    const r = await rowOf(/gw-nocity/)
    // 地区列：国旗 + “—”。
    const region = [...r.querySelectorAll('td')].find((td) => td.querySelector('.flag'))!
    expect(region.textContent?.trim()).toBe('—')
  })

  it('上游：编辑时清空城市即清除', async () => {
    const api = newMock()
    const { user } = renderApp('/upstreams?q=gw-c1', api)
    await user.click(await screen.findByRole('button', { name: /编辑 gw-c1/ }, T))
    const dlg = await screen.findByRole('dialog')
    await user.clear(within(dlg).getByLabelText('城市'))
    await user.click(within(dlg).getByRole('button', { name: '保存' }))
    await waitFor(async () => {
      const u = (await api.adminListUpstreams({ query: { q: 'gw-c1' } })).items[0]!
      expect(u.city).toBeNull()
    })
  })
})
