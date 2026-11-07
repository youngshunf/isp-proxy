// 人工核款：确认框写明订单号与金额；连点“确认收到”只发一次请求，结果只有一条收款记录。
import { screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { fmtMoney } from '@isp/ui'
import { newMock, renderApp } from '../test/renderApp'

const isDisabled = (el: HTMLElement) => (el as HTMLButtonElement).disabled

describe('确认收款', () => {
  it('对话框显示订单号与应收金额，双击确认只发一次请求', async () => {
    // 有延迟：第二次点击发生在第一次请求返回之前
    const api = newMock({ latency: [60, 60] })
    const order = (await api.adminListOrders({ query: { payment: ['awaiting'], limit: 1 } })).items[0]!
    const spy = vi.spyOn(api, 'adminConfirmPayment')
    const { user } = renderApp(`/orders/${order.id}`, api)

    await user.click(await screen.findByRole('button', { name: '确认收款' }))
    const dialog = await screen.findByRole('dialog')
    const head = within(dialog).getByLabelText('待确认的订单')
    expect(head.textContent).toContain(order.no)
    expect(head.textContent).toContain(fmtMoney(order.amount))

    const ok = within(dialog).getByRole('button', { name: `确认收到 ${fmtMoney(order.amount)}` })
    // 流水号未填时不能提交
    expect(isDisabled(ok)).toBe(true)
    await user.type(within(dialog).getByLabelText('外部收款参考'), 'TX-TEST-0001')
    expect(isDisabled(ok)).toBe(false)

    await user.dblClick(ok)
    // 成功提示出现即请求已返回（对话框的关闭动画在 jsdom 里不会结束，不以它为准）
    expect(await screen.findByText(`订单 ${order.no} 已确认收款`, {}, { timeout: 3000 })).toBeTruthy()
    expect(spy).toHaveBeenCalledTimes(1)
    expect(spy.mock.calls[0]![0]).toMatchObject({
      orderId: order.id,
      body: { method: 'manual_transfer', amount: order.amount, externalRef: 'TX-TEST-0001', lateArrival: false },
    })
    const after = await api.adminGetOrder({ orderId: order.id })
    expect(after.payment).toBe('confirmed')
    expect(after.paymentRecord?.externalRef).toBe('TX-TEST-0001')
  })

  it('到账金额与应收不符时不能提交', async () => {
    const api = newMock()
    const order = (await api.adminListOrders({ query: { payment: ['awaiting'], limit: 1 } })).items[0]!
    const { user } = renderApp(`/orders/${order.id}`, api)
    await user.click(await screen.findByRole('button', { name: '确认收款' }))
    const dialog = await screen.findByRole('dialog')
    await user.type(within(dialog).getByLabelText('外部收款参考'), 'TX-TEST-0002')
    const amount = within(dialog).getByLabelText('实际到账金额')
    await user.clear(amount)
    await user.type(amount, '1')
    expect(within(dialog).getByText(/不一致/)).toBeTruthy()
    expect(isDisabled(within(dialog).getByRole('button', { name: /确认收到/ }))).toBe(true)
  })
})
