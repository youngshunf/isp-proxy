import { cleanup, render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import { afterEach, describe, expect, it } from 'vitest'
import { createMockApi } from '@isp/api-client/mock'
import { SimCard } from './SimCard'

const subs = await createMockApi({ latency: [0, 0] }).listSubscriptions()
const byId = (id: string) => subs.find((s) => s.id === id)!

function renderCard(id: string) {
  return render(
    <MemoryRouter>
      <SimCard sub={byId(id)} />
    </MemoryRouter>,
  )
}

afterEach(cleanup)

describe('SimCard', () => {
  it('流量用尽：主状态与对应的“买流量”同时出现，时长不受影响', () => {
    renderCard('sub_j9tx')
    expect(screen.getByText('流量用尽')).toBeTruthy()
    expect(screen.getByRole('button', { name: '买流量' })).toBeTruthy()
    expect(screen.queryByRole('button', { name: '续时长' })).toBeNull()
  })

  it('保留期：给出续时长并说明余量保留', () => {
    renderCard('sub_s4wp')
    expect(screen.getByText('已到期')).toBeTruthy()
    expect(screen.getByRole('button', { name: '续时长' })).toBeTruthy()
    expect(screen.getByText('余量保留，不随到期清零')).toBeTruthy()
  })

  it('待履约：没有连接可复制，引导查看订单', () => {
    renderCard('sub_g5ln')
    expect(screen.getByText('开通后在这里复制连接')).toBeTruthy()
    expect(screen.queryByRole('group', { name: /复制/ })).toBeNull()
  })

  it('可用：三种协议都能复制', () => {
    renderCard('sub_7k2m')
    for (const p of ['HTTP', 'HTTPS', 'SOCKS5']) expect(screen.getByRole('button', { name: `复制 ${p} 连接` })).toBeTruthy()
  })
})
