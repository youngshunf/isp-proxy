// 上游库存（R-12）：录入（粘贴地址或逐项填写）、编辑（带行版本，防止覆盖别人的修改）、经节点验证、改状态。
// 上游密码只写不读：接口只返回“是否已设密码”，编辑时留空表示不改。
import { EditOutlined, PlusOutlined, RadarChartOutlined } from '@ant-design/icons'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { App, Button, Input, InputNumber, Modal, Segmented, Select, Space } from 'antd'
import { useEffect, useMemo, useState } from 'react'
import { Link, useSearchParams } from 'react-router'
import type { Probe, Upstream, UpstreamStatus } from '@isp/api-client/admin'
import { CountryFlag, Plate, fmtDateTime } from '@isp/ui'
import { errorText, qk, useApi } from '../app/api'
import { Callout, Mono, PageHead, State } from '../components/bits'
import { ConfirmAction } from '../components/ConfirmAction'
import { DataTable, arr, str, type Column, type FilterDef } from '../components/DataTable'
import { useProbe } from '../lib/jobs'
import { UPSTREAM, options } from '../lib/status'

const COUNTRIES = [
  { value: 'us', label: '美国', },
  { value: 'gb', label: '英国' },
  { value: 'de', label: '德国' },
  { value: 'jp', label: '日本' },
  { value: 'sg', label: '新加坡' },
  { value: 'ca', label: '加拿大' },
]

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

interface FormState {
  mode: 'url' | 'fields'
  url: string
  protocol: 'http' | 'https' | 'socks5'
  host: string
  port: number | null
  username: string
  password: string
  nodeId?: string
  countryCode?: string
  city: string
  provider: string
  note: string
}

function UpstreamDialog({ value, nodes, onClose }: { value: Upstream | 'new' | null; nodes: { value: string; label: string }[]; onClose: () => void }) {
  const api = useApi()
  const qc = useQueryClient()
  const { message } = App.useApp()
  const editing = value && value !== 'new' ? value : null
  const blank: FormState = { mode: 'url', url: '', protocol: 'socks5', host: '', port: null, username: '', password: '', nodeId: nodes[0]?.value, countryCode: 'us', city: '', provider: '', note: '' }
  const [f, setF] = useState<FormState>(blank)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  useEffect(() => {
    if (!value) return
    setError(null)
    if (value === 'new') setF({ ...blank, nodeId: nodes[0]?.value })
    else
      setF({
        mode: 'fields',
        url: '',
        protocol: value.protocol,
        host: value.host,
        port: value.port,
        username: value.username,
        password: '',
        nodeId: value.nodeId,
        countryCode: value.countryCode,
        city: value.city,
        provider: value.provider,
        note: value.note,
      })
  }, [value])
  const set = <K extends keyof FormState>(k: K, v: FormState[K]) => setF((x) => ({ ...x, [k]: v }))
  const country = COUNTRIES.find((c) => c.value === f.countryCode)

  const submit = async () => {
    setBusy(true)
    setError(null)
    try {
      if (editing) {
        await api.adminUpdateUpstream({
          upstreamId: editing.id,
          body: {
            rowVersion: editing.rowVersion,
            protocol: f.protocol,
            host: f.host.trim(),
            port: f.port ?? undefined,
            username: f.username,
            password: f.password || undefined,
            countryCode: f.countryCode,
            country: country?.label,
            city: f.city.trim(),
            provider: f.provider,
            note: f.note,
          },
        })
        message.success('已保存')
      } else {
        await api.adminCreateUpstream({
          body: {
            ...(f.mode === 'url' ? { url: f.url.trim() } : { protocol: f.protocol, host: f.host.trim(), port: f.port ?? undefined, username: f.username, password: f.password || undefined }),
            nodeId: f.nodeId!,
            countryCode: f.countryCode!,
            country: country?.label ?? f.countryCode!,
            city: f.city.trim(),
            provider: f.provider || undefined,
            note: f.note || undefined,
          },
        })
        message.success('已录入，状态为“待验证”')
      }
      void qc.invalidateQueries({ queryKey: qk.upstreams })
      onClose()
    } catch (e) {
      setError(errorText(e))
    } finally {
      setBusy(false)
    }
  }

  const connReady = f.mode === 'url' ? f.url.trim().length > 0 : f.host.trim().length > 0 && !!f.port
  const ready = connReady && !!f.nodeId && !!f.countryCode && f.city.trim().length > 0

  return (
    <Modal
      open={!!value}
      title={editing ? `编辑上游 ${editing.host}` : '录入上游'}
      onCancel={() => !busy && onClose()}
      onOk={submit}
      okText={editing ? '保存' : '录入'}
      cancelText="取消"
      okButtonProps={{ disabled: !ready, loading: busy }}
      destroyOnHidden
      width={640}
    >
      <div className="confirm-body">
        {editing?.status === 'bound' && <Callout tone="warn">资源已被订阅占用：只能改地区、供应商与备注；改连接信息要先为订阅换出口。</Callout>}
        {editing && editing.status !== 'bound' && <Callout tone="info">修改协议、主机、端口、用户名或密码后，资源回到“待验证”，需重新验证才能售卖。</Callout>}
        {!editing && (
          <Segmented
            value={f.mode}
            onChange={(v) => set('mode', v as FormState['mode'])}
            options={[
              { value: 'url', label: '粘贴地址' },
              { value: 'fields', label: '逐项填写' },
            ]}
          />
        )}
        {f.mode === 'url' && !editing ? (
          <label className="field">
            <span className="field-label">上游地址</span>
            <Input aria-label="上游地址" className="mono" value={f.url} onChange={(e) => set('url', e.target.value)} placeholder="socks5://user:pass@gw.example.net:1080" />
          </label>
        ) : (
          <div className="form-grid">
            <label className="field">
              <span className="field-label">协议</span>
              <Select aria-label="协议" value={f.protocol} onChange={(v) => set('protocol', v)} options={['http', 'https', 'socks5'].map((x) => ({ value: x, label: x.toUpperCase() }))} />
            </label>
            <label className="field">
              <span className="field-label">端口</span>
              <InputNumber aria-label="端口" min={1} max={65535} value={f.port} onChange={(v) => set('port', v)} style={{ width: '100%' }} />
            </label>
            <label className="field full" style={{ marginTop: 12 }}>
              <span className="field-label">主机</span>
              <Input aria-label="主机" className="mono" value={f.host} onChange={(e) => set('host', e.target.value)} />
            </label>
            <label className="field" style={{ marginTop: 12 }}>
              <span className="field-label">用户名</span>
              <Input aria-label="用户名" value={f.username} onChange={(e) => set('username', e.target.value)} autoComplete="off" />
            </label>
            <label className="field" style={{ marginTop: 12 }}>
              <span className="field-label">密码{editing && <em>{editing.hasPassword ? '已设置，留空不改' : '未设置'}</em>}</span>
              <Input.Password aria-label="上游密码" value={f.password} onChange={(e) => set('password', e.target.value)} autoComplete="new-password" />
            </label>
          </div>
        )}
        <div className="form-grid">
          <label className="field">
            <span className="field-label">节点</span>
            <Select aria-label="节点" value={f.nodeId} onChange={(v) => set('nodeId', v)} options={nodes} disabled={!!editing} />
          </label>
          <label className="field">
            <span className="field-label">国家</span>
            <Select aria-label="国家" value={f.countryCode} onChange={(v) => set('countryCode', v)} options={COUNTRIES} />
          </label>
          <label className="field" style={{ marginTop: 12 }}>
            <span className="field-label">城市</span>
            <Input aria-label="城市" value={f.city} onChange={(e) => set('city', e.target.value)} />
          </label>
          <label className="field" style={{ marginTop: 12 }}>
            <span className="field-label">供应商</span>
            <Input aria-label="供应商" value={f.provider} onChange={(e) => set('provider', e.target.value)} />
          </label>
          <label className="field full" style={{ marginTop: 12 }}>
            <span className="field-label">备注</span>
            <Input aria-label="备注" value={f.note} onChange={(e) => set('note', e.target.value)} />
          </label>
        </div>
        {error && <Callout tone="stop">{error}</Callout>}
      </div>
    </Modal>
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
