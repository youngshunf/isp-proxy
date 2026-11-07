// 告警（R-19）：节点离线、上游异常、配置失败、证书、计量持久化失败、时钟偏差、履约 / 换出口失败、库存与端口池偏低、晚到款。
// 确认（ack）只表示“有人在处理”，不会关闭告警；条件消失后由系统标记为已恢复。
import { useQueryClient } from '@tanstack/react-query'
import { App, Button, Input } from 'antd'
import { useMemo, useState } from 'react'
import { Link } from 'react-router'
import type { Alert, AlertKind, AlertSeverity, AlertStatus } from '@isp/api-client/admin'
import { Plate, fmtDateTime } from '@isp/ui'
import { qk, useApi } from '../app/api'
import { Ago, Mono, PageHead, State } from '../components/bits'
import { ConfirmAction } from '../components/ConfirmAction'
import { DataTable, arr, str, type Column, type FilterDef } from '../components/DataTable'
import { ALERT_KIND, ALERT_STATUS, SEVERITY, options } from '../lib/status'

export default function AlertsPage() {
  const api = useApi()
  const [acking, setAcking] = useState<Alert | null>(null)
  const filters = useMemo<FilterDef[]>(
    () => [
      { key: 'status', label: '状态', type: 'multi', options: options(ALERT_STATUS) },
      { key: 'severity', label: '级别', type: 'select', options: options(SEVERITY), width: 110 },
      { key: 'kind', label: '类型', type: 'select', options: options(ALERT_KIND), width: 160 },
    ],
    [],
  )
  const columns: Column<Alert>[] = [
    { key: 'sev', title: '级别', fixedVisible: true, render: (_, a) => <State view={SEVERITY[a.severity]} /> },
    {
      key: 'msg',
      title: '告警',
      render: (_, a) => (
        <div className="cell-stack" style={{ maxWidth: 420 }}>
          <b style={{ fontWeight: 500 }}>{ALERT_KIND[a.kind]}</b>
          <span>{a.message}</span>
        </div>
      ),
    },
    {
      key: 'target',
      title: '对象',
      render: (_, a) => (
        <div className="cell-stack">
          {a.nodeId && <span>节点 <Mono>{a.nodeId}</Mono></span>}
          {a.subscriptionId && <Link to={`/subscriptions/${a.subscriptionId}`}>订阅</Link>}
          {a.orderId && <Link to={`/orders/${a.orderId}`}>订单</Link>}
          {a.resourceId && <span>资源 <Mono>{a.resourceId}</Mono></span>}
          {!a.nodeId && !a.subscriptionId && !a.orderId && !a.resourceId && <span className="muted">—</span>}
        </div>
      ),
    },
    {
      key: 'seen',
      title: '最近发生',
      render: (_, a) => (
        <div className="cell-stack">
          <Ago at={a.lastSeenAt} />
          <span className="muted">
            共 {a.occurrences} 次 · 首次 {fmtDateTime(a.firstSeenAt)}
          </span>
        </div>
      ),
    },
    {
      key: 'status',
      title: '状态',
      render: (_, a) => (
        <div className="cell-stack">
          <span>{ALERT_STATUS[a.status]}</span>
          {a.ackedBy && (
            <span className="muted">
              {a.ackedBy.email}
              {a.ackNote ? `：${a.ackNote}` : ''}
            </span>
          )}
        </div>
      ),
    },
    {
      key: 'op',
      title: '操作',
      fixedVisible: true,
      fixed: 'right',
      render: (_, a) =>
        a.status === 'open' ? (
          <Button size="small" onClick={() => setAcking(a)}>
            确认
          </Button>
        ) : (
          <span className="muted small">{a.resolvedAt ? `恢复于 ${fmtDateTime(a.resolvedAt)}` : '—'}</span>
        ),
    },
  ]
  return (
    <>
      <PageHead title="告警" sub="确认表示已有人跟进，不会关闭告警；条件消失后系统自动标记为已恢复。" />
      <Plate>
        <DataTable<Alert>
          id="alerts"
          label="告警列表"
          columns={columns}
          filters={filters}
          queryKey={qk.alerts}
          rowKey={(a) => a.id}
          refetchInterval={30_000}
          rowClassName={(a) => (a.status === 'open' && a.severity === 'critical' ? 'row-flag' : '')}
          fetchPage={({ filters: f, cursor, limit }) =>
            api.adminListAlerts({
              query: { status: arr<AlertStatus>(f, 'status'), severity: str(f, 'severity') as AlertSeverity | undefined, kind: str(f, 'kind') as AlertKind | undefined, cursor, limit },
            })
          }
        />
      </Plate>
      {acking && <AckDialog a={acking} onClose={() => setAcking(null)} />}
    </>
  )
}

function AckDialog({ a, onClose }: { a: Alert; onClose: () => void }) {
  const api = useApi()
  const qc = useQueryClient()
  const { message } = App.useApp()
  const [note, setNote] = useState('')
  return (
    <ConfirmAction
      open
      onClose={onClose}
      tone="info"
      title={`确认告警 · ${ALERT_KIND[a.kind]}`}
      okText="确认"
      impacts={[<>状态 未处理 → 已确认，记录确认人；告警不会关闭，条件仍在时继续计次。</>, <>{a.message}</>]}
      onConfirm={() => api.adminAckAlert({ alertId: a.id, body: { note: note.trim() || undefined } })}
      onDone={() => {
        message.success('已确认')
        void qc.invalidateQueries({ queryKey: qk.alerts })
        void qc.invalidateQueries({ queryKey: qk.overview })
      }}
    >
      <label className="field">
        <span className="field-label">备注（可选）</span>
        <Input aria-label="确认备注" value={note} onChange={(e) => setNote(e.target.value)} placeholder="例如：已联系机房" />
      </label>
    </ConfirmAction>
  )
}
