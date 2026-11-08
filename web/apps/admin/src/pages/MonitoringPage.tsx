// 节点与监控（R-12、R-19）：节点在线、配置版本、计量持久化、时钟偏差、端口池；单个订阅的用量趋势。
// 节点没有上报时写“无数据”，不写 0；趋势图的断联 / 空数据 / 零值 / 过期分开表达（A-37）。
// 节点在这里登记（名称、公共地址、端口池）和修改（名称、公共地址、状态）。
import { EditOutlined, PlusOutlined } from '@ant-design/icons'
import { useQuery } from '@tanstack/react-query'
import { Button, Select } from 'antd'
import { useMemo, useState } from 'react'
import { Link, useSearchParams } from 'react-router'
import type { Node } from '@isp/api-client/admin'
import { Plate, StateMark } from '@isp/ui'
import { qk, useApi } from '../app/api'
import { Ago, LoadError, Loading, Mono, PageHead, State } from '../components/bits'
import { NodeDialog } from '../components/NodeDialog'
import { UsageTrend } from '../components/UsageTrend'

function nodeState(n: Node) {
  if (n.status === 'disabled') return { label: '已停用', tone: 'stop', shape: 'hollow' } as const
  if (!n.online) return { label: '离线', tone: 'stop', shape: 'hollow' } as const
  if (n.status === 'draining') return { label: '排空中', tone: 'warn', shape: 'half' } as const
  return { label: '在线', tone: 'ok', shape: 'filled' } as const
}

export default function MonitoringPage() {
  const api = useApi()
  const nodes = useQuery({ queryKey: qk.nodes, queryFn: () => api.adminListNodes(), refetchInterval: 15_000 })
  const [editing, setEditing] = useState<Node | 'new' | null>(null)
  return (
    <>
      <PageHead
        title="节点与监控"
        sub="节点每 ≤ 5 秒上报一次累计用量；最近 15 秒内有上报为“实时”，否则为“过期”。"
        extra={
          <Button icon={<PlusOutlined />} onClick={() => setEditing('new')}>
            登记节点
          </Button>
        }
      />
      <div className="stack">
        <Plate title="节点">
          {nodes.isPending ? (
            <Loading />
          ) : nodes.isError ? (
            <LoadError error={nodes.error} retry={() => void nodes.refetch()} />
          ) : (
            <div className="nodes" style={{ marginTop: 12 }}>
              {nodes.data.map((n) => (
                <NodeCard key={n.id} n={n} onEdit={() => setEditing(n)} />
              ))}
            </div>
          )}
        </Plate>
        <Plate>
          <div className="plate-pad">
            <TrendPicker nodes={nodes.data ?? []} />
          </div>
        </Plate>
      </div>
      <NodeDialog value={editing} onClose={() => setEditing(null)} />
    </>
  )
}

function NodeCard({ n, onEdit }: { n: Node; onEdit: () => void }) {
  const st = nodeState(n)
  const p = n.ports
  const pct = (x: number) => `${p.total ? (x / p.total) * 100 : 0}%`
  const skew = n.clockSkewMs === null ? null : Math.abs(n.clockSkewMs)
  return (
    <article className="node" aria-label={`节点 ${n.name}`}>
      <div className="node-head">
        <b>{n.name}</b>
        <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
          <State view={st} />
          <Button size="small" type="text" icon={<EditOutlined />} onClick={onEdit} aria-label={`修改节点 ${n.name}`} />
        </span>
      </div>
      <div className="id-line">
        <Mono>{n.id}</Mono>
        <Mono>{n.publicHost}</Mono>
        <span>agent {n.agentVersion ?? '—'}</span>
      </div>
      <dl className="facts" style={{ fontSize: 13 }}>
        <div className="fact">
          <dt>直出地域</dt>
          <dd>{n.directRegion ? `${n.directRegion.name}（${n.directRegion.countryCode.toUpperCase()}）` : <span className="muted">未填写</span>}</dd>
        </div>
        <div className="fact">
          <dt>心跳</dt>
          <dd>
            <Ago at={n.lastSeenAt} empty="从未" />
          </dd>
        </div>
        <div className="fact">
          <dt>计量上报</dt>
          <dd>
            <Ago at={n.lastReportAt} empty="从未" />
          </dd>
        </div>
        <div className="fact">
          <dt>配置版本</dt>
          <dd className={n.appliedRevision !== n.desiredRevision ? 'state tone-warn' : undefined}>
            已应用 {n.appliedRevision} / 期望 {n.desiredRevision}
          </dd>
        </div>
        <div className="fact">
          <dt>计量持久化</dt>
          <dd>
            {n.persistOk === null ? (
              <span className="muted">无数据</span>
            ) : n.persistOk ? (
              <span className="state tone-ok">
                <StateMark shape="filled" />
                正常
              </span>
            ) : (
              <span className="state tone-stop">
                <StateMark shape="hollow" />
                失败
              </span>
            )}
          </dd>
        </div>
        <div className="fact">
          <dt>时钟偏差</dt>
          <dd className={skew !== null && skew > 1000 ? 'state tone-warn' : undefined}>{skew === null ? <span className="muted">无数据</span> : `${n.clockSkewMs} ms`}</dd>
        </div>
        <div className="fact">
          <dt>订阅 / 连接</dt>
          <dd>
            <Link to={`/subscriptions?nodeId=${n.id}`}>{n.subscriptions} 个订阅</Link> · {n.activeConns === null ? <span className="muted">连接无数据</span> : `${n.activeConns} 个连接`}
          </dd>
        </div>
      </dl>
      <div>
        <div className="port-bar" role="img" aria-label={`端口池 ${p.total} 个：已分配 ${p.allocated}，冷却 ${p.cooldown}，空闲 ${p.free}`}>
          <i className="used" style={{ width: pct(p.allocated) }} />
          <i className="cool" style={{ width: pct(p.cooldown) }} />
        </div>
        <div className="muted small" style={{ marginTop: 4 }}>
          端口 {n.portRange.start}–{n.portRange.end} · 已分配 {p.allocated} · 冷却 {p.cooldown} · 空闲 {p.free}
        </div>
      </div>
    </article>
  )
}

function TrendPicker({ nodes }: { nodes: Node[] }) {
  const api = useApi()
  const [sp, setSp] = useSearchParams()
  const [q, setQ] = useState('')
  const subs = useQuery({
    queryKey: [...qk.subs, 'trend-pick', q],
    queryFn: () => api.adminListSubscriptions({ query: { q: q || undefined, lifecycle: ['active', 'retained', 'pending'], limit: 30 } }),
  })
  const chosenId = sp.get('sub') ?? (subs.data?.items.find((s) => s.blockers.length === 0) ?? subs.data?.items[0])?.id
  const chosen = useQuery({
    queryKey: qk.sub(chosenId ?? '-'),
    queryFn: () => api.adminGetSubscription({ subscriptionId: chosenId! }),
    enabled: !!chosenId,
  })
  const node = useMemo(() => nodes.find((n) => n.id === chosen.data?.nodeId), [nodes, chosen.data?.nodeId])
  return (
    <div className="stack" style={{ gap: 14 }}>
      <div className="usage-head">
        <div>
          <h3 style={{ fontSize: 17 }}>订阅用量趋势</h3>
          <p className="muted small">按订阅查看节点上报的流量。剩余流量以账本为准，图表只用于观察。</p>
        </div>
        <Select
          aria-label="选择订阅"
          style={{ width: 320, maxWidth: '100%' }}
          showSearch={{ filterOption: false, onSearch: setQ }}
          value={chosenId}
          onChange={(v) => setSp({ sub: v }, { replace: true })}
          placeholder="搜索订阅名称、ID、IP"
          options={(subs.data?.items ?? []).map((s) => ({ value: s.id, label: `${s.name} · ${s.user.email}` }))}
          loading={subs.isFetching}
        />
      </div>
      {chosen.data && (
        <div className="id-line">
          <Link to={`/subscriptions/${chosen.data.id}`}>{chosen.data.name}</Link>
          <Mono>{chosen.data.id}</Mono>
          <span>节点 {node ? `${node.name}（${nodeState(node).label}）` : '—'}</span>
        </div>
      )}
      {chosenId ? <UsageTrend key={chosenId} subscriptionId={chosenId} nodeOnline={node ? node.online : null} title="流量（每柱）" /> : <Loading rows={3} />}
    </div>
  )
}
