// 城市可选（D-27）：购买页显示套餐名称；不限城市的套餐可选城市，选了下单带 city，不选不带。
import { screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import type { OrderInput } from '@isp/api-client'
import { newMock, renderApp } from '../test/renderApp'

/** 记下 createOrder 收到的输入。 */
function spyOrders() {
  const api = newMock()
  const inputs: OrderInput[] = []
  const orig = api.createOrder.bind(api)
  api.createOrder = (input, key) => {
    inputs.push(input)
    return orig(input, key)
  }
  return { api, inputs }
}

describe('购买页：套餐名称与城市', () => {
  it('每行醒目显示套餐名称，城市为空的显示“不限城市”', async () => {
    renderApp('/buy')
    await screen.findByText('选择套餐')
    const any = screen.getByRole('radio', { name: '美国 不限城市' })
    const row = within(any.closest('tr')!)
    expect(row.getByText('Flex · 美国不限城市')).toBeTruthy()
    expect(row.getByText(/美国 · 不限城市/)).toBeTruthy()
    expect(row.getByText('独享')).toBeTruthy()
    const lax = within(screen.getByRole('radio', { name: '美国 洛杉矶' }).closest('tr')!)
    expect(lax.getByText('美国 · 洛杉矶 包月')).toBeTruthy()
  })

  it('固定城市的套餐不出现城市选择', async () => {
    const { user } = renderApp('/buy')
    await screen.findByText('选择套餐')
    await user.click(screen.getByRole('radio', { name: '美国 洛杉矶' }))
    expect(screen.queryByRole('group', { name: '城市' })).toBeNull()
  })

  it('不限城市套餐：默认“不限城市”，下单不带 city', async () => {
    const { api, inputs } = spyOrders()
    const { user } = renderApp('/buy', api)
    await screen.findByText('选择套餐')
    await user.click(screen.getByRole('radio', { name: '美国 不限城市' }))
    const group = within(screen.getByRole('group', { name: '城市' }))
    expect(group.getByRole('button', { name: /不限城市/ }).getAttribute('aria-pressed')).toBe('true')
    expect(group.getByRole('button', { name: /洛杉矶/ })).toBeTruthy()
    expect(group.getByRole('button', { name: /纽约.*库存紧张/ })).toBeTruthy()
    await user.click(screen.getByRole('button', { name: /提交订单/ }))
    expect(await screen.findByText('付款方式')).toBeTruthy()
    expect(inputs).toHaveLength(1)
    expect(inputs[0]).not.toHaveProperty('city')
  })

  it('不限城市套餐：选了城市，下单带上该城市，订单明细写明城市', async () => {
    const { api, inputs } = spyOrders()
    const { user } = renderApp('/buy', api)
    await screen.findByText('选择套餐')
    await user.click(screen.getByRole('radio', { name: '美国 不限城市' }))
    const group = within(screen.getByRole('group', { name: '城市' }))
    await user.click(group.getByRole('button', { name: /纽约/ }))
    expect(group.getByRole('button', { name: /纽约/ }).getAttribute('aria-pressed')).toBe('true')
    await user.click(screen.getByRole('button', { name: /提交订单/ }))
    expect(await screen.findByText('付款方式')).toBeTruthy()
    expect(inputs[0]).toMatchObject({ kind: 'new', planId: 'us-any', city: '纽约' })
    const [o] = await api.listOrders()
    expect(o!.lines[0]!.label).toBe('Flex · 美国不限城市')
    expect(o!.lines[0]!.detail).toMatch(/，城市：纽约$/)
  })

  it('换套餐时清掉已选城市', async () => {
    const { api, inputs } = spyOrders()
    const { user } = renderApp('/buy', api)
    await screen.findByText('选择套餐')
    await user.click(screen.getByRole('radio', { name: '美国 不限城市' }))
    await user.click(within(screen.getByRole('group', { name: '城市' })).getByRole('button', { name: /纽约/ }))
    await user.click(screen.getByRole('radio', { name: '美国 洛杉矶' }))
    await user.click(screen.getByRole('button', { name: /提交订单/ }))
    expect(await screen.findByText('付款方式')).toBeTruthy()
    expect(inputs[0]).toMatchObject({ planId: 'us-lax' })
    expect(inputs[0]).not.toHaveProperty('city')
  })
})

describe('mock：城市规则', () => {
  it('固定城市的套餐选了其他城市返回 422', async () => {
    const api = newMock()
    await expect(api.createOrder({ kind: 'new', planId: 'us-lax', months: 1, quantity: 1, city: '纽约' }, 'k1')).rejects.toMatchObject({ code: 'invalid' })
    await expect(api.createOrder({ kind: 'new', planId: 'us-lax', months: 1, quantity: 1, city: '洛杉矶' }, 'k2')).resolves.toBeTruthy()
  })
})
