// A-37：断联、空数据、零值、过期分开表达；无样本的时段不是 0。
import { screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import type { UsagePoint, UsageSeries } from '@isp/api-client/admin'
import { ApiError } from '@isp/api-client/admin'
import { NetworkError } from '@isp/api-client/admin'
import { classifyUsage } from '../lib/usageView'
import { newMock, renderApp, subIdByName } from '../test/renderApp'

// jsdom 里不画 ECharts，只验证状态与说明
vi.mock('./UsageChart', () => ({ default: () => <div data-testid="usage-chart" /> }))

const H = 3600_000
const T0 = Date.parse('2026-11-07T00:00:00Z')
const pt = (i: number, bytes: number | null): UsagePoint => ({
  at: new Date(T0 + i * H).toISOString(),
  bytes,
  maxConns: bytes === null ? null : bytes ? 3 : 0,
  samples: bytes === null ? 0 : 12,
  sampledAt: bytes === null ? null : new Date(T0 + (i + 1) * H).toISOString(),
})
const series = (points: UsagePoint[], freshness: UsageSeries['freshness'], last: string | null = points.findLast((p) => p.bytes !== null)?.sampledAt ?? null): UsageSeries => ({
  subscriptionId: 'sub_1',
  source: 'agent_report',
  unit: 'bytes',
  bucketSeconds: 3600,
  from: new Date(T0).toISOString(),
  to: new Date(T0 + points.length * H).toISOString(),
  lastSampleAt: last,
  freshness,
  points,
})

describe('classifyUsage', () => {
  it('请求失败 → 断联（不是 0）', () => {
    const v = classifyUsage(undefined, { nodeOnline: true, error: new NetworkError('网络断开') })
    expect(v.state).toBe('error')
    expect(v.label).toBe('断联')
    expect(v.detail).toMatch(/不代表没有流量/)
  })

  it('一个样本都没有 → 空数据', () => {
    const v = classifyUsage(series([pt(0, null), pt(1, null)], 'no_data', null), { nodeOnline: true })
    expect(v.state).toBe('empty')
    expect(v.detail).toMatch(/不是 0/)
  })

  it('节点离线且数据过期 → 断联；之后的空白计为无样本', () => {
    const v = classifyUsage(series([pt(0, 5e6), pt(1, 4e6), pt(2, null), pt(3, null)], 'stale'), { nodeOnline: false })
    expect(v.state).toBe('disconnected')
    expect(v.gapBuckets).toBe(2)
    expect(v.totalBytes).toBe(9e6)
  })

  it('节点在线但这个订阅的上报过期 → 过期', () => {
    const v = classifyUsage(series([pt(0, 5e6), pt(1, null)], 'stale'), { nodeOnline: true })
    expect(v.state).toBe('stale')
  })

  it('有样本且全部为 0 → 零值（与空数据不同）', () => {
    const v = classifyUsage(series([pt(0, 0), pt(1, 0), pt(2, 0)], 'fresh'), { nodeOnline: true })
    expect(v.state).toBe('zero')
    expect(v.zeroBuckets).toBe(3)
    expect(v.gapBuckets).toBe(0)
  })

  it('开通前的无样本时段不算断档；中间的断档单独计数；0 与 null 分开', () => {
    const v = classifyUsage(series([pt(0, null), pt(1, null), pt(2, 7e6), pt(3, null), pt(4, 0), pt(5, 2e6)], 'fresh'), { nodeOnline: true })
    expect(v.state).toBe('fresh')
    expect(v.gapBuckets).toBe(1)
    expect(v.zeroBuckets).toBe(1)
    expect(v.sampled).toBe(3)
  })
})

describe('UsageTrend 在页面上的表达', () => {
  async function show(name: string, patch?: (api: ReturnType<typeof newMock>) => void) {
    const api = newMock()
    patch?.(api)
    const id = await subIdByName(api, name)
    renderApp(`/monitoring?sub=${id}`, api)
    return screen.findByRole('status', { name: /用量状态/ }, { timeout: 4000 })
  }

  it('来源、单位、数据时间写在图上方', async () => {
    await show('纽约 2 号')
    const src = screen.getByText(/来源：节点 agent 上报/)
    expect(src.textContent).toMatch(/单位：字节/)
    expect(src.textContent).toMatch(/数据时间：最后样本/)
  })

  it('节点离线的订阅显示“断联”，仍画出已有数据', async () => {
    const s = await show('亚马逊德国店')
    expect(within(s).getByText('断联')).toBeTruthy()
    expect(await screen.findByTestId('usage-chart')).toBeTruthy()
  })

  it('未开通的订阅显示“空数据”且不画图', async () => {
    const s = await show('新 IP')
    expect(within(s).getByText('空数据')).toBeTruthy()
    expect(screen.queryByTestId('usage-chart')).toBeNull()
    expect(screen.getByText('这个时段没有样本，不画图。')).toBeTruthy()
  })

  it('有断档的实时数据：标出无样本段数', async () => {
    const s = await show('纽约 2 号')
    expect(within(s).getByText('实时')).toBeTruthy()
    expect(screen.getByText(/无样本 [1-9]\d* 段/)).toBeTruthy()
  })

  it('全部为 0 的样本显示“零值”', async () => {
    const s = await show('纽约 2 号', (api) => {
      api.adminGetSubscriptionUsage = async () => series([pt(0, 0), pt(1, 0)], 'fresh')
    })
    expect(within(s).getByText('零值')).toBeTruthy()
  })

  it('节点在线但上报过期显示“数据过期”', async () => {
    const s = await show('纽约 2 号', (api) => {
      api.adminGetSubscriptionUsage = async () => series([pt(0, 4e6), pt(1, null)], 'stale')
    })
    expect(within(s).getByText('数据过期')).toBeTruthy()
  })

  it('指标请求失败显示“断联”并可重试，不画图', async () => {
    const s = await show('纽约 2 号', (api) => {
      api.adminGetSubscriptionUsage = async () => {
        throw new ApiError('rate_limited', '请求太频繁')
      }
    })
    expect(within(s).getByText('断联')).toBeTruthy()
    expect(within(s).getByRole('button', { name: '重试' })).toBeTruthy()
    expect(screen.queryByTestId('usage-chart')).toBeNull()
  })
})
