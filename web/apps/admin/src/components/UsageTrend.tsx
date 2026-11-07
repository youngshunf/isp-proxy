// 单个订阅的用量趋势（docs/05 F-W5、A-37）。
// 图上方固定写明来源、单位、每柱时长和数据时间；状态条先说清是“实时 / 零值 / 过期 / 断联 / 空数据”中的哪一种，
// 再画图。空数据与请求失败不画坐标轴（避免一条平线被读成 0）。图表不替代账本：余量以账本为准。
import { useQuery } from '@tanstack/react-query'
import { Button, Segmented, Spin, Table } from 'antd'
import { Suspense, lazy, useMemo, useState } from 'react'
import type { UsagePoint } from '@isp/api-client/admin'
import { StateMark, fmtAgo, fmtBytes, fmtBytesExact, fmtDateTime } from '@isp/ui'
import { qk, useApi, useServerNow } from '../app/api'
import { RANGES, bucketKind, bucketLabel, classifyUsage, type RangeKey } from '../lib/usageView'

const UsageChart = lazy(() => import('./UsageChart'))

export function UsageTrend({ subscriptionId, nodeOnline, title = '用量趋势' }: { subscriptionId: string; nodeOnline: boolean | null; title?: string }) {
  const api = useApi()
  const now = useServerNow()
  const [range, setRange] = useState<RangeKey>('24h')
  const [asTable, setAsTable] = useState(false)
  const r = RANGES[range]
  // 结束时间按 5 分钟取整，查询键稳定、不会每次渲染都换
  const to = Math.ceil(now() / 300_000) * 300_000
  const from = to - r.ms
  const q = useQuery({
    queryKey: [...qk.usage(subscriptionId, range), to],
    queryFn: () =>
      api.adminGetSubscriptionUsage({ subscriptionId, query: { from: new Date(from).toISOString(), to: new Date(to).toISOString(), bucket: r.bucket } }),
    refetchInterval: 60_000,
  })
  const s = q.data
  const view = useMemo(
    () => classifyUsage(s, { nodeOnline, error: q.error, ago: (iso) => fmtAgo(iso, now()) }),
    [s, nodeOnline, q.error, now],
  )

  const head = (
    <div className="usage-head">
      <div>
        <h3>{title}</h3>
        <div className="usage-source">
          来源：节点 agent 上报 · 单位：字节（1 GB = 10⁹）· 每柱 {bucketLabel(s?.bucketSeconds ?? (r.bucket === '5m' ? 300 : r.bucket === '1h' ? 3600 : 86_400))} · 数据时间：
          {s?.lastSampleAt ? (
            <time dateTime={s.lastSampleAt} title={fmtDateTime(s.lastSampleAt)}>
              最后样本 {fmtDateTime(s.lastSampleAt)}
            </time>
          ) : (
            '无样本'
          )}
        </div>
      </div>
      <Segmented
        size="small"
        value={range}
        onChange={(v) => setRange(v as RangeKey)}
        options={Object.entries(RANGES).map(([k, v]) => ({ value: k, label: v.label }))}
        aria-label="时间范围"
      />
    </div>
  )

  if (q.isPending) {
    return (
      <div className="usage">
        {head}
        <div className="usage-placeholder">
          <Spin size="small" /> 读取用量…
        </div>
      </div>
    )
  }

  const drawable = view.state !== 'error' && view.state !== 'empty'
  return (
    <div className="usage" data-state={view.state}>
      {head}
      <div className={`usage-state tone-${view.tone}`} role="status" aria-label={`用量状态：${view.label}`}>
        <span className="state strong">
          <StateMark shape={view.state === 'fresh' || view.state === 'zero' ? 'filled' : view.state === 'stale' ? 'half' : 'hollow'} />
          {view.label}
        </span>
        <span className="usage-detail">{view.detail}</span>
        {view.state === 'error' && (
          <Button size="small" onClick={() => void q.refetch()}>
            重试
          </Button>
        )}
      </div>
      {drawable && s ? (
        <>
          <div className="usage-sum">
            <span>
              合计 <b>{fmtBytes(view.totalBytes)}</b>
            </span>
            <span>有样本 {view.sampled} 段</span>
            <span>
              <i className="key key-zero" aria-hidden="true" />
              零值 {view.zeroBuckets} 段
            </span>
            <span>
              <i className="key key-gap" aria-hidden="true" />
              无样本 {view.gapBuckets} 段
            </span>
            <Button type="link" size="small" onClick={() => setAsTable((x) => !x)} aria-pressed={asTable}>
              {asTable ? '看图' : '数据表'}
            </Button>
          </div>
          {asTable ? (
            <UsageTable points={s.points} />
          ) : (
            <Suspense fallback={<div className="usage-placeholder" style={{ height: 240 }}><Spin size="small" /></div>}>
              <UsageChart series={s} />
            </Suspense>
          )}
        </>
      ) : (
        <div className="usage-placeholder">{view.state === 'empty' ? '这个时段没有样本，不画图。' : '没有读到数据，不画图。'}</div>
      )}
      <p className="usage-foot">趋势只用于观察；剩余流量以账本为准。</p>
    </div>
  )
}

function UsageTable({ points }: { points: UsagePoint[] }) {
  const rows = [...points].reverse()
  return (
    <Table
      size="small"
      rowKey="at"
      dataSource={rows}
      pagination={{ pageSize: 12, size: 'small', showSizeChanger: false }}
      columns={[
        { title: '时段起点', dataIndex: 'at', render: (v: string) => fmtDateTime(v) },
        {
          title: '流量',
          align: 'right',
          render: (_: unknown, p: UsagePoint) => {
            const k = bucketKind(p)
            if (k === 'gap') return <span className="muted">无样本</span>
            return <span title={fmtBytesExact(p.bytes!)}>{k === 'zero' ? '0 B（有样本）' : fmtBytes(p.bytes!)}</span>
          },
        },
        { title: '峰值连接', dataIndex: 'maxConns', align: 'right', render: (v: number | null) => v ?? '—' },
        { title: '样本数', dataIndex: 'samples', align: 'right' },
      ]}
    />
  )
}
