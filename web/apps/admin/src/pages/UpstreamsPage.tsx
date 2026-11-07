// 上游库存（R-12）：录入（粘贴地址或逐项填写）、编辑（带行版本，防止覆盖别人的修改）、经节点验证、改状态。
// 上游密码只写不读：接口只返回“是否已设密码”，编辑时留空表示不改。
import { EditOutlined, PlusOutlined, RadarChartOutlined } from '@ant-design/icons'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { App, Button, Segmented, Space } from 'antd'
import { useEffect, useMemo, useState } from 'react'
import { Link, useSearchParams } from 'react-router'
import type { Probe, Upstream, UpstreamStatus } from '@isp/api-client/admin'
import { CountryFlag, Plate, fmtDateTime } from '@isp/ui'
import { errorText, qk, useApi } from '../app/api'
import { Mono, PageHead, State } from '../components/bits'
import { ConfirmAction } from '../components/ConfirmAction'
import { DataTable, arr, str, type Column, type FilterDef } from '../components/DataTable'
import { COUNTRIES, UpstreamDialog } from '../components/UpstreamDialog'
import { useProbe } from '../lib/jobs'
import { UPSTREAM, options } from '../lib/status'

export default function UpstreamsPage() {
  const api = useApi()
  const [sp, setSp] = useSearchParams()
  const nodes = useQuery({ queryKey: qk.nodes, queryFn: () => api.adminListNodes() })
  const [editing, setEditing] = useState<Upstream | 'new' | null>(sp.get('new') === '1' ? 'new' : null)
  const [statusOf, setStatusOf] = useState<Upstream | null>(null)
  const [probes, setProbes] = useState<Record<string, Probe>>({})
  const { message } = App.useApp()

  const verify = async (u: Upstream) => {
    try {
      const p = await api.adminVerifyUpstream({ upstreamId: u.id })
      setProbes((m) => ({ ...m, [u.id]: p }))
    } catch (e) {
      message.error(errorText(e))
    }
  }

  const filters = useMemo<FilterDef[]>(
    () => [
      { key: 'q', label: '搜索', type: 'search', placeholder: '主机、资源 ID、供应商、出口 IP' },
      { key: 'status', label: '状态', type: 'multi', options: options(UPSTREAM) },
      { key: 'countryCode', label: '国家', type: 'select', options: COUNTRIES, width: 110 },
      { key: 'nodeId', label: '节点', type: 'select', options: (nodes.data ?? []).map((n) => ({ value: n.id, label: n.name })), width: 130 },
    ],
    [nodes.data],
  )
  const columns: Column<Upstream>[] = [
    {
      key: 'addr',
      title: '上游',
      fixedVisible: true,
      render: (_, u) => (
        <div className="cell-stack">
          <Mono>{`${u.protocol}://${u.host}:${u.port}`}</Mono>
          <span className="muted">
            <Mono>{u.id}</Mono>
          </span>
        </div>
      ),
    },
    {
      key: 'auth',
      title: '认证',
      render: (_, u) => (
        <div className="cell-stack">
          <Mono>{u.username || '（无用户名）'}</Mono>
          <span className="muted">{u.hasPassword ? '已设密码' : '无密码'}</span>
        </div>
      ),
    },
    {
      key: 'region',
      title: '地区',
      render: (_, u) => (
        <span style={{ display: 'inline-flex', gap: 6, alignItems: 'center' }}>
          <CountryFlag code={u.countryCode} name={u.country} size="sm" />
          {u.city}
        </span>
      ),
    },
    { key: 'provider', title: '供应商', render: (_, u) => u.provider || '—' },
    {
      key: 'status',
      title: '状态',
      render: (_, u) => (
        <div className="cell-stack">
          <State view={UPSTREAM[u.status]} />
          {u.statusReason && <span className="muted">{u.statusReason}</span>}
          {u.cooldownUntil && u.status === 'cooldown' && <span className="muted">冷却至 {fmtDateTime(u.cooldownUntil)}</span>}
        </div>
      ),
    },
    {
      key: 'use',
      title: '占用',
      render: (_, u) =>
        u.boundSubscriptionId ? (
          <Link to={`/subscriptions/${u.boundSubscriptionId}`}>订阅</Link>
        ) : u.reservedOrderId ? (
          <Link to={`/orders/${u.reservedOrderId}`}>订单预留</Link>
        ) : u.reservedTaskId ? (
          '任务预留'
        ) : (
          <span className="muted">—</span>
        ),
    },
    {
      key: 'verify',
      title: '最近验证',
      render: (_, u) => (probes[u.id] ? <VerifyCell probe={probes[u.id]!} /> : <LastVerify u={u} />),
    },
    { key: 'node', title: '节点', render: (_, u) => <Mono>{u.nodeId}</Mono> },
    { key: 'note', title: '备注', defaultHidden: true, render: (_, u) => u.note || '—' },
    { key: 'updated', title: '更新', defaultHidden: true, render: (_, u) => fmtDateTime(u.updatedAt) },
    {
      key: 'op',
      title: '操作',
      fixedVisible: true,
      fixed: 'right',
      render: (_, u) => (
        <Space size={4} wrap>
          <Button size="small" icon={<RadarChartOutlined />} onClick={() => verify(u)} disabled={u.status === 'retired'}>
            验证
          </Button>
          <Button size="small" icon={<EditOutlined />} onClick={() => setEditing(u)} aria-label={`编辑 ${u.host}`}>
            编辑
          </Button>
          <Button size="small" onClick={() => setStatusOf(u)} disabled={u.status === 'bound' || u.status === 'reserved'}>
            状态
          </Button>
        </Space>
      ),
    },
  ]

  return (
    <>
      <PageHead
        title="上游库存"
        sub="一个上游资源对应一个出口 IP。新录入的资源须经节点验证出口后才能设为可售。"
        extra={
          <Button type="primary" icon={<PlusOutlined />} onClick={() => setEditing('new')}>
            录入上游
          </Button>
        }
      />
      <Plate>
        <DataTable<Upstream>
          id="upstreams"
          label="上游库存"
          columns={columns}
          filters={filters}
          queryKey={qk.upstreams}
          rowKey={(u) => u.id}
          rowClassName={(u) => (u.status === 'quarantine' ? 'row-flag' : '')}
          fetchPage={({ filters: f, cursor, limit }) =>
            api.adminListUpstreams({
              query: { q: str(f, 'q'), status: arr<UpstreamStatus>(f, 'status'), countryCode: str(f, 'countryCode'), nodeId: str(f, 'nodeId'), cursor, limit },
            })
          }
        />
      </Plate>
      <UpstreamDialog
        value={editing}
        nodes={(nodes.data ?? []).map((n) => ({ value: n.id, label: n.name }))}
        onClose={() => {
          setEditing(null)
          if (sp.get('new')) {
            const next = new URLSearchParams(sp)
            next.delete('new')
            setSp(next, { replace: true })
          }
        }}
      />
      {statusOf && <StatusDialog u={statusOf} onClose={() => setStatusOf(null)} />}
    </>
  )
}

function LastVerify({ u }: { u: Upstream }) {
  if (!u.lastVerify) return <span className="muted">从未验证</span>
  const v = u.lastVerify
  return (
    <div className="cell-stack">
      {v.ok ? (
        <span>
          <Mono>{v.exitIp}</Mono> <span className="muted">{v.exitCountryCode?.toUpperCase()}</span>
        </span>
      ) : (
        <span className="state tone-stop">失败：{v.error}</span>
      )}
      <span className="muted">{fmtDateTime(v.at)}</span>
    </div>
  )
}

function VerifyCell({ probe }: { probe: Probe }) {
  const q = useProbe(probe.id, probe)
  const qc = useQueryClient()
  const p = q.data ?? probe
  const done = p.status === 'done' || p.status === 'expired'
  useEffect(() => {
    if (done) void qc.invalidateQueries({ queryKey: qk.upstreams })
  }, [done, qc])
  if (!done) return <span className="state tone-wait">验证中…</span>
  if (p.result === 'failed' || p.status === 'expired') return <span className="state tone-stop">失败：{p.error ?? '探测过期'}</span>
  return (
    <span>
      <Mono>{p.observedIp}</Mono> · {p.latencyMs} ms
    </span>
  )
}

function StatusDialog({ u, onClose }: { u: Upstream; onClose: () => void }) {
  const api = useApi()
  const qc = useQueryClient()
  const { message } = App.useApp()
  const [to, setTo] = useState<'available' | 'quarantine' | 'retired'>(u.status === 'available' ? 'quarantine' : 'available')
  const impacts: Record<typeof to, string[]> = {
    available: ['资源进入可售库存，新订单与换出口可以选用它。', u.lastVerify?.ok ? `最近一次验证通过（出口 ${u.lastVerify.exitIp}）。` : '最近一次验证没有通过：服务端会拒绝，请先重新验证。'],
    quarantine: ['资源退出可售库存，不会被新订单或换出口选中。', '用于疑似被封、质量异常的资源；排查后可恢复为可售。'],
    retired: ['资源永久停用，不再验证、不再售卖。', '历史绑定记录保留。'],
  }
  return (
    <ConfirmAction
      open
      onClose={onClose}
      tone={to === 'retired' ? 'stop' : 'warn'}
      title={`改状态 · ${u.host}:${u.port}`}
      okText="确认修改"
      reasonLabel="原因"
      impacts={[`状态 ${UPSTREAM[u.status].label} → ${UPSTREAM[to].label}`, ...impacts[to]]}
      onConfirm={(reason) => api.adminSetUpstreamStatus({ upstreamId: u.id, body: { status: to, reason } })}
      onDone={() => {
        message.success('状态已更新')
        void qc.invalidateQueries({ queryKey: qk.upstreams })
      }}
    >
      <Segmented
        value={to}
        onChange={(v) => setTo(v as typeof to)}
        options={[
          { value: 'available', label: '可售' },
          { value: 'quarantine', label: '隔离' },
          { value: 'retired', label: '停用' },
        ]}
      />
    </ConfirmAction>
  )
}
