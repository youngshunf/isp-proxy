// 订阅列表（R-12）：按状态、阻断原因、节点、到期时间筛选；筛选写在地址栏，概览的计数直接链到这里。
import { PlusOutlined } from '@ant-design/icons'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { App, Button, Input, InputNumber, Modal, Radio, Select } from 'antd'
import { useMemo, useRef, useState } from 'react'
import { useNavigate } from 'react-router'
import type { AdminLifecycle, AdminSubscription, Blocker } from '@isp/api-client/admin'
import { Plate, fmtBytes, fmtMoney } from '@isp/ui'
import { errorText, newIdempotencyKey, qk, useApi } from '../app/api'
import { Callout, PageHead } from '../components/bits'
import { DataTable, arr, bool, str, type FilterDef } from '../components/DataTable'
import { subColumns } from '../components/subCells'
import { BLOCKER, LIFECYCLE, options } from '../lib/status'

export default function SubscriptionsPage() {
  const api = useApi()
  const nodes = useQuery({ queryKey: qk.nodes, queryFn: () => api.adminListNodes() })
  const [creating, setCreating] = useState(false)

  const filters = useMemo<FilterDef[]>(
    () => [
      { key: 'q', label: '搜索', type: 'search', placeholder: '名称、订阅 ID、用户邮箱、出口 IP、用户名' , width: 300 },
      { key: 'lifecycle', label: '生命周期', type: 'multi', options: options(LIFECYCLE) },
      { key: 'blocker', label: '阻断原因', type: 'select', options: options(BLOCKER) },
      { key: 'usable', label: '可用', type: 'bool', options: [{ value: 'true', label: '可用' }, { value: 'false', label: '不可用' }], width: 110 },
      { key: 'nodeId', label: '节点', type: 'select', options: (nodes.data ?? []).map((n) => ({ value: n.id, label: n.name })), width: 130 },
      { key: 'expiresBefore', label: '到期早于', type: 'date' },
      { key: 'userId', label: '用户 ID', type: 'search', width: 200 },
    ],
    [nodes.data],
  )
  const columns = useMemo(() => subColumns({ withUser: true }), [])

  return (
    <>
      <PageHead
        title="订阅"
        sub="一个订阅 = 一个出口 IP + 一套接入。状态取服务端排序后的第一个阻断原因，其余列在下方。"
        extra={
          <Button icon={<PlusOutlined />} onClick={() => setCreating(true)}>
            代建订阅
          </Button>
        }
      />
      <Plate>
        <DataTable<AdminSubscription>
          id="subscriptions"
          label="订阅列表"
          columns={columns}
          filters={filters}
          queryKey={qk.subs}
          rowKey={(s) => s.id}
          rowClassName={(s) => (s.blockers.includes('fulfill_failed') || s.blockers.includes('dataplane') ? 'row-flag' : '')}
          fetchPage={({ filters: f, cursor, limit }) =>
            api.adminListSubscriptions({
              query: {
                q: str(f, 'q'),
                lifecycle: arr<AdminLifecycle>(f, 'lifecycle'),
                blocker: str(f, 'blocker') as Blocker | undefined,
                usable: bool(f, 'usable'),
                nodeId: str(f, 'nodeId'),
                userId: str(f, 'userId'),
                expiresBefore: str(f, 'expiresBefore'),
                cursor,
                limit,
              },
            })
          }
        />
      </Plate>
      <CreateSubscriptionDialog open={creating} onClose={() => setCreating(false)} />
    </>
  )
}

function CreateSubscriptionDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const api = useApi()
  const qc = useQueryClient()
  const navigate = useNavigate()
  const { message } = App.useApp()
  const [userQ, setUserQ] = useState('')
  const users = useQuery({
    queryKey: [...qk.users, 'pick', userQ],
    queryFn: () => api.adminListUsers({ query: { q: userQ || undefined, status: 'active', limit: 20 } }),
    enabled: open,
  })
  const plans = useQuery({ queryKey: qk.plans, queryFn: () => api.adminListPlans(), enabled: open })
  const [userId, setUserId] = useState<string>()
  const [planId, setPlanId] = useState<string>()
  const [months, setMonths] = useState<number | null>(1)
  const [name, setName] = useState('')
  const [method, setMethod] = useState<'manual_transfer' | 'admin_gift'>('admin_gift')
  const [ref, setRef] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const key = useRef(newIdempotencyKey())

  const plan = plans.data?.find((p) => p.id === planId)
  const amount = plan && months ? (method === 'admin_gift' ? 0 : plan.current.price.amount * months) : null
  const ready = !!userId && !!plan && !!months && (method === 'admin_gift' || ref.trim().length > 0) && !busy

  const submit = async () => {
    if (!ready || !plan || amount === null) return
    setBusy(true)
    setError(null)
    try {
      const r = await api.adminCreateSubscription({
        idempotencyKey: key.current,
        body: {
          userId: userId!,
          planId: plan.id,
          planVersionId: plan.current.id,
          months: months!,
          name: name.trim() || undefined,
          payment: { method, amount: { amount, currency: 'CNY' }, externalRef: method === 'admin_gift' ? 'admin-gift' : ref.trim(), lateArrival: false },
        },
      })
      message.success(`已代建订阅，订单 ${r.order.no}`)
      void qc.invalidateQueries({ queryKey: ['admin'] })
      key.current = newIdempotencyKey()
      onClose()
      navigate(`/subscriptions/${r.subscription.id}`)
    } catch (e) {
      setError(errorText(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal
      open={open}
      title="代建订阅"
      onCancel={() => !busy && onClose()}
      onOk={submit}
      okText="创建并开始履约"
      okButtonProps={{ disabled: !ready, loading: busy }}
      cancelText="取消"
      destroyOnHidden
      width={600}
    >
      <div className="confirm-body">
        <Callout tone="warn">
          <div className="impact-title">将会发生的变化</div>
          <ul className="impact-list">
            <li>为该用户生成一笔“新购”订单，并直接记一条收款记录（{method === 'admin_gift' ? '管理员赠送，金额 ¥0' : '银行转账'}）。</li>
            <li>按所选套餐当前版本入账时长与流量，随即开始履约（分配端口与上游出口）。</li>
          </ul>
        </Callout>
        <div className="form-grid">
          <label className="field full">
            <span className="field-label">用户</span>
            <Select
              aria-label="用户"
              showSearch={{ filterOption: false, onSearch: setUserQ }}
              placeholder="输入邮箱搜索"
              value={userId}
              onChange={setUserId}
              loading={users.isFetching}
              options={(users.data?.items ?? []).map((u) => ({ value: u.id, label: `${u.email}${u.verified ? '' : '（未验证）'}` }))}
              notFoundContent={users.isFetching ? '搜索中…' : '没有匹配的可用用户'}
            />
          </label>
          <label className="field full">
            <span className="field-label">套餐</span>
            <Select
              aria-label="套餐"
              value={planId}
              onChange={setPlanId}
              placeholder="选择在售套餐"
              options={(plans.data ?? [])
                .filter((p) => p.status === 'on_sale')
                .map((p) => ({
                  value: p.id,
                  label: `${p.current.name} · ${fmtMoney(p.current.price)}/月 · ${fmtBytes(p.current.includedBytes)} · 库存 ${p.stock.available}`,
                  disabled: p.stock.available === 0,
                }))}
            />
          </label>
          <label className="field">
            <span className="field-label">月数</span>
            <InputNumber aria-label="月数" min={1} max={12} value={months} onChange={setMonths} style={{ width: '100%' }} />
          </label>
          <label className="field">
            <span className="field-label">名称（可选）</span>
            <Input aria-label="名称" value={name} onChange={(e) => setName(e.target.value)} placeholder={plan?.current.name} maxLength={40} />
          </label>
          <div className="field full" style={{ marginTop: 12 }}>
            <span className="field-label">收款</span>
            <Radio.Group value={method} onChange={(e) => setMethod(e.target.value)}>
              <Radio value="admin_gift">管理员赠送（¥0）</Radio>
              <Radio value="manual_transfer">已线下收款</Radio>
            </Radio.Group>
          </div>
          {method === 'manual_transfer' && (
            <label className="field full" style={{ marginTop: 12 }}>
              <span className="field-label">
                外部收款参考（流水号）<em>必填</em>
              </span>
              <Input aria-label="外部收款参考" value={ref} onChange={(e) => setRef(e.target.value)} />
            </label>
          )}
        </div>
        {amount !== null && (
          <div className="pay-head">
            <div>
              <span className="muted">订单金额</span>
              <b className="pay-amount">{fmtMoney({ amount, currency: 'CNY' })}</b>
            </div>
            <div className="muted small">
              {plan!.current.name} v{plan!.current.version} × {months} 个月
            </div>
          </div>
        )}
        {error && <Callout tone="stop">{error}</Callout>}
      </div>
    </Modal>
  )
}
