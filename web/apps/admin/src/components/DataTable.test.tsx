// 通用表格（F-W2）：筛选写进地址栏并传给接口；清除筛选；列配置可隐藏列并记住。
import { screen, waitFor, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { newMock, renderApp } from '../test/renderApp'

const table = () => screen.getByRole('table')
const rowNames = () =>
  within(table())
    .queryAllByRole('link')
    .map((a) => a.textContent)

describe('DataTable', () => {
  it('地址栏里的筛选直接生效，并按契约传给接口', async () => {
    const api = newMock()
    const spy = vi.spyOn(api, 'adminListSubscriptions')
    const { user, router } = renderApp('/subscriptions?blocker=traffic_exhausted', api)
    await waitFor(() => expect(rowNames()).toContain('美区备用'))
    expect(spy.mock.calls.at(-1)![0]!.query).toMatchObject({ blocker: 'traffic_exhausted', limit: 20 })
    expect(rowNames()).not.toContain('纽约 2 号')

    // 清除筛选：地址栏去掉参数，列表回到全部
    await user.click(screen.getByRole('button', { name: /清除筛选/ }))
    await waitFor(() => expect(router.state.location.search).toBe(''))
    await waitFor(() => expect(rowNames()).toContain('纽约 2 号'))
    expect(spy.mock.calls.at(-1)![0]!.query!.blocker).toBeUndefined()
  })

  it('搜索回车后写进地址栏；没有结果时说明是筛选导致', async () => {
    const api = newMock()
    const { user, router } = renderApp('/subscriptions', api)
    await waitFor(() => expect(rowNames().length).toBeGreaterThan(0))
    const search = screen.getByRole('searchbox', { name: '搜索' })
    await user.type(search, '纽约 2 号{Enter}')
    await waitFor(() => expect(new URLSearchParams(router.state.location.search).get('q')).toBe('纽约 2 号'))
    await waitFor(() => expect(rowNames().filter((x) => x && !x.startsWith('sub_'))).toEqual(['纽约 2 号']))

    await user.clear(search)
    await user.type(search, '不存在的订阅{Enter}')
    expect(await screen.findByText('没有符合筛选条件的记录')).toBeTruthy()
  })

  it('列配置：隐藏一列后表头消失，并记在本机', async () => {
    const { user } = renderApp('/subscriptions')
    await waitFor(() => expect(within(table()).getByRole('columnheader', { name: '剩余流量' })).toBeTruthy())
    await user.click(screen.getByRole('button', { name: '列设置' }))
    await user.click(await screen.findByRole('checkbox', { name: '剩余流量' }))
    await waitFor(() => expect(within(table()).queryByRole('columnheader', { name: '剩余流量' })).toBeNull())
    expect(JSON.parse(localStorage.getItem('isp-admin:cols:subscriptions')!)).toContain('traffic')
    // 主键列不能隐藏
    expect((screen.getByRole('checkbox', { name: '订阅' }) as HTMLInputElement).disabled).toBe(true)
  })
})
