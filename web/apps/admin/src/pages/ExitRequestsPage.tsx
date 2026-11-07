// 换出口申请（R-12）：受理（免费 / 收费 / 免收）、驳回、执行（进入换出口流程并关联申请）、收尾。
// 超出免费次数的申请必须先决定费用；驳回与失败原因客户可见，内部备注客户不可见。
import { useQueryClient } from '@tanstack/react-query'
import { App, Button, Input, InputNumber, Radio, Space } from 'antd'
import { useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router'
import type { AdminExitRequest, ExitFeeStatus, ExitRequestStatus } from '@isp/api-client/admin'
import { Plate, fmtDateTime, fmtMoney } from '@isp/ui'
import { qk, useApi } from '../app/api'
import { Mono, PageHead, State, SubLink } from '../components/bits'
import { ConfirmAction } from '../components/ConfirmAction'
import { DataTable, arr, str, type Column, type FilterDef } from '../components/DataTable'
import { EXIT_REASON, EXIT_REQUEST, FEE, options } from '../lib/status'

type Dlg = { kind: 'accept' | 'reject' | 'complete'; r: AdminExitRequest } | null

export default function ExitRequestsPage() {
  const api = useApi()
  const [dlg, setDlg] = useState<Dlg>(null)
  const filters = useMemo<FilterDef[]>(
    () => [
      { key: 'status', label: '状态', type: 'multi', options: options(EXIT_REQUEST) },
      { key: 'feeStatus', label: '费用', type: 'select', options: options(FEE) },
      { key: 'subscriptionId', label: '订阅 ID', type: 'search', width: 200 },
    ],
    [],
  )
  const columns: Column<AdminExitRequest>[] = [
    {
      key: 'no',
      title: '申请',
      fixedVisible: true,
      render: (_, r) => (
        <div className="cell-stack">
          <Mono>{r.no}</Mono>
          <span className="muted">{fmtDateTime(r.createdAt)}</span>
        </div>
      ),
    },
    {
      key: 'sub',
      title: '订阅',
      render: (_, r) => (
        <div className="cell-stack">
          <SubLink id={r.subscriptionId} name={r.subscriptionName} />
          <span className="muted">{r.user.email}</span>
        </div>
      ),
    },
    {
      key: 'reason',
      title: '原因',
      render: (_, r) => (
        <div className="cell-stack" style={{ maxWidth: 260 }}>
          <span>{EXIT_REASON[r.reason]}</span>
          {r.note && <span className="muted">“{r.note}”</span>}
        </div>
      ),
    },
    { key: 'also', title: '同时换接入', render: (_, r) => (r.alsoChangeAccess ? '是' : '否') },
    {
      key: 'fee',
      title: '费用',
      render: (_, r) => (
        <div className="cell-stack">
          <span>{r.charge === 'free' ? '免费次数内' : '超出免费次数'}</span>
          <span className={r.feeStatus === 'awaiting_decision' ? 'state tone-warn' : 'muted'}>
            {FEE[r.feeStatus]}
            {r.feeAmount ? ` ${fmtMoney(r.feeAmount)}` : ''}
          </span>
        </div>
      ),
    },
    { key: 'status', title: '状态', render: (_, r) => <State view={EXIT_REQUEST[r.status]} /> },
    {
      key: 'ip',
      title: '出口',
      render: (_, r) => (
        <span>
          <Mono>{r.oldIp}</Mono>
          {r.newIp && (
            <>
              {' → '}
              <Mono>{r.newIp}</Mono>
            </>
          )}
        </span>
      ),
    },
    { key: 'failure', title: '失败 / 驳回原因', defaultHidden: true, render: (_, r) => r.failureReason ?? '—' },
    { key: 'note', title: '内部备注', defaultHidden: true, render: (_, r) => r.internalNote || '—' },
    { key: 'by', title: '处理人', render: (_, r) => r.handledBy?.email ?? <span className="muted">—</span> },
    {
      key: 'op',
      title: '操作',
      fixedVisible: true,
      fixed: 'right',
      render: (_, r) =>
        r.status === 'pending' ? (
          <Space size={4} wrap>
            <Button size="small" type="primary" onClick={() => setDlg({ kind: 'accept', r })}>
              受理
            </Button>
            <Button size="small" onClick={() => setDlg({ kind: 'reject', r })}>
              驳回
            </Button>
          </Space>
        ) : r.status === 'processing' ? (
          <Space size={4} wrap>
            {r.taskId ? (
              <span className="muted small">任务执行中</span>
            ) : (
              <Link to={`/subscriptions/${r.subscriptionId}/exit-change?request=${r.id}`}>
                <Button size="small" type="primary">
                  执行换出口
                </Button>
              </Link>
            )}
            <Button size="small" onClick={() => setDlg({ kind: 'complete', r })}>
              收尾
            </Button>
          </Space>
        ) : (
          <span className="muted small">{r.finishedAt ? fmtDateTime(r.finishedAt) : '—'}</span>
        ),
    },
  ]
  return (
    <>
      <PageHead title="换出口申请" sub="客户提交的换出口申请。受理后在“执行换出口”里选候选、验证、比较并提交；任务成功后申请自动完成。" />
      <Plate>
        <DataTable<AdminExitRequest>
          id="exit-requests"
          label="换出口申请"
          columns={columns}
          filters={filters}
          queryKey={qk.exitRequests}
          rowKey={(r) => r.id}
          rowClassName={(r) => (r.feeStatus === 'awaiting_decision' ? 'row-flag' : '')}
          fetchPage={({ filters: f, cursor, limit }) =>
            api.adminListExitRequests({
              query: { status: arr<ExitRequestStatus>(f, 'status'), feeStatus: str(f, 'feeStatus') as ExitFeeStatus | undefined, subscriptionId: str(f, 'subscriptionId'), cursor, limit },
            })
          }
        />
      </Plate>
      {dlg?.kind === 'accept' && <AcceptDialog r={dlg.r} onClose={() => setDlg(null)} />}
      {dlg?.kind === 'reject' && <RejectDialog r={dlg.r} onClose={() => setDlg(null)} />}
      {dlg?.kind === 'complete' && <CompleteDialog r={dlg.r} onClose={() => setDlg(null)} />}
    </>
  )
}

function useDone(msg: string) {
  const qc = useQueryClient()
  const { message } = App.useApp()
  return () => {
    message.success(msg)
    void qc.invalidateQueries({ queryKey: qk.exitRequests })
    void qc.invalidateQueries({ queryKey: qk.overview })
  }
}

function AcceptDialog({ r, onClose }: { r: AdminExitRequest; onClose: () => void }) {
  const api = useApi()
  const done = useDone('已受理')
  const [fee, setFee] = useState<'free' | 'charged' | 'waived'>(r.charge === 'free' ? 'free' : 'charged')
  const [yuan, setYuan] = useState<number | null>(null)
  const [ref, setRef] = useState('')
  const [note, setNote] = useState(r.internalNote)
  useEffect(() => setFee(r.charge === 'free' ? 'free' : 'charged'), [r.charge])
  const cents = yuan === null ? 0 : Math.round(yuan * 100)
  const ready = fee !== 'charged' || (cents > 0 && ref.trim().length > 0)
  return (
    <ConfirmAction
      open
      onClose={onClose}
      tone="warn"
      title={`受理申请 ${r.no}`}
      okText="受理"
      ready={ready}
      impacts={[
        <>申请状态 待处理 → 处理中；客户端显示“处理中”。</>,
        fee === 'free' ? (
          <>占用一次免费换出口次数。</>
        ) : fee === 'charged' ? (
          <>按收费处理：记录费用 {cents ? fmtMoney({ amount: cents, currency: 'CNY' }) : '（待填写）'} 与收款参考。</>
        ) : (
          <>免收费用：超出免费次数，但这一次不收费（写入审计）。</>
        ),
        <>受理后去“执行换出口”选择候选；客户要求{r.alsoChangeAccess ? '同时换接入' : '只换出口'}。</>,
      ]}
      onConfirm={() =>
        api.adminAcceptExitRequest({
          exitRequestId: r.id,
          body: { fee, feeAmount: fee === 'charged' ? { amount: cents, currency: 'CNY' } : undefined, feeExternalRef: fee === 'charged' ? ref.trim() : undefined, internalNote: note },
        })
      }
      onDone={done}
    >
      <div className="field">
        <span className="field-label">费用</span>
        <Radio.Group value={fee} onChange={(e) => setFee(e.target.value)} aria-label="费用">
          <Radio value="free" disabled={r.charge !== 'free'}>
            免费次数内
          </Radio>
          <Radio value="charged" disabled={r.charge === 'free'}>
            收费
          </Radio>
          <Radio value="waived" disabled={r.charge === 'free'}>
            免收
          </Radio>
        </Radio.Group>
      </div>
      {fee === 'charged' && (
        <div className="form-grid">
          <label className="field">
            <span className="field-label">金额（元）</span>
            <InputNumber aria-label="费用金额" min={0} precision={2} value={yuan} onChange={setYuan} style={{ width: '100%' }} />
          </label>
          <label className="field">
            <span className="field-label">收款参考</span>
            <Input aria-label="收款参考" value={ref} onChange={(e) => setRef(e.target.value)} />
          </label>
        </div>
      )}
      <label className="field">
        <span className="field-label">
          内部备注<em>客户不可见</em>
        </span>
        <Input.TextArea aria-label="内部备注" value={note} onChange={(e) => setNote(e.target.value)} autoSize={{ minRows: 2 }} />
      </label>
    </ConfirmAction>
  )
}

function RejectDialog({ r, onClose }: { r: AdminExitRequest; onClose: () => void }) {
  const api = useApi()
  const done = useDone('已驳回')
  return (
    <ConfirmAction
      open
      onClose={onClose}
      title={`驳回申请 ${r.no}`}
      okText="驳回"
      reasonLabel="驳回原因（客户可见）"
      impacts={[<>申请状态 → 失败，客户看到这里填写的原因。</>, <>出口与接入不变；不占用免费次数。</>]}
      onConfirm={(failureReason) => api.adminRejectExitRequest({ exitRequestId: r.id, body: { failureReason } })}
      onDone={done}
    />
  )
}

function CompleteDialog({ r, onClose }: { r: AdminExitRequest; onClose: () => void }) {
  const api = useApi()
  const done = useDone('已收尾')
  const [outcome, setOutcome] = useState<'succeeded' | 'failed'>(r.taskId ? 'succeeded' : 'failed')
  const [reason, setReason] = useState('')
  return (
    <ConfirmAction
      open
      onClose={onClose}
      tone="warn"
      title={`收尾申请 ${r.no}`}
      okText="确认"
      ready={outcome === 'succeeded' ? !!r.taskId : reason.trim().length > 0}
      impacts={
        outcome === 'succeeded'
          ? [<>标记为成功：关联任务 <Mono>{r.taskId ?? '（无）'}</Mono>，新出口取订阅当前出口。</>, <>只有已成功的换出口任务才能标记成功。</>]
          : [<>标记为失败：客户看到填写的原因；出口与接入不变。</>]
      }
      onConfirm={() =>
        api.adminCompleteExitRequest({
          exitRequestId: r.id,
          body: outcome === 'succeeded' ? { outcome, taskId: r.taskId ?? undefined } : { outcome, failureReason: reason.trim() },
        })
      }
      onDone={done}
    >
      <Radio.Group value={outcome} onChange={(e) => setOutcome(e.target.value)}>
        <Radio value="succeeded" disabled={!r.taskId}>
          成功
        </Radio>
        <Radio value="failed">失败</Radio>
      </Radio.Group>
      {outcome === 'failed' && (
        <label className="field">
          <span className="field-label">失败原因（客户可见）</span>
          <Input.TextArea aria-label="失败原因" value={reason} onChange={(e) => setReason(e.target.value)} autoSize={{ minRows: 2 }} />
        </label>
      )}
    </ConfirmAction>
  )
}
