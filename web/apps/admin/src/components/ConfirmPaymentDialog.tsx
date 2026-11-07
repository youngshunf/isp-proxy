// 人工确认收款（docs/01 §5、docs/05 R-12 订单）：
// 框内先把订单号与应收金额写大，录入的到账金额与流水号逐项核对；同一订单只有一份收款记录，
// 连点或重试由 ConfirmAction 的 ref 挡住，服务端再按“同一记录返回原结果、内容不同 409”兜底。
import { useQueryClient } from '@tanstack/react-query'
import { App, Checkbox, Input, InputNumber, Radio } from 'antd'
import { useEffect, useState } from 'react'
import type { AdminOrder } from '@isp/api-client/admin'
import { fmtDateTime, fmtMoney } from '@isp/ui'
import { useApi } from '../app/api'
import { ORDER_KIND } from '../lib/status'
import { Callout, Mono } from './bits'
import { ConfirmAction } from './ConfirmAction'

export function ConfirmPaymentDialog({ order, open, onClose }: { order: AdminOrder; open: boolean; onClose: () => void }) {
  const api = useApi()
  const qc = useQueryClient()
  const { message } = App.useApp()
  const [method, setMethod] = useState<AdminOrder['paymentMethod']>('manual_transfer')
  const [yuan, setYuan] = useState<number | null>(order.amount.amount / 100)
  const [ref, setRef] = useState('')
  const [note, setNote] = useState('')
  const late = order.payment === 'cancelled'
  const [lateAck, setLateAck] = useState(false)

  useEffect(() => {
    if (open) {
      setMethod('manual_transfer')
      setYuan(order.amount.amount / 100)
      setRef('')
      setNote('')
      setLateAck(false)
    }
  }, [open, order.amount.amount])

  const cents = yuan === null ? null : Math.round(yuan * 100)
  const mismatch = cents !== null && cents !== order.amount.amount
  const ready = cents !== null && !mismatch && ref.trim().length > 0 && (!late || lateAck)

  const impacts = [
    <>
      订单 <Mono>{order.no}</Mono> 的收款状态改为“已收款”，记录确认人与流水号，<b>之后不能再改</b>。
    </>,
    order.kind === 'new' ? (
      <>开始履约：分配节点端口与上游出口，生成接入信息。</>
    ) : (
      <>
        立即给订阅“{order.subscriptionName}”入账：{order.lines.map((l) => `${l.label} ${l.detail}`).join('，')}。
      </>
    ),
  ]
  if (late) impacts.push(<>订单已取消，这是晚到款：确认后不自动履约，进入“重新履约或退款”的人工处置。</>)

  return (
    <ConfirmAction
      open={open}
      onClose={onClose}
      tone="warn"
      title="确认收款"
      okText={`确认收到 ${fmtMoney(order.amount)}`}
      impacts={impacts}
      ready={ready}
      onConfirm={() =>
        api.adminConfirmPayment({
          orderId: order.id,
          body: {
            method,
            amount: { amount: cents!, currency: 'CNY' },
            externalRef: ref.trim(),
            note: note.trim() || undefined,
            lateArrival: late,
          },
        })
      }
      onDone={() => {
        message.success(`订单 ${order.no} 已确认收款`)
        void qc.invalidateQueries({ queryKey: ['admin'] })
      }}
    >
      <div className="pay-head" aria-label="待确认的订单">
        <div>
          <span className="muted">订单号</span>
          <Mono className="pay-no">{order.no}</Mono>
        </div>
        <div>
          <span className="muted">应收金额</span>
          <b className="pay-amount">{fmtMoney(order.amount)}</b>
        </div>
        <div className="pay-meta">
          {ORDER_KIND[order.kind]} · {order.user.email} · 付款备注码 <Mono>{order.payRef}</Mono>
          {order.userClaimedPaidAt && <> · 用户 {fmtDateTime(order.userClaimedPaidAt)} 标记已付款</>}
        </div>
      </div>
      <div className="field">
        <span className="field-label">收款方式</span>
        <Radio.Group value={method} onChange={(e) => setMethod(e.target.value)} aria-label="收款方式">
          <Radio value="manual_transfer">银行转账</Radio>
          <Radio value="admin_gift">管理员赠送（不收款）</Radio>
        </Radio.Group>
      </div>
      <label className="field">
        <span className="field-label">
          实际到账金额（元）<em>必须等于应收金额</em>
        </span>
        <InputNumber
          aria-label="实际到账金额"
          value={yuan}
          onChange={(v) => setYuan(v)}
          min={0}
          precision={2}
          step={1}
          style={{ width: 200 }}
          status={mismatch ? 'error' : undefined}
        />
        {mismatch && <span className="field-error">与应收 {fmtMoney(order.amount)} 不一致：核对流水，金额不符请联系用户而不是改单。</span>}
      </label>
      <label className="field">
        <span className="field-label">
          外部收款参考（流水号）<em>必填，与银行流水一致</em>
        </span>
        <Input aria-label="外部收款参考" value={ref} onChange={(e) => setRef(e.target.value)} placeholder="例如 TX20261108-0042" maxLength={120} />
      </label>
      <label className="field">
        <span className="field-label">备注</span>
        <Input aria-label="备注" value={note} onChange={(e) => setNote(e.target.value)} maxLength={200} />
      </label>
      {late && (
        <Callout tone="stop">
          <Checkbox checked={lateAck} onChange={(e) => setLateAck(e.target.checked)}>
            我已核实这是取消后到账的晚到款
          </Checkbox>
        </Callout>
      )}
    </ConfirmAction>
  )
}
