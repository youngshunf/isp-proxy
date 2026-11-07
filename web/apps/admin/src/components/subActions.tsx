// 订阅详情上的写操作对话框：暂停/恢复、换接入、账本调整、划转、删除、重试开通。
// 每个对话框先列出“将会发生的变化”；带 Idempotency-Key 的接口在打开对话框时生成一次键，重试复用。
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { App, InputNumber, Radio, Select } from 'antd'
import { useEffect, useMemo, useState } from 'react'
import type { AdminSubscriptionDetail, Task } from '@isp/api-client/admin'
import { GB, fmtBytes, fmtSpan } from '@isp/ui'
import { newIdempotencyKey, qk, useApi } from '../app/api'
import { Mono } from './bits'
import { ConfirmAction } from './ConfirmAction'

type Sub = AdminSubscriptionDetail
interface DialogProps {
  sub: Sub
  open: boolean
  onClose: () => void
}

function useRefresh(subId: string) {
  const qc = useQueryClient()
  return () => {
    void qc.invalidateQueries({ queryKey: qk.sub(subId) })
    void qc.invalidateQueries({ queryKey: qk.subs, exact: true })
    void qc.invalidateQueries({ queryKey: qk.overview })
  }
}

/** 打开时生成一次幂等键，关闭后下次打开换新键。 */
function useIntentKey(open: boolean) {
  return useMemo(() => (open ? newIdempotencyKey() : ''), [open])
}

export function PauseDialog({ sub, open, onClose }: DialogProps) {
  const api = useApi()
  const refresh = useRefresh(sub.id)
  const { message } = App.useApp()
  return (
    <ConfirmAction
      open={open}
      onClose={onClose}
      title={`暂停订阅“${sub.name}”`}
      okText="暂停"
      reasonLabel="暂停原因"
      impacts={[
        <>立即拒绝新连接，现有 {sub.activeConns ?? '未知数量的'} 个连接在 1 秒内断开。</>,
        <>客户端显示“已暂停”，客户不能自助换接入。</>,
        <>续时长、买流量都不能解除暂停，需要管理员恢复。</>,
      ]}
      onConfirm={(reason) => api.adminPauseSubscription({ subscriptionId: sub.id, body: { reason } })}
      onDone={() => {
        message.success('已暂停')
        refresh()
      }}
    />
  )
}

export function ResumeDialog({ sub, open, onClose }: DialogProps) {
  const api = useApi()
  const refresh = useRefresh(sub.id)
  const { message } = App.useApp()
  const others = sub.blockers.filter((b) => b !== 'admin_paused')
  return (
    <ConfirmAction
      open={open}
      onClose={onClose}
      tone="warn"
      title={`恢复订阅“${sub.name}”`}
      okText="恢复"
      impacts={[
        <>解除管理员暂停（原因：{sub.pausedReason ?? '—'}）。</>,
        others.length ? <>仍有其他阻断：{others.length} 项，恢复后依然不可用，直到这些阻断解除。</> : <>没有其他阻断，恢复后立即可用。</>,
      ]}
      onConfirm={() => api.adminResumeSubscription({ subscriptionId: sub.id })}
      onDone={() => {
        message.success('已恢复')
        refresh()
      }}
    />
  )
}

export function ChangeAccessDialog({ sub, open, onClose, onTask }: DialogProps & { onTask: (t: Task) => void }) {
  const api = useApi()
  const refresh = useRefresh(sub.id)
  const key = useIntentKey(open)
  const [mode, setMode] = useState<'password' | 'ports_and_password'>('password')
  useEffect(() => {
    if (open) setMode('password')
  }, [open])
  const a = sub.access
  return (
    <ConfirmAction
      open={open}
      onClose={onClose}
      tone="warn"
      title={`换接入 · ${sub.name}`}
      okText={mode === 'password' ? '重置密码' : '更换端口与密码'}
      reasonLabel="原因（客户诉求或工单号）"
      impacts={[
        mode === 'password' ? (
          <>生成新密码，接入 v{a?.version} → v{(a?.version ?? 0) + 1}；端口不变。</>
        ) : (
          <>
            重新分配 HTTP / HTTPS / SOCKS5 端口并生成新密码（现为 <Mono>{a ? `${a.ports.http} / ${a.ports.https} / ${a.ports.socks5}` : '—'}</Mono>）。
          </>
        ),
        <>旧凭据在新凭据生效后立即作废，使用旧凭据的连接在 1 秒内断开。</>,
        <>出口 IP 不变；不占用客户每日自助换接入的次数（剩 {sub.quotas.accessChangesLeft} / {sub.quotas.accessChangesPerDay}）。</>,
      ]}
      onConfirm={(reason) => api.adminChangeAccess({ subscriptionId: sub.id, idempotencyKey: key, body: { mode, reason } })}
      onDone={(t) => {
        onTask(t as Task)
        refresh()
      }}
    >
      <div className="field">
        <span className="field-label">更换内容</span>
        <Radio.Group value={mode} onChange={(e) => setMode(e.target.value)}>
          <Radio value="password">只重置密码</Radio>
          <Radio value="ports_and_password">端口和密码一起换</Radio>
        </Radio.Group>
      </div>
    </ConfirmAction>
  )
}

const SOURCE_LABEL = { adjust: '调整（可正可负）', gift: '赠送', compensation: '补偿' } as const

export function LedgerDialog({ sub, open, onClose }: DialogProps) {
  const api = useApi()
  const refresh = useRefresh(sub.id)
  const qc = useQueryClient()
  const { message } = App.useApp()
  const key = useIntentKey(open)
  const ledger = useQuery({ queryKey: qk.subPart(sub.id, 'ledger'), queryFn: () => api.adminListLedger({ subscriptionId: sub.id }), enabled: open })
  const [source, setSource] = useState<'adjust' | 'gift' | 'compensation'>('compensation')
  const [kind, setKind] = useState<'traffic' | 'time'>('traffic')
  const [qty, setQty] = useState<number | null>(null)
  useEffect(() => {
    if (open) {
      setSource('compensation')
      setKind('traffic')
      setQty(null)
    }
  }, [open])
  // 流量按 GB 录入，时长按天录入；提交时换成字节 / 秒（账本整数）
  const amount = qty === null ? null : Math.round(kind === 'traffic' ? qty * GB : qty * 86_400)
  const negativeNotAllowed = source !== 'adjust' && amount !== null && amount < 0
  const before = ledger.data ? (kind === 'traffic' ? ledger.data.entitledBytesTotal : ledger.data.entitledSecondsTotal) : null
  const after = before !== null && amount !== null ? before + amount : null
  const fmt = (v: number) => (kind === 'traffic' ? fmtBytes(v) : fmtSpan(v))
  return (
    <ConfirmAction
      open={open}
      onClose={onClose}
      tone="warn"
      title={`账本入账 · ${sub.name}`}
      okText="写入账本"
      reasonLabel="原因"
      ready={amount !== null && amount !== 0 && !negativeNotAllowed && (after === null || after >= 0)}
      impacts={[
        <>新增一条独立的账本条目（{SOURCE_LABEL[source]}），不改写历史条目。</>,
        after !== null && before !== null ? (
          <>
            {kind === 'traffic' ? '流量' : '时长'}合计 {fmt(before)} → <b>{fmt(after)}</b>
            {after < 0 && <span className="field-error">（不能小于 0）</span>}
          </>
        ) : (
          <>填写数量后显示变化前后的合计。</>
        ),
        <>客户在用户端可见这条记录与原因。</>,
      ]}
      onConfirm={(reason) => api.adminCreateLedgerEntry({ subscriptionId: sub.id, idempotencyKey: key, body: { sourceType: source, entitlementKind: kind, amount: amount!, reason } })}
      onDone={() => {
        message.success('已入账')
        void qc.invalidateQueries({ queryKey: qk.subPart(sub.id, 'ledger') })
        refresh()
      }}
    >
      <div className="form-grid">
        <label className="field">
          <span className="field-label">类型</span>
          <Select aria-label="入账类型" value={source} onChange={setSource} options={Object.entries(SOURCE_LABEL).map(([value, label]) => ({ value, label }))} />
        </label>
        <label className="field">
          <span className="field-label">项目</span>
          <Select
            aria-label="入账项目"
            value={kind}
            onChange={(v) => {
              setKind(v)
              setQty(null)
            }}
            options={[
              { value: 'traffic', label: '流量' },
              { value: 'time', label: '时长' },
            ]}
          />
        </label>
        <label className="field full" style={{ marginTop: 12 }}>
          <span className="field-label">
            数量（{kind === 'traffic' ? 'GB，1 GB = 10⁹ 字节' : '天'}）<em>{source === 'adjust' ? '扣回填负数' : '只能为正数'}</em>
          </span>
          <InputNumber aria-label="入账数量" value={qty} onChange={setQty} step={kind === 'traffic' ? 1 : 1} precision={kind === 'traffic' ? 3 : 2} style={{ width: 220 }} status={negativeNotAllowed ? 'error' : undefined} />
          {negativeNotAllowed && <span className="field-error">赠送与补偿只能为正数；扣回请选“调整”。</span>}
        </label>
      </div>
    </ConfirmAction>
  )
}

export function TransferDialog({ sub, open, onClose }: DialogProps) {
  const api = useApi()
  const qc = useQueryClient()
  const { message } = App.useApp()
  const key = useIntentKey(open)
  const siblings = useQuery({
    queryKey: [...qk.subs, 'siblings', sub.user.id],
    queryFn: () => api.adminListSubscriptions({ query: { userId: sub.user.id, limit: 100 } }),
    enabled: open,
  })
  const [to, setTo] = useState<string>()
  useEffect(() => {
    if (open) setTo(undefined)
  }, [open])
  const targets = (siblings.data?.items ?? []).filter((s) => s.id !== sub.id)
  const target = targets.find((t) => t.id === to)
  const remaining = Math.max(0, sub.traffic.remainingBytes)
  return (
    <ConfirmAction
      open={open}
      onClose={onClose}
      title={`划转剩余流量 · ${sub.name}`}
      okText="划转"
      reasonLabel="原因"
      ready={!!to && remaining > 0}
      impacts={[
        <>
          本订阅剩余 <b>{fmtBytes(remaining)}</b> 全额划出，余额归零（账本记“划出”）。
        </>,
        target ? (
          <>
            目标“{target.name}”剩余 {fmtBytes(Math.max(0, target.traffic.remainingBytes))} → <b>{fmtBytes(Math.max(0, target.traffic.remainingBytes) + remaining)}</b>（账本记“划入”）。
          </>
        ) : (
          <>只能划给同一用户（{sub.user.email}）名下的其他订阅。</>
        ),
        <>时长不划转；本订阅流量归零后按“流量耗尽”停用。</>,
      ]}
      onConfirm={(reason) => api.adminTransferBalance({ idempotencyKey: key, body: { fromSubscriptionId: sub.id, toSubscriptionId: to!, reason } })}
      onDone={() => {
        message.success('已划转')
        void qc.invalidateQueries({ queryKey: ['admin', 'subs'] })
        void qc.invalidateQueries({ queryKey: qk.overview })
      }}
    >
      <label className="field">
        <span className="field-label">划给</span>
        <Select
          aria-label="目标订阅"
          value={to}
          onChange={setTo}
          loading={siblings.isPending}
          placeholder={targets.length || siblings.isPending ? '选择同一用户的其他订阅' : '该用户没有其他订阅'}
          options={targets.map((t) => ({ value: t.id, label: `${t.name} · 剩余 ${fmtBytes(Math.max(0, t.traffic.remainingBytes))}`, disabled: t.lifecycle === 'deleted' }))}
        />
      </label>
      {remaining <= 0 && <span className="field-error">本订阅没有可划转的剩余流量。</span>}
    </ConfirmAction>
  )
}

export function DeleteDialog({ sub, open, onClose }: DialogProps) {
  const api = useApi()
  const refresh = useRefresh(sub.id)
  const { message } = App.useApp()
  const [ack, setAck] = useState(false)
  useEffect(() => {
    if (open) setAck(false)
  }, [open])
  const remaining = Math.max(0, sub.traffic.remainingBytes)
  return (
    <ConfirmAction
      open={open}
      onClose={onClose}
      title={`删除订阅“${sub.name}”`}
      okText="删除订阅"
      reasonLabel="删除原因"
      ready={remaining === 0 || ack}
      impacts={[
        <>撤销当前接入（v{sub.access?.version ?? '—'}），解除出口 {sub.exit?.verifiedIp ? <Mono>{sub.exit.verifiedIp}</Mono> : '—'}；上游进入冷却期。</>,
        <>订阅从客户列表消失，记录与账本保留，不能恢复。</>,
        remaining > 0 ? <b>还有 {fmtBytes(remaining)} 剩余流量：删除前请先划转或退款，或确认人工处置。</b> : <>没有剩余流量。</>,
      ]}
      onConfirm={(reason) => api.adminDeleteSubscription({ subscriptionId: sub.id, body: { reason, acknowledgeRemainingTraffic: ack } })}
      onDone={() => {
        message.success('订阅已删除')
        refresh()
      }}
    >
      {remaining > 0 && (
        <label className="field" style={{ flexDirection: 'row', alignItems: 'center', gap: 8 }}>
          <input type="checkbox" checked={ack} onChange={(e) => setAck(e.target.checked)} />
          <span>我确认剩余 {fmtBytes(remaining)} 已另行人工处置</span>
        </label>
      )}
    </ConfirmAction>
  )
}

export function RetryFulfillDialog({ sub, open, onClose, onTask }: DialogProps & { onTask: (t: Task) => void }) {
  const api = useApi()
  const refresh = useRefresh(sub.id)
  return (
    <ConfirmAction
      open={open}
      onClose={onClose}
      tone="warn"
      title={`重试开通 · ${sub.name}`}
      okText="重试开通"
      impacts={[
        <>重新分配端口并从库存挑选一个可售上游，经节点实测出口后启用。</>,
        <>上一次失败原因：{sub.lifecycleReason ?? '—'}</>,
      ]}
      onConfirm={() => api.adminRetryFulfillment({ subscriptionId: sub.id, body: {} })}
      onDone={(t) => {
        onTask(t as Task)
        refresh()
      }}
    />
  )
}
