import { cleanup, render, screen, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import { afterEach, describe, expect, it } from 'vitest'
import { isDirect, type DirectSubscription } from '@isp/api-client'
import { createMockApi } from '@isp/api-client/mock'
import { DirectCard } from './DirectCard'

const all = await createMockApi({ latency: [0, 0] }).listSubscriptions()
const direct = all.find(isDirect)!

function renderCard(sub: DirectSubscription) {
  return render(
    <MemoryRouter>
      <DirectCard sub={sub} />
    </MemoryRouter>,
  )
}

afterEach(cleanup)

describe('DirectCard', () => {
  it('跟随付费 IP：显示直出标记、带宽上限、累计用量与可用截止，三种协议都能复制', () => {
    renderCard(direct)
    const card = screen.getByRole('article')
    expect(within(card).getByRole('img', { name: '服务器直出' })).toBeTruthy()
    // 不显示国旗：卡面上没有国旗图片（The Flag Identity Rule：国旗只代表出口国家）。
    expect(card.querySelector('.flag img')).toBeNull()
    expect(within(card).getByText(/10 Mbps/)).toBeTruthy()
    expect(within(card).getByText('累计用量')).toBeTruthy()
    expect(within(card).getByText(/不限流量/)).toBeTruthy()
    expect(within(card).getByText(/跟随付费 IP，至/)).toBeTruthy()
    expect(within(card).getByText('可用')).toBeTruthy()
    for (const p of ['HTTP', 'HTTPS', 'SOCKS5']) expect(within(card).getByRole('button', { name: `复制 ${p} 连接` })).toBeTruthy()
  })

  it('没有续时长、买流量、换出口入口', () => {
    renderCard(direct)
    for (const name of ['续时长', '买流量', /换出口/]) expect(screen.queryByRole('button', { name })).toBeNull()
  })

  it('没有付费 IP：主状态与原因可见，给出购买入口', () => {
    renderCard({
      ...direct,
      blockers: ['no_paid_subscription'],
      direct: { ...direct.direct, paidUntil: null },
      time: { ...direct.time, expiresAt: null, remainingSeconds: 0 },
    })
    expect(screen.getByText('没有付费 IP')).toBeTruthy()
    expect(screen.getByText('没有时长有效的付费 IP')).toBeTruthy()
    expect(screen.getByRole('button', { name: '购买 IP' })).toBeTruthy()
  })
})
