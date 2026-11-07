// 审计（R-12）：开通、暂停、换接入、换出口、收款确认、权益调整、划转、凭据查看等。只读，可按操作方、动作、对象、时间查询。
import { useMemo } from 'react'
import type { AuditLog } from '@isp/api-client/admin'
import { Plate, fmtDateTime } from '@isp/ui'
import { qk, useApi } from '../app/api'
import { Mono, PageHead } from '../components/bits'
import { DataTable, str, type Column, type FilterDef } from '../components/DataTable'
import { ACTOR_KIND } from '../lib/status'

const OUTCOME = { ok: '成功', denied: '拒绝', failed: '失败' } as const

export default function AuditPage() {
  const api = useApi()
  const filters = useMemo<FilterDef[]>(
    () => [
      { key: 'action', label: '动作', type: 'search', placeholder: '动作，例如 order.confirm_payment', width: 260 },
      { key: 'actorKind', label: '操作方', type: 'select', options: Object.entries(ACTOR_KIND).map(([value, label]) => ({ value, label })), width: 110 },
      { key: 'targetType', label: '对象类型', type: 'select', options: ['subscription', 'order', 'user', 'upstream', 'refund', 'exit_request', 'plan', 'rules', 'admin', 'alert'].map((v) => ({ value: v, label: v })), width: 140 },
      { key: 'targetId', label: '对象 ID', type: 'search', width: 200 },
      { key: 'from', label: '起', type: 'date' },
      { key: 'to', label: '止', type: 'date' },
    ],
    [],
  )
  const columns: Column<AuditLog>[] = [
    { key: 'at', title: '时间', fixedVisible: true, render: (_, l) => <span className="nowrap">{fmtDateTime(l.at)}</span> },
    {
      key: 'actor',
      title: '操作方',
      render: (_, l) => (
        <div className="cell-stack">
          <span>{ACTOR_KIND[l.actor.kind]}</span>
          <span className="muted">{l.actor.email ?? l.actor.id ?? '—'}</span>
        </div>
      ),
    },
    { key: 'action', title: '动作', render: (_, l) => <Mono>{l.action}</Mono> },
    {
      key: 'target',
      title: '对象',
      render: (_, l) =>
        l.targetType ? (
          <div className="cell-stack">
            <span>{l.targetType}</span>
            <span className="muted">
              <Mono>{l.targetId ?? '—'}</Mono>
            </span>
          </div>
        ) : (
          '—'
        ),
    },
    {
      key: 'outcome',
      title: '结果',
      render: (_, l) => <span className={l.outcome === 'ok' ? undefined : 'state tone-stop'}>{OUTCOME[l.outcome]}</span>,
    },
    {
      key: 'details',
      title: '详情',
      render: (_, l) =>
        Object.keys(l.details).length ? (
          <code className="mono small" style={{ whiteSpace: 'pre-wrap', display: 'block', maxWidth: 420 }}>
            {Object.entries(l.details)
              .map(([k, v]) => `${k}=${typeof v === 'object' ? JSON.stringify(v) : String(v)}`)
              .join('  ')}
          </code>
        ) : (
          <span className="muted">—</span>
        ),
    },
    { key: 'ip', title: '来源 IP', defaultHidden: true, render: (_, l) => (l.ip ? <Mono>{l.ip}</Mono> : '—') },
    { key: 'ua', title: 'User-Agent', defaultHidden: true, render: (_, l) => l.userAgent ?? '—' },
  ]
  return (
    <>
      <PageHead title="审计" sub="所有写操作与凭据查看都有记录；凭据明文不进入审计。" />
      <Plate>
        <DataTable<AuditLog>
          id="audit"
          label="审计记录"
          columns={columns}
          filters={filters}
          queryKey={qk.audit}
          rowKey={(l) => String(l.id)}
          fetchPage={({ filters: f, cursor, limit }) =>
            api.adminListAuditLogs({
              query: {
                action: str(f, 'action'),
                actorKind: str(f, 'actorKind') as AuditLog['actor']['kind'] | undefined,
                targetType: str(f, 'targetType'),
                targetId: str(f, 'targetId'),
                from: str(f, 'from'),
                to: str(f, 'to'),
                cursor,
                limit,
              },
            })
          }
        />
      </Plate>
    </>
  )
}
