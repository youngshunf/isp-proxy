// 国家：录入上游可以搜索全部国家（如菲律宾），列表里没有时手动输入代码与名称。
import { screen, waitFor, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { countryName, OTHER_COUNTRIES } from '../lib/countries'
import { newMock, renderApp } from '../test/renderApp'

const T = { timeout: 5000 }

async function openNew(user: ReturnType<typeof renderApp>['user'], url: string) {
  await user.click(await screen.findByRole('button', { name: /录入上游/ }, T))
  const dlg = await screen.findByRole('dialog')
  await user.type(within(dlg).getByLabelText('上游地址'), url)
  return dlg
}

describe('国家', () => {
  it('名称：常用列表、浏览器中文名与改写', () => {
    expect(countryName('ph')).toBe('菲律宾')
    expect(countryName('ae')).toBe('阿联酋（迪拜）')
    expect(countryName('mo')).toBe('中国澳门')
    expect(countryName('PE')).toBe('秘鲁')
    expect(OTHER_COUNTRIES.some((c) => c.value === 'us')).toBe(false)
  })

  it('录入上游：搜索“菲律”选中菲律宾', async () => {
    const api = newMock()
    const { user } = renderApp('/upstreams', api)
    const dlg = await openNew(user, 'socks5://u:p@gw-ph.example.net:1080')
    await user.click(within(dlg).getByLabelText('国家'))
    await user.keyboard('菲律')
    await waitFor(() => expect(document.querySelector('.ant-select-item-option[title="菲律宾"]')).toBeTruthy())
    await user.click(document.querySelector('.ant-select-item-option[title="菲律宾"]')!)
    await user.click(within(dlg).getByRole('button', { name: '录入' }))
    await waitFor(async () => {
      const u = (await api.adminListUpstreams({ query: { q: 'gw-ph' } })).items[0]
      expect(u).toMatchObject({ countryCode: 'ph', country: '菲律宾' })
    })
  })

  it('录入上游：列表里没有时手动输入代码和名称', async () => {
    const api = newMock()
    const { user } = renderApp('/upstreams', api)
    const dlg = await openNew(user, 'socks5://u:p@gw-xk.example.net:1080')
    await user.click(within(dlg).getByRole('button', { name: '列表里没有？手动输入' }))
    const ok = within(dlg).getByRole('button', { name: '录入' })
    await user.type(within(dlg).getByLabelText('国家代码'), 'X')
    expect(ok.hasAttribute('disabled')).toBe(true) // 代码不足两位
    await user.type(within(dlg).getByLabelText('国家代码'), 'K')
    // 浏览器认识的代码自动填中文名，可以再改。
    expect((within(dlg).getByLabelText('国家名称') as HTMLInputElement).value).toBe('科索沃')
    expect(ok.hasAttribute('disabled')).toBe(false)
    await user.click(ok)
    await waitFor(async () => {
      const u = (await api.adminListUpstreams({ query: { q: 'gw-xk' } })).items[0]
      expect(u).toMatchObject({ countryCode: 'xk', country: '科索沃' })
    })
  })
})
