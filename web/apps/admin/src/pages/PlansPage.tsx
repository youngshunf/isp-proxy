// 套餐与流量包（R-12）：编辑即发布新版本；已下单与已开通的订阅继续按原版本执行，新订单使用新版本。
// 上架 / 下架只影响新订单；版本历史可展开查看。
// 出口类型（D-24）：独享 / 共享在新建时选定，之后不能改（已售订阅按购买时的类型执行）。
import { PlusOutlined } from '@ant-design/icons'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { App, Button, Input, InputNumber, Segmented, Select, Space, Table, Tabs } from 'antd'
import { useState } from 'react'
import type { AdminPlan, AdminTrafficPack, ExitMode, PlanVersionInput, SaleStatus } from '@isp/api-client/admin'
import { CountryFlag, ExitModeChip, GB, Plate, fmtBytes, fmtDateTime, fmtMoney } from '@isp/ui'
import { qk, useApi } from '../app/api'
import { LoadError, Loading, Mono, PageHead, State } from '../components/bits'
import { ConfirmAction } from '../components/ConfirmAction'
import { SALE } from '../lib/status'

const COUNTRY: Record<string, string> = { us: '美国', gb: '英国', de: '德国', jp: '日本', sg: '新加坡', ca: '加拿大' }

export default function PlansPage() {
  return (
    <>
      <PageHead title="套餐" sub="修改价格或规格会发布一个新版本：已有订单与订阅不受影响，新订单按新版本。" />
      <Plate className="tabs-plate">
        <Tabs
          items={[
            { key: 'plans', label: '套餐', children: <PlanTable /> },
            { key: 'packs', label: '流量包', children: <PackTable /> },
          ]}
        />
      </Plate>
    </>
  )
}

type PlanDlg = { kind: 'version'; plan: AdminPlan } | { kind: 'new' } | { kind: 'status'; plan: AdminPlan } | null

function PlanTable() {
  const api = useApi()
  const q = useQuery({ queryKey: qk.plans, queryFn: () => api.adminListPlans() })
  const [dlg, setDlg] = useState<PlanDlg>(null)
  const [status, setStatus] = useState<SaleStatus | 'all'>('all')
  if (q.isPending) return <Loading />
  if (q.isError) return <LoadError error={q.error} retry={() => void q.refetch()} />
  const rows = q.data.filter((p) => status === 'all' || p.status === status)
  return (
    <>
      <div className="dt-bar">
        <Segmented
          value={status}
          onChange={(v) => setStatus(v as typeof status)}
          options={[{ value: 'all', label: '全部' }, ...Object.entries(SALE).map(([value, v]) => ({ value, label: v.label }))]}
          aria-label="销售状态"
        />
        <Button icon={<PlusOutlined />} onClick={() => setDlg({ kind: 'new' })}>
          新建套餐
        </Button>
      </div>
      <Table<AdminPlan>
        className="plain-table"
        size="small"
        rowKey="id"
        pagination={false}
        scroll={{ x: 'max-content' }}
        dataSource={rows}
        expandable={{ expandedRowRender: (p) => <PlanVersions planId={p.id} /> }}
        columns={[
          {
            title: '套餐',
            render: (_, p) => (
              <span style={{ display: 'inline-flex', gap: 8, alignItems: 'center' }}>
                <CountryFlag code={p.current.countryCode} name={p.current.country} size="sm" />
                <span className="cell-stack">
                  <span>{p.current.name}</span>
                  <span className="muted">
                    <Mono>{p.code}</Mono> · v{p.current.version}
                  </span>
                </span>
              </span>
            ),
          },
          { title: '出口', render: (_, p) => <ExitModeChip mode={p.exitMode} /> },
          { title: '月价', align: 'right', render: (_, p) => <span className="amount">{fmtMoney(p.current.price)}</span> },
          { title: '含流量', align: 'right', render: (_, p) => fmtBytes(p.current.includedBytes) },
          { title: '规格', render: (_, p) => `${p.current.concurrency} 并发 · ${p.current.bandwidthMbps} Mbps · ${p.current.periodDays} 天` },
          {
            title: '库存',
            render: (_, p) => (
              <div className="cell-stack">
                <State view={{ plenty: { label: '充足', tone: 'ok', shape: 'filled' }, low: { label: '偏少', tone: 'warn', shape: 'half' }, out: { label: '售罄', tone: 'stop', shape: 'hollow' } }[p.stock.level] as never} strong={false} />
                <span className="muted">
                  {p.exitMode === 'shared' ? '剩余名额' : '可售'} {p.stock.available} · 预留 {p.stock.reserved} · 占用 {p.stock.bound}
                </span>
              </div>
            ),
          },
          { title: '状态', render: (_, p) => <State view={SALE[p.status]} /> },
          {
            title: '操作',
            render: (_, p) => (
              <Space size={4}>
                <Button size="small" onClick={() => setDlg({ kind: 'version', plan: p })}>
                  发布新版本
                </Button>
                <Button size="small" onClick={() => setDlg({ kind: 'status', plan: p })}>
                  {p.status === 'on_sale' ? '下架' : '上架'}
                </Button>
              </Space>
            ),
          },
        ]}
      />
      {dlg?.kind === 'version' && <PlanVersionDialog plan={dlg.plan} onClose={() => setDlg(null)} />}
      {dlg?.kind === 'new' && <PlanVersionDialog onClose={() => setDlg(null)} />}
      {dlg?.kind === 'status' && (
        <SaleDialog
          name={dlg.plan.current.name}
          current={dlg.plan.status}
          onClose={() => setDlg(null)}
          submit={(status) => api.adminSetPlanStatus({ planId: dlg.plan.id, body: { status } })}
          invalidate={qk.plans}
        />
      )}
    </>
  )
}

function PlanVersions({ planId }: { planId: string }) {
  const api = useApi()
  const q = useQuery({ queryKey: [...qk.plans, planId], queryFn: () => api.adminGetPlan({ planId }) })
  if (q.isPending) return <Loading rows={2} />
  if (q.isError) return <LoadError error={q.error} />
  return (
    <Table
      size="small"
      rowKey="id"
      pagination={false}
      dataSource={q.data.versions}
      columns={[
        { title: '版本', dataIndex: 'version', render: (v: number) => <span className="version-chip">v{v}</span> },
        { title: '名称', dataIndex: 'name' },
        { title: '月价', dataIndex: 'price', align: 'right', render: (v) => fmtMoney(v) },
        { title: '含流量', dataIndex: 'includedBytes', align: 'right', render: (v: number) => fmtBytes(v) },
        { title: '规格', render: (_, v) => `${v.concurrency} 并发 · ${v.bandwidthMbps} Mbps · ${v.periodDays} 天` },
        { title: '发布', render: (_, v) => `${fmtDateTime(v.createdAt)} · ${v.createdBy?.email ?? '系统'}` },
      ]}
    />
  )
}

function PlanVersionDialog({ plan, onClose }: { plan?: AdminPlan; onClose: () => void }) {
  const api = useApi()
  const qc = useQueryClient()
  const { message } = App.useApp()
  const c = plan?.current
  const [code, setCode] = useState('')
  const [exitMode, setExitMode] = useState<ExitMode>('exclusive')
  const [v, setV] = useState<PlanVersionInput>(
    c
      ? { name: c.name, countryCode: c.countryCode, country: c.country, city: c.city, price: c.price, periodDays: c.periodDays, includedBytes: c.includedBytes, concurrency: c.concurrency, bandwidthMbps: c.bandwidthMbps }
      : { name: '', countryCode: 'us', country: '美国', city: '', price: { amount: 0, currency: 'CNY' }, periodDays: 30, includedBytes: 100 * GB, concurrency: 50, bandwidthMbps: 50 },
  )
  const set = <K extends keyof PlanVersionInput>(k: K, x: PlanVersionInput[K]) => setV((o) => ({ ...o, [k]: x }))
  const diffs: string[] = []
  if (c) {
    if (c.name !== v.name) diffs.push(`名称：${c.name} → ${v.name}`)
    if (c.price.amount !== v.price.amount) diffs.push(`月价：${fmtMoney(c.price)} → ${fmtMoney(v.price)}`)
    if (c.includedBytes !== v.includedBytes) diffs.push(`含流量：${fmtBytes(c.includedBytes)} → ${fmtBytes(v.includedBytes)}`)
    if (c.concurrency !== v.concurrency) diffs.push(`并发：${c.concurrency} → ${v.concurrency}`)
    if (c.bandwidthMbps !== v.bandwidthMbps) diffs.push(`带宽：${c.bandwidthMbps} → ${v.bandwidthMbps} Mbps`)
    if (c.periodDays !== v.periodDays) diffs.push(`周期：${c.periodDays} → ${v.periodDays} 天`)
    if (c.city !== v.city || c.countryCode !== v.countryCode) diffs.push(`地区：${c.country} ${c.city} → ${v.country} ${v.city}`)
  }
  const ready = v.name.trim() && v.city.trim() && v.concurrency >= 1 && v.bandwidthMbps >= 1 && (plan ? diffs.length > 0 : /^[a-z0-9][a-z0-9-]{1,31}$/.test(code))
  return (
    <ConfirmAction
      open
      onClose={onClose}
      tone="warn"
      title={plan ? `发布新版本 · ${c!.name}（当前 v${c!.version}）` : '新建套餐'}
      okText={plan ? `发布 v${c!.version + 1}` : '创建（草稿）'}
      ready={!!ready}
      impacts={
        plan
          ? [...(diffs.length ? diffs : ['还没有改动']), '已下单（含待核款）与已开通的订阅继续按原版本；新订单与续费按新版本。', '旧版本保留在历史中，不能修改或删除。']
          : [
              '新建的套餐为“草稿”，上架后才出现在用户端。',
              exitMode === 'shared'
                ? '共享：每个订阅分到一个绑定上限 ≥2 的资源上的名额，同一出口最多由该资源的上限个订阅共用；库存按剩余名额计。'
                : '独享：每个订阅独占一个出口资源（优先分配绑定上限为 1 的资源）。',
              '出口类型创建后不能修改。',
            ]
      }
      onConfirm={() =>
        plan
          ? api.adminCreatePlanVersion({ planId: plan.id, body: { ...v, name: v.name.trim(), city: v.city.trim() } })
          : api.adminCreatePlan({ body: { code, exitMode, version: { ...v, name: v.name.trim(), city: v.city.trim() } } })
      }
      onDone={() => {
        message.success(plan ? '已发布新版本' : '已创建草稿')
        void qc.invalidateQueries({ queryKey: qk.plans })
      }}
    >
      <div className="form-grid">
        {!plan && (
          <label className="field full">
            <span className="field-label">
              代码<em>小写字母、数字、连字符，创建后不能改</em>
            </span>
            <Input aria-label="套餐代码" className="mono" value={code} onChange={(e) => setCode(e.target.value)} placeholder="us-sea" />
          </label>
        )}
        {!plan ? (
          <div className="field full" style={{ marginTop: 12 }}>
            <span className="field-label" id="exit-mode-label">
              出口类型<em>创建后不能改</em>
            </span>
            <Segmented
              aria-labelledby="exit-mode-label"
              value={exitMode}
              onChange={(x) => setExitMode(x as ExitMode)}
              options={[
                { value: 'exclusive', label: '独享' },
                { value: 'shared', label: '共享' },
              ]}
            />
          </div>
        ) : (
          <div className="field full" style={{ marginBottom: 12 }}>
            <span className="field-label">出口类型<em>不能改</em></span>
            <span>
              <ExitModeChip mode={plan.exitMode} />
            </span>
          </div>
        )}
        <label className="field full" style={{ marginTop: plan ? 0 : 12 }}>
          <span className="field-label">名称</span>
          <Input aria-label="套餐名称" value={v.name} onChange={(e) => set('name', e.target.value)} />
        </label>
        <label className="field" style={{ marginTop: 12 }}>
          <span className="field-label">国家</span>
          <Select
            aria-label="国家"
            value={v.countryCode}
            onChange={(x) => setV((o) => ({ ...o, countryCode: x, country: COUNTRY[x] ?? x }))}
            options={Object.entries(COUNTRY).map(([value, label]) => ({ value, label }))}
          />
        </label>
        <label className="field" style={{ marginTop: 12 }}>
          <span className="field-label">城市</span>
          <Input aria-label="城市" value={v.city} onChange={(e) => set('city', e.target.value)} />
        </label>
        <label className="field" style={{ marginTop: 12 }}>
          <span className="field-label">月价（元）</span>
          <InputNumber aria-label="月价" min={0} precision={2} value={v.price.amount / 100} onChange={(x) => set('price', { amount: Math.round((x ?? 0) * 100), currency: 'CNY' })} style={{ width: '100%' }} />
        </label>
        <label className="field" style={{ marginTop: 12 }}>
          <span className="field-label">含流量（GB）</span>
          <InputNumber aria-label="含流量" min={0} value={v.includedBytes / GB} onChange={(x) => set('includedBytes', Math.round((x ?? 0) * GB))} style={{ width: '100%' }} />
        </label>
        <label className="field" style={{ marginTop: 12 }}>
          <span className="field-label">并发</span>
          <InputNumber aria-label="并发" min={1} value={v.concurrency} onChange={(x) => set('concurrency', x ?? 1)} style={{ width: '100%' }} />
        </label>
        <label className="field" style={{ marginTop: 12 }}>
          <span className="field-label">带宽（Mbps）</span>
          <InputNumber aria-label="带宽" min={1} value={v.bandwidthMbps} onChange={(x) => set('bandwidthMbps', x ?? 1)} style={{ width: '100%' }} />
        </label>
        <label className="field" style={{ marginTop: 12 }}>
          <span className="field-label">周期（天）</span>
          <InputNumber aria-label="周期" min={1} value={v.periodDays} onChange={(x) => set('periodDays', x ?? 30)} style={{ width: '100%' }} />
        </label>
      </div>
    </ConfirmAction>
  )
}

function SaleDialog({ name, current, submit, onClose, invalidate }: { name: string; current: SaleStatus; submit: (s: SaleStatus) => Promise<unknown>; onClose: () => void; invalidate: readonly unknown[] }) {
  const qc = useQueryClient()
  const { message } = App.useApp()
  const to: SaleStatus = current === 'on_sale' ? 'off_sale' : 'on_sale'
  return (
    <ConfirmAction
      open
      onClose={onClose}
      tone="warn"
      title={`${to === 'on_sale' ? '上架' : '下架'} · ${name}`}
      okText={to === 'on_sale' ? '上架' : '下架'}
      impacts={
        to === 'on_sale'
          ? [`状态 ${SALE[current].label} → 在售：用户端可以下单。`]
          : ['状态 在售 → 已下架：用户端不再展示、不能新下单。', '已有订阅照常使用，续费按订阅自己的套餐版本。']
      }
      onConfirm={() => submit(to)}
      onDone={() => {
        message.success('已更新')
        void qc.invalidateQueries({ queryKey: invalidate })
      }}
    />
  )
}

type PackDlg = { kind: 'version'; pack: AdminTrafficPack } | { kind: 'new' } | { kind: 'status'; pack: AdminTrafficPack } | null

function PackTable() {
  const api = useApi()
  const q = useQuery({ queryKey: qk.packs, queryFn: () => api.adminListTrafficPacks() })
  const [dlg, setDlg] = useState<PackDlg>(null)
  if (q.isPending) return <Loading />
  if (q.isError) return <LoadError error={q.error} retry={() => void q.refetch()} />
  return (
    <>
      <div className="dt-bar">
        <span className="muted small">流量包可单独购买，也可随续时长一起买。</span>
        <Button icon={<PlusOutlined />} onClick={() => setDlg({ kind: 'new' })}>
          新建流量包
        </Button>
      </div>
      <Table<AdminTrafficPack>
        className="plain-table"
        size="small"
        rowKey="id"
        pagination={false}
        scroll={{ x: 'max-content' }}
        dataSource={q.data}
        columns={[
          { title: '流量包', render: (_, p) => <span className="cell-stack"><span>{fmtBytes(p.current.bytes)}</span><span className="muted"><Mono>{p.code}</Mono> · v{p.current.version}</span></span> },
          { title: '价格', align: 'right', render: (_, p) => <span className="amount">{fmtMoney(p.current.price)}</span> },
          { title: '单价', align: 'right', render: (_, p) => `${fmtMoney({ amount: Math.round(p.current.price.amount / (p.current.bytes / GB)), currency: 'CNY' })}/GB` },
          { title: '状态', render: (_, p) => <State view={SALE[p.status]} /> },
          { title: '发布', render: (_, p) => fmtDateTime(p.current.createdAt) },
          {
            title: '操作',
            render: (_, p) => (
              <Space size={4}>
                <Button size="small" onClick={() => setDlg({ kind: 'version', pack: p })}>
                  发布新版本
                </Button>
                <Button size="small" onClick={() => setDlg({ kind: 'status', pack: p })}>
                  {p.status === 'on_sale' ? '下架' : '上架'}
                </Button>
              </Space>
            ),
          },
        ]}
      />
      {dlg?.kind === 'version' && <PackVersionDialog pack={dlg.pack} onClose={() => setDlg(null)} />}
      {dlg?.kind === 'new' && <PackVersionDialog onClose={() => setDlg(null)} />}
      {dlg?.kind === 'status' && (
        <SaleDialog
          name={fmtBytes(dlg.pack.current.bytes)}
          current={dlg.pack.status}
          onClose={() => setDlg(null)}
          submit={(status) => api.adminSetTrafficPackStatus({ trafficPackId: dlg.pack.id, body: { status } })}
          invalidate={qk.packs}
        />
      )}
    </>
  )
}

function PackVersionDialog({ pack, onClose }: { pack?: AdminTrafficPack; onClose: () => void }) {
  const api = useApi()
  const qc = useQueryClient()
  const { message } = App.useApp()
  const c = pack?.current
  const [code, setCode] = useState('')
  const [gb, setGb] = useState<number | null>(c ? c.bytes / GB : 10)
  const [yuan, setYuan] = useState<number | null>(c ? c.price.amount / 100 : 0)
  const bytes = Math.round((gb ?? 0) * GB)
  const price = Math.round((yuan ?? 0) * 100)
  const changed = !c || c.bytes !== bytes || c.price.amount !== price
  return (
    <ConfirmAction
      open
      onClose={onClose}
      tone="warn"
      title={pack ? `发布新版本 · ${fmtBytes(c!.bytes)}（当前 v${c!.version}）` : '新建流量包'}
      okText={pack ? `发布 v${c!.version + 1}` : '创建（草稿）'}
      ready={bytes > 0 && changed && (!!pack || /^[a-z0-9][a-z0-9-]{1,31}$/.test(code))}
      impacts={
        pack
          ? [`流量 ${fmtBytes(c!.bytes)} → ${fmtBytes(bytes)}；价格 ${fmtMoney(c!.price)} → ${fmtMoney({ amount: price, currency: 'CNY' })}`, '已下单的订单按原版本入账；新订单按新版本。']
          : ['新建为草稿，上架后用户端可购买。']
      }
      onConfirm={() =>
        pack
          ? api.adminCreateTrafficPackVersion({ trafficPackId: pack.id, body: { bytes, price: { amount: price, currency: 'CNY' } } })
          : api.adminCreateTrafficPack({ body: { code, version: { bytes, price: { amount: price, currency: 'CNY' } } } })
      }
      onDone={() => {
        message.success('已保存')
        void qc.invalidateQueries({ queryKey: qk.packs })
      }}
    >
      <div className="form-grid">
        {!pack && (
          <label className="field full">
            <span className="field-label">代码</span>
            <Input aria-label="流量包代码" className="mono" value={code} onChange={(e) => setCode(e.target.value)} placeholder="t100" />
          </label>
        )}
        <label className="field" style={{ marginTop: pack ? 0 : 12 }}>
          <span className="field-label">流量（GB）</span>
          <InputNumber aria-label="流量" min={1} value={gb} onChange={setGb} style={{ width: '100%' }} />
        </label>
        <label className="field" style={{ marginTop: pack ? 0 : 12 }}>
          <span className="field-label">价格（元）</span>
          <InputNumber aria-label="价格" min={0} precision={2} value={yuan} onChange={setYuan} style={{ width: '100%' }} />
        </label>
      </div>
    </ConfirmAction>
  )
}
