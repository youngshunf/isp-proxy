// 规则版本（R-12）：周期、保留期、冷却、换接入 / 换出口次数、未付保留、收款说明与下单快照。
// 规则只追加新版本、不改旧版本；订单记录下单时的规则版本。收款说明未配置时用户端拒绝下单。
import { PlusOutlined } from '@ant-design/icons'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { App, Button, DatePicker, Input, InputNumber, Select, Table } from 'antd'
import dayjs from 'dayjs'
import { useState } from 'react'
import type { RulesInput, RulesVersion } from '@isp/api-client/admin'
import { Plate, fmtDateTime } from '@isp/ui'
import { qk, useApi, useServerNow } from '../app/api'
import { Callout, Facts, LoadError, Loading, Mono, PageHead, State } from '../components/bits'
import { ConfirmAction } from '../components/ConfirmAction'

const paid = (r: RulesInput) => r.payment.account.trim().length > 0

const FIELD_LABEL: Partial<Record<keyof RulesInput, string>> = {
  periodDays: '周期',
  retentionDays: '保留期',
  portCooldownDays: '端口冷却',
  resourceCooldownDays: '上游冷却',
  accessChangesPerWindow: '换接入次数',
  accessChangeWindowHours: '换接入窗口',
  freeExitChangesPerWindow: '免费换出口',
  exitChangeWindowDays: '换出口窗口',
  unpaidHoldHours: '未付保留',
  maxUnpaidNewOrders: '未付新购上限',
  monthOptions: '月数选项',
  payment: '收款说明',
}

export default function RulesPage() {
  const api = useApi()
  const now = useServerNow()
  const q = useQuery({ queryKey: qk.rules, queryFn: () => api.adminListRules() })
  const [creating, setCreating] = useState(false)
  if (q.isPending) return <Loading rows={6} />
  if (q.isError) return <LoadError error={q.error} retry={() => void q.refetch()} />
  const rules = q.data
  const current = rules.find((r) => Date.parse(r.effectiveFrom) <= now())
  return (
    <>
      <PageHead
        title="规则"
        sub="规则只能发布新版本，不能修改历史版本；每笔订单记录下单时生效的版本。"
        extra={
          <Button type="primary" icon={<PlusOutlined />} onClick={() => setCreating(true)}>
            发布新版本
          </Button>
        }
      />
      <div className="stack">
        {current && !paid(current) && (
          <Callout tone="stop">当前生效的 {current.version} 没有配置收款说明：用户端下单会被拒绝。请发布带收款说明的新版本。</Callout>
        )}
        {current && (
          <Plate title={`当前生效：${current.version}`} extra={<span className="muted small">自 {fmtDateTime(current.effectiveFrom)}</span>}>
            <div className="plate-pad">
              <RulesFacts r={current} />
            </div>
          </Plate>
        )}
        <Plate title="版本历史">
          <Table<RulesVersion>
            className="plain-table"
            size="small"
            rowKey="version"
            pagination={false}
            scroll={{ x: 'max-content' }}
            dataSource={rules}
            style={{ marginTop: 12 }}
            expandable={{ expandedRowRender: (r) => <div style={{ padding: '4px 8px' }}><RulesFacts r={r} /></div> }}
            columns={[
              {
                title: '版本',
                dataIndex: 'version',
                render: (v: string, r) => (
                  <span className="cell-stack">
                    <Mono>{v}</Mono>
                    {r === current && <span className="state tone-ok small">当前生效</span>}
                  </span>
                ),
              },
              { title: '生效', dataIndex: 'effectiveFrom', render: (v: string) => fmtDateTime(v) },
              { title: '周期 / 保留', render: (_, r) => `${r.periodDays} 天 / ${r.retentionDays} 天` },
              { title: '换接入', render: (_, r) => `${r.accessChangesPerWindow} 次 / ${r.accessChangeWindowHours} 小时` },
              { title: '免费换出口', render: (_, r) => `${r.freeExitChangesPerWindow} 次 / ${r.exitChangeWindowDays} 天` },
              { title: '未付保留', render: (_, r) => `${r.unpaidHoldHours} 小时 · 最多 ${r.maxUnpaidNewOrders} 笔` },
              {
                title: '收款说明',
                render: (_, r) => (paid(r) ? <State view={{ label: '已配置', tone: 'ok', shape: 'filled' }} strong={false} /> : <State view={{ label: '未配置（拒绝下单）', tone: 'stop', shape: 'hollow' }} strong={false} />),
              },
              { title: '发布人', render: (_, r) => r.createdBy?.email ?? '系统' },
            ]}
          />
        </Plate>
      </div>
      {creating && <NewRulesDialog base={rules[0]!} onClose={() => setCreating(false)} />}
    </>
  )
}

function RulesFacts({ r }: { r: RulesInput }) {
  return (
    <Facts
      cols={2}
      items={[
        ['周期', `${r.periodDays} 天`],
        ['保留期', `${r.retentionDays} 天`],
        ['端口冷却', `${r.portCooldownDays} 天`],
        ['上游冷却', `${r.resourceCooldownDays} 天`],
        ['换接入', `每 ${r.accessChangeWindowHours} 小时 ${r.accessChangesPerWindow} 次`],
        ['免费换出口', `每 ${r.exitChangeWindowDays} 天 ${r.freeExitChangesPerWindow} 次`],
        ['未付保留', `${r.unpaidHoldHours} 小时`],
        ['未付新购上限', `${r.maxUnpaidNewOrders} 笔`],
        ['月数选项', r.monthOptions.join('、')],
        ['收款户名', r.payment.payee || null],
        ['开户行', r.payment.bank || null],
        ['账号', r.payment.account ? <Mono key="a">{r.payment.account}</Mono> : <span key="a" className="state tone-stop">未配置</span>],
        ['付款说明', r.payment.note || null],
        ['下单快照', r.snapshotLines.length ? <ul key="s" style={{ margin: 0, paddingLeft: 18 }}>{r.snapshotLines.map((l) => <li key={l}>{l}</li>)}</ul> : null],
      ]}
    />
  )
}

function nextVersion(prev: string, now: number): string {
  const d = new Date(now + 8 * 3600_000)
  const day = `R-${d.getUTCFullYear()}-${String(d.getUTCMonth() + 1).padStart(2, '0')}-${String(d.getUTCDate()).padStart(2, '0')}`
  if (!prev.startsWith(day)) return day
  const n = Number(prev.split('.')[1] ?? 0)
  return `${day}.${n + 1}`
}

function NewRulesDialog({ base, onClose }: { base: RulesVersion; onClose: () => void }) {
  const api = useApi()
  const qc = useQueryClient()
  const now = useServerNow()
  const { message } = App.useApp()
  const { createdAt: _c, createdBy: _b, ...rest } = base
  void _c
  void _b
  const [r, setR] = useState<RulesInput>({ ...rest, version: nextVersion(base.version, now()), effectiveFrom: new Date(now() + 10 * 60_000).toISOString() })
  const [lines, setLines] = useState(base.snapshotLines.join('\n'))
  const set = <K extends keyof RulesInput>(k: K, v: RulesInput[K]) => setR((o) => ({ ...o, [k]: v }))
  const setPay = (k: keyof RulesInput['payment'], v: string) => setR((o) => ({ ...o, payment: { ...o.payment, [k]: v } }))
  const num = (k: keyof RulesInput, label: string, unit: string) => (
    <label className="field" style={{ marginTop: 12 }}>
      <span className="field-label">
        {label}（{unit}）
      </span>
      <InputNumber aria-label={label} min={0} value={r[k] as number} onChange={(v) => set(k, (v ?? 0) as never)} style={{ width: '100%' }} />
    </label>
  )
  const changes = (Object.keys(rest) as (keyof RulesInput)[])
    .filter((k) => k !== 'version' && k !== 'effectiveFrom' && k !== 'snapshotLines' && JSON.stringify(rest[k]) !== JSON.stringify(r[k]))
    .map((k) => FIELD_LABEL[k] ?? k)
  const body: RulesInput = { ...r, snapshotLines: lines.split('\n').map((l) => l.trim()).filter(Boolean) }
  return (
    <ConfirmAction
      open
      onClose={onClose}
      tone="warn"
      title={`发布规则 ${r.version}`}
      okText="发布"
      ready={/^R-\d{4}-\d{2}-\d{2}(\.\d+)?$/.test(r.version) && r.monthOptions.length > 0}
      impacts={[
        `自 ${fmtDateTime(r.effectiveFrom)} 起，新订单使用这一版；已有订单保留下单时的版本。`,
        changes.length ? `相对 ${base.version} 改动了 ${changes.length} 项：${changes.join('、')}` : `与 ${base.version} 内容相同（只改了快照或版本号）`,
        paid(body) ? '收款说明已配置。' : '收款说明未配置：这一版生效后用户端下单会被拒绝。',
      ]}
      onConfirm={() => api.adminCreateRules({ body })}
      onDone={() => {
        message.success('已发布')
        void qc.invalidateQueries({ queryKey: qk.rules })
      }}
    >
      <div className="form-grid">
        <label className="field">
          <span className="field-label">版本号</span>
          <Input aria-label="版本号" className="mono" value={r.version} onChange={(e) => set('version', e.target.value)} />
        </label>
        <label className="field">
          <span className="field-label">生效时间</span>
          <DatePicker aria-label="生效时间" showTime value={dayjs(r.effectiveFrom)} onChange={(d) => d && set('effectiveFrom', d.toISOString())} style={{ width: '100%' }} />
        </label>
        {num('periodDays', '周期', '天')}
        {num('retentionDays', '保留期', '天')}
        {num('portCooldownDays', '端口冷却', '天')}
        {num('resourceCooldownDays', '上游冷却', '天')}
        {num('accessChangesPerWindow', '换接入次数', '次')}
        {num('accessChangeWindowHours', '换接入窗口', '小时')}
        {num('freeExitChangesPerWindow', '免费换出口', '次')}
        {num('exitChangeWindowDays', '换出口窗口', '天')}
        {num('unpaidHoldHours', '未付保留', '小时')}
        {num('maxUnpaidNewOrders', '未付新购上限', '笔')}
        <label className="field full" style={{ marginTop: 12 }}>
          <span className="field-label">月数选项</span>
          <Select
            aria-label="月数选项"
            mode="multiple"
            value={r.monthOptions}
            onChange={(v: number[]) => set('monthOptions', [...v].sort((a, b) => a - b))}
            options={[1, 2, 3, 6, 12].map((m) => ({ value: m, label: `${m} 个月` }))}
          />
        </label>
      </div>
      <div className="field-label" style={{ marginTop: 4 }}>
        收款说明<em>用户下单后看到的转账信息（示意数据）</em>
      </div>
      <div className="form-grid">
        <label className="field">
          <span className="field-label">户名</span>
          <Input aria-label="收款户名" value={r.payment.payee} onChange={(e) => setPay('payee', e.target.value)} />
        </label>
        <label className="field">
          <span className="field-label">开户行</span>
          <Input aria-label="开户行" value={r.payment.bank} onChange={(e) => setPay('bank', e.target.value)} />
        </label>
        <label className="field full" style={{ marginTop: 12 }}>
          <span className="field-label">账号</span>
          <Input aria-label="收款账号" className="mono" value={r.payment.account} onChange={(e) => setPay('account', e.target.value)} status={paid(r) ? undefined : 'warning'} />
        </label>
        <label className="field full" style={{ marginTop: 12 }}>
          <span className="field-label">付款说明</span>
          <Input.TextArea aria-label="付款说明" value={r.payment.note} onChange={(e) => setPay('note', e.target.value)} autoSize={{ minRows: 2 }} />
        </label>
        <label className="field full" style={{ marginTop: 12 }}>
          <span className="field-label">下单快照<em>每行一条，下单时写进订单</em></span>
          <Input.TextArea aria-label="下单快照" value={lines} onChange={(e) => setLines(e.target.value)} autoSize={{ minRows: 3 }} />
        </label>
      </div>
    </ConfirmAction>
  )
}
