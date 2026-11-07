import { InfoCircleOutlined, SwapOutlined, WarningOutlined } from '@ant-design/icons'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Alert, App, Button, Checkbox, Form, Input, Modal, Radio } from 'antd'
import { useState } from 'react'
import { Link } from 'react-router'
import type { ExitReason, Subscription } from '@isp/api-client'
import { api, errorText, qk } from '../app/api'
import { fmtAgo, fmtDate, fmtDay } from '../lib/format'
import { EXIT_CHECK, EXIT_REASON, EXIT_REQUEST } from '../lib/status'
import { SignalBars, StateMark } from './marks'
import { Plate } from './Plate'

interface FormValues {
  reason: ExitReason
  note?: string
  alsoChangeAccess: boolean
}

function ExitRequestDialog({ sub, onClose }: { sub: Subscription; onClose: () => void }) {
  const [form] = Form.useForm<FormValues>()
  const also = Form.useWatch('alsoChangeAccess', form)
  const qc = useQueryClient()
  const { message } = App.useApp()
  const free = sub.quotas.freeExitChangesLeft > 0
  const create = useMutation({
    mutationFn: (v: FormValues) =>
      api.createExitRequest({ subscriptionId: sub.id, reason: v.reason, note: v.note ?? '', alsoChangeAccess: v.alsoChangeAccess }),
    onSuccess: (r) => {
      void qc.invalidateQueries({ queryKey: qk.subs })
      void qc.invalidateQueries({ queryKey: qk.requests })
      message.success(`已提交换出口申请 ${r.no}`)
      onClose()
    },
  })
  return (
    <Modal
      open
      title={`申请换出口 · ${sub.name}`}
      okText="提交申请"
      cancelText="取消"
      onOk={() => form.submit()}
      onCancel={onClose}
      okButtonProps={{ loading: create.isPending }}
      width={560}
      destroyOnHidden
    >
      <Form<FormValues>
        form={form}
        layout="vertical"
        requiredMark={false}
        initialValues={{ alsoChangeAccess: false }}
        onFinish={(v) => create.mutate(v)}
        style={{ marginTop: 12 }}
      >
        <Form.Item name="reason" label="为什么要换" rules={[{ required: true, message: '请选择一个原因' }]}>
          <Radio.Group>
            <div style={{ display: 'grid', gap: 6 }}>
              {(Object.keys(EXIT_REASON) as ExitReason[]).map((k) => (
                <Radio key={k} value={k}>
                  {EXIT_REASON[k]}
                </Radio>
              ))}
            </div>
          </Radio.Group>
        </Form.Item>
        <Form.Item name="note" label="补充说明（选填）">
          <Input.TextArea rows={3} maxLength={200} showCount placeholder="例如：目标站从昨天开始频繁要求验证" />
        </Form.Item>
        <Form.Item name="alsoChangeAccess" valuePropName="checked" style={{ marginBottom: 12 }}>
          <Checkbox>同时更换接入（端口与密码）</Checkbox>
        </Form.Item>
      </Form>
      <div className="callout warn">
        <WarningOutlined />
        <div>
          切换时这个 IP 上的活动连接会中断一次；时长和流量不变。
          {also ? '切换完成时旧接入同时失效，需要用新连接更新程序。' : '接入不变，程序不用改配置。'}
        </div>
      </div>
      <p className="plate-note" style={{ margin: '12px 0 0' }}>
        {free
          ? '本次使用免费次数（每 30 天 1 次）。新出口在同一国家内分配，提交后在「申请」页查看进度。'
          : `免费次数已用完${sub.quotas.freeExitWindowEndsAt ? `，${fmtDay(sub.quotas.freeExitWindowEndsAt)} 恢复` : ''}。提交后由管理员确认是否收费，确认前不会切换。`}
      </p>
      {create.isError && <Alert style={{ marginTop: 12 }} type="error" showIcon title={errorText(create.error)} />}
    </Modal>
  )
}

/** 出口分区：实测地址、检测结果、换出口申请。 */
export function ExitPanel({ sub }: { sub: Subscription }) {
  const [open, setOpen] = useState(false)
  const requests = useQuery({ queryKey: qk.requests, queryFn: () => api.listExitRequests(), enabled: !!sub.openExitRequestId })
  const openReq = requests.data?.find((r) => r.id === sub.openExitRequestId)

  if (!sub.exit) {
    return (
      <Plate title="出口" labelledBy="exit-h">
        <div className="plate-pad plate-note">开通时在 {sub.plan.country} · {sub.plan.city} 分配，下单不锁定具体地址。</div>
      </Plate>
    )
  }
  const chk = EXIT_CHECK[sub.exit.check]
  const blockedSelf = sub.blockers.includes('admin_paused') || sub.blockers.includes('account_banned')
  const free = sub.quotas.freeExitChangesLeft > 0

  return (
    <Plate title="出口检测与更换" labelledBy="exit-h" extra={<span className="plate-note">出口 v{sub.exit.version}</span>}>
      <div className="plate-pad">
        <dl className="kv">
          <dt>最近检测</dt>
          <dd className="value">
            <span className="signal" style={{ fontSize: 14, color: 'var(--ink)' }}>
              <SignalBars bars={chk.bars} tone={chk.tone} outline={sub.exit.check === 'stale'} />
              {chk.label}
              {sub.exit.latencyMs !== null && sub.exit.check !== 'stale' && ` · ${sub.exit.latencyMs} ms`}
            </span>
            <div className="plate-note">
              {fmtDate(sub.exit.checkedAt)}（{fmtAgo(sub.exit.checkedAt)}）
              {sub.exit.check === 'stale' && ' · 停止转发期间不再检测'}
            </div>
          </dd>
          <dd className="act" />
        </dl>

        {sub.openExitRequestId ? (
          <div className="callout info" style={{ marginTop: 14 }}>
            <InfoCircleOutlined />
            <div style={{ flex: 1 }}>
              {openReq ? (
                <>
                  <div style={{ display: 'flex', alignItems: 'center', gap: 8, color: 'var(--teal)', fontWeight: 600 }}>
                    <StateMark shape={EXIT_REQUEST[openReq.status].shape} />
                    换出口申请 {EXIT_REQUEST[openReq.status].label}
                  </div>
                  <div>
                    <span className="mono">{openReq.no}</span> · 提交于 {fmtDate(openReq.createdAt)} ·{' '}
                    {openReq.alsoChangeAccess ? '完成时同时更换接入' : '接入不变'}。处理完成前当前出口照常可用。
                  </div>
                </>
              ) : (
                <div>已有换出口申请在处理，完成前当前出口照常可用。</div>
              )}
              <Link to="/requests">查看申请进度</Link>
            </div>
          </div>
        ) : (
          <div className="actions" style={{ marginTop: 18, alignItems: 'center' }}>
            <Button icon={<SwapOutlined />} onClick={() => setOpen(true)} disabled={blockedSelf}>
              申请换出口
            </Button>
            <span className="plate-note">
              {free
                ? '本期还有 1 次免费 · 接入与余量不变，可选同时换接入'
                : `免费次数已用完${sub.quotas.freeExitWindowEndsAt ? `，${fmtDay(sub.quotas.freeExitWindowEndsAt)} 恢复` : ''} · 超出由管理员确认`}
            </span>
          </div>
        )}
      </div>
      {open && <ExitRequestDialog sub={sub} onClose={() => setOpen(false)} />}
    </Plate>
  )
}
