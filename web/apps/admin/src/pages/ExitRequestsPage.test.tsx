// 换出口申请：内部备注任何状态下都能改，客户不可见；保存后接口里是新备注。
import { screen, waitFor, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { newMock, renderApp } from '../test/renderApp'

describe('换出口申请', () => {
  it('修改内部备注', async () => {
    const api = newMock()
    const r = (await api.adminListExitRequests({ query: { limit: 50 } })).items[0]!
    const { user } = renderApp('/exit-requests', api)
    await user.click(await screen.findByRole('button', { name: `修改 ${r.no} 的内部备注` }))
    const dlg = await screen.findByRole('dialog')
    const box = within(dlg).getByLabelText('内部备注')
    await user.clear(box)
    await user.type(box, '客户电话确认过，优先处理')
    await user.click(within(dlg).getByRole('button', { name: '保存' }))
    await waitFor(async () => expect((await api.adminGetExitRequest({ exitRequestId: r.id })).internalNote).toBe('客户电话确认过，优先处理'))
  })
})
