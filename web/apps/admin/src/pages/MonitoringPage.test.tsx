// 节点登记与修改：登记时填名称、公共地址与端口池；修改只改名称、公共地址与状态（端口池不可改）。
import { screen, waitFor, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { newMock, renderApp } from '../test/renderApp'

describe('节点', () => {
  it('登记节点：端口池校验，登记后出现在列表里', async () => {
    const api = newMock()
    const { user } = renderApp('/monitoring', api)
    await user.click(await screen.findByRole('button', { name: /登记节点/ }))
    const dlg = await screen.findByRole('dialog')
    await user.type(within(dlg).getByLabelText('节点名称'), 'ams-09')
    await user.type(within(dlg).getByLabelText('公共地址'), 'ams-09.example.net')
    const start = within(dlg).getByLabelText('端口池起')
    await user.clear(start)
    await user.type(start, '40000')
    const end = within(dlg).getByLabelText('端口池止')
    await user.clear(end)
    await user.type(end, '30000')
    expect(within(dlg).getByText(/起始不大于结束/)).toBeTruthy()
    expect((within(dlg).getByRole('button', { name: '登记' }) as HTMLButtonElement).disabled).toBe(true)
    await user.clear(end)
    await user.type(end, '40999')
    await user.click(within(dlg).getByRole('button', { name: '登记' }))
    expect(await screen.findByRole('article', { name: '节点 ams-09' })).toBeTruthy()
    const n = (await api.adminListNodes()).find((x) => x.name === 'ams-09')!
    expect(n).toMatchObject({ publicHost: 'ams-09.example.net', portRange: { start: 40000, end: 40999 }, status: 'active' })
  })

  it('修改节点：改公共地址并设为排空，只提交改动的字段', async () => {
    const api = newMock()
    const before = (await api.adminListNodes())[0]!
    const { user } = renderApp('/monitoring', api)
    await user.click(await screen.findByRole('button', { name: `修改节点 ${before.name}` }))
    const dlg = await screen.findByRole('dialog')
    expect(within(dlg).queryByLabelText('端口池起')).toBeNull()
    const host = within(dlg).getByLabelText('公共地址')
    await user.clear(host)
    await user.type(host, 'new-host.example.net')
    await user.click(within(dlg).getByText('排空'))
    await user.click(within(dlg).getByRole('button', { name: '保存' }))
    await waitFor(async () => {
      const after = (await api.adminListNodes()).find((x) => x.id === before.id)!
      expect(after).toMatchObject({ name: before.name, publicHost: 'new-host.example.net', status: 'draining' })
    })
  })

  it('修改节点的直出出口地域：校验代码，保存后节点卡片显示；清空两项即清除', async () => {
    const api = newMock()
    const target = (await api.adminListNodes()).find((n) => !n.directRegion)!
    const { user } = renderApp('/monitoring', api)
    await user.click(await screen.findByRole('button', { name: `修改节点 ${target.name}` }))
    let dlg = await screen.findByRole('dialog')
    await user.type(within(dlg).getByLabelText('直出出口地域'), '新加坡')
    await user.type(within(dlg).getByLabelText('地域代码'), 's1')
    expect(within(dlg).getByText(/地域代码须为两位字母/)).toBeTruthy()
    expect((within(dlg).getByRole('button', { name: /保\s*存/ }) as HTMLButtonElement).disabled).toBe(true)
    await user.clear(within(dlg).getByLabelText('地域代码'))
    await user.type(within(dlg).getByLabelText('地域代码'), 'SG')
    await user.click(within(dlg).getByRole('button', { name: /保\s*存/ }))
    const card = await screen.findByRole('article', { name: `节点 ${target.name}` })
    await waitFor(() => expect(within(card).getByText('新加坡（SG）')).toBeTruthy())
    expect((await api.adminListNodes()).find((n) => n.id === target.id)!.directRegion).toEqual({ countryCode: 'sg', name: '新加坡' })

    await user.click(screen.getByRole('button', { name: `修改节点 ${target.name}` }))
    dlg = await screen.findByRole('dialog')
    await user.clear(within(dlg).getByLabelText('直出出口地域'))
    await user.clear(within(dlg).getByLabelText('地域代码'))
    await user.click(within(dlg).getByRole('button', { name: /保\s*存/ }))
    await waitFor(async () => expect((await api.adminListNodes()).find((n) => n.id === target.id)!.directRegion).toBeNull())
  })
})
