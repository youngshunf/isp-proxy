// 订单上的写操作：取消、重试履约、申请退款、退款复核。确认收款见 ConfirmPaymentDialog。
import { useQueryClient } from '@tanstack/react-query'
import { App, Input, InputNumber, Radio } from 'antd'
import { useEffect, useState } from 'react'
import type { AdminOrder, Refund } from '@isp/api-client/admin'
import { GB, fmtBytes, fmtMoney, fmtSpan } from '@isp/ui'
import { useApi } from '../app/api'
import { Mono } from './bits'
import { ConfirmAction } from './ConfirmAction'

function useRefreshAll() {
  const qc = useQueryClient()
  return () => void qc.invalidateQueries({ queryKey: ['admin'] })
}

export function CancelOrderDialog({ order, open, onClose }: { order: AdminOrder; open: boolean; onClose: () => void }) {
  const api = useApi()
  const refresh = useRefreshAll()
  const { message } = App.useApp()
  return (
    <ConfirmAction
      open={open}
      onClose={onClose}
      title={`取消订单 ${order.no}`}
      okText="取消订单"
      reasonLabel="取消原因"
      impacts={[
        <>收款状态 待核款 → 已取消；为这笔订单预留的上游资源放回库存。</>,
        <>之后若款项到账，只能按“晚到款”确认，进入人工处置。</>,
        <>
          订单金额 {fmtMoney(order.amount)}，用户 {order.user.email}。
        </>,
      ]}
      onConfirm={(reason) => api.adminCancelOrder({ orderId: order.id, body: { reason } })}
      onDone={() => {
        message.success('订单已取消')
        refresh()
      }}
    />
  )
}

export function RetryOrderDialog({ order, open, onClose }: { order: AdminOrder; open: boolean; onClose: () => void }) {
  const api = useApi()
  const refresh = useRefreshAll()
  const { message } = App.useApp()
  return (
    <ConfirmAction
      open={open}
      onClose={onClose}
      tone="warn"
      title={`重试履约 · ${order.no}`}
      okText="重试履约"
      impacts={[<>上次失败：{order.fulfillmentError ?? '—'}</>, <>重新分配端口并从库存挑选上游，经节点实测出口后交付。</>]}
      onConfirm={() => api.adminRetryOrderFulfillment({ orderId: order.id, body: {} })}
      onDone={() => {
        message.success('已重新开始履约')
        refresh()
      }}
    />
  )
}

export function RequestRefundDialog({ order, open, onClose }: { order: AdminOrder; open: boolean; onClose: () => void }) {
  const api = useApi()
  const refresh = useRefreshAll()
  const { message } = App.useApp()
  const [yuan, setYuan] = useState<number | null>(null)
  const [gb, setGb] = useState<number | null>(0)
  const [days, setDays] = useState<number | null>(0)
  useEffect(() => {
    if (open) {
      setYuan(order.amount.amount / 100)
      setGb(0)
      setDays(0)
    }
  }, [open, order.amount.amount])
  const cents = yuan === null ? 0 : Math.round(yuan * 100)
  const bytes = Math.round((gb ?? 0) * GB)
  const seconds = Math.round((days ?? 0) * 86_400)
  const valid = cents > 0 && cents <= order.amount.amount
  return (
    <ConfirmAction
      open={open}
      onClose={onClose}
      title={`申请退款 · ${order.no}`}
      okText="提交退款复核"
      reasonLabel="退款原因"
      ready={valid}
      impacts={[
        <>收款状态 已收款 → 退款审核中；需另一次“批准”才会退款并回收权益。</>,
        <>
          退款 <b>{fmtMoney({ amount: cents, currency: 'CNY' })}</b>（订单 {fmtMoney(order.amount)}）
          {order.subscriptionName ? `；批准后从“${order.subscriptionName}”回收 ${fmtBytes(bytes)} 流量、${fmtSpan(seconds)} 时长` : ''}。
        </>,
      ]}
      onConfirm={(reason) => api.adminRequestRefund({ orderId: order.id, body: { amount: { amount: cents, currency: 'CNY' }, reclaimBytes: bytes, reclaimSeconds: seconds, reason } })}
      onDone={() => {
        message.success('已提交退款复核')
        refresh()
      }}
    >
      <div className="form-grid">
        <label className="field full">
          <span className="field-label">退款金额（元）</span>
          <InputNumber aria-label="退款金额" min={0} max={order.amount.amount / 100} precision={2} value={yuan} onChange={setYuan} style={{ width: 200 }} />
        </label>
        <label className="field" style={{ marginTop: 12 }}>
          <span className="field-label">回收流量（GB）</span>
          <InputNumber aria-label="回收流量" min={0} precision={3} value={gb} onChange={setGb} style={{ width: '100%' }} />
        </label>
        <label className="field" style={{ marginTop: 12 }}>
          <span className="field-label">回收时长（天）</span>
          <InputNumber aria-label="回收时长" min={0} precision={2} value={days} onChange={setDays} style={{ width: '100%' }} />
        </label>
      </div>
    </ConfirmAction>
  )
}

export function DecideRefundDialog({ refund, orderNo, open, onClose }: { refund: Refund; orderNo?: string; open: boolean; onClose: () => void }) {
  const api = useApi()
  const refresh = useRefreshAll()
  const { message } = App.useApp()
  const [approve, setApprove] = useState(true)
  const [ref, setRef] = useState('')
  useEffect(() => {
    if (open) {
      setApprove(true)
      setRef('')
    }
  }, [open])
  return (
    <ConfirmAction
      open={open}
      onClose={onClose}
      tone={approve ? 'stop' : 'warn'}
      title={`退款复核${orderNo ? ` · ${orderNo}` : ''}`}
      okText={approve ? `批准退款 ${fmtMoney(refund.amount)}` : '驳回'}
      reasonLabel="复核说明"
      ready={!approve || ref.trim().length > 0}
      impacts={
        approve
          ? [
              <>
                记录退款 <b>{fmtMoney(refund.amount)}</b> 已退回（流水必填），订单收款状态改为“已退款”。
              </>,
              <>
                在账本写入回收条目：流量 −{fmtBytes(refund.reclaimBytes)}，时长 −{fmtSpan(refund.reclaimSeconds)}。
              </>,
              <>申请人 {refund.requestedBy.email}，原因：{refund.reason}</>,
            ]
          : [<>驳回退款，订单回到“已收款”，权益不变。</>, <>申请人 {refund.requestedBy.email}，原因：{refund.reason}</>]
      }
      onConfirm={(note) => api.adminDecideRefund({ refundId: refund.id, body: { approve, note, externalRef: approve ? ref.trim() : undefined } })}
      onDone={() => {
        message.success(approve ? '已批准退款' : '已驳回')
        refresh()
      }}
    >
      <Radio.Group value={approve} onChange={(e) => setApprove(e.target.value)} aria-label="复核结论">
        <Radio value={true}>批准</Radio>
        <Radio value={false}>驳回</Radio>
      </Radio.Group>
      {approve && (
        <label className="field">
          <span className="field-label">
            退款流水号<em>必填</em>
          </span>
          <Input aria-label="退款流水号" value={ref} onChange={(e) => setRef(e.target.value)} />
        </label>
      )}
      <span className="muted small">
        退款单 <Mono>{refund.id}</Mono>
      </span>
    </ConfirmAction>
  )
}
