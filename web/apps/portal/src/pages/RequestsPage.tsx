import { ArrowRightOutlined } from '@ant-design/icons'
import { useQuery } from '@tanstack/react-query'
import { Button, Result, Skeleton } from 'antd'
import { Link } from 'react-router'
import { api, errorText, qk } from '../app/api'
import { StateMark } from '@isp/ui'
import { Plate } from '@isp/ui'
import { fmtDate } from '../lib/format'
import { EXIT_REASON, EXIT_REQUEST } from '../lib/status'

export default function RequestsPage() {
  const q = useQuery({ queryKey: qk.requests, queryFn: () => api.listExitRequests() })
  return (
    <>
      <div className="page-head">
        <div>
          <h1>换出口申请</h1>
          <div className="page-head-sub">在 IP 详情页的「出口」分区发起；切换完成后时长与流量不变。</div>
        </div>
      </div>
      <Plate>
        {q.isPending && (
          <div className="plate-pad">
            <Skeleton active paragraph={{ rows: 4 }} />
          </div>
        )}
        {q.isError && (
          <Result status="warning" title="申请没有加载出来" subTitle={errorText(q.error)} extra={<Button onClick={() => q.refetch()}>重新加载</Button>} />
        )}
        {q.data && q.data.length === 0 && (
          <div className="empty">
            <h2>还没有申请</h2>
            <p>需要换 IP 时，打开对应 IP 的详情页申请换出口。</p>
          </div>
        )}
        {q.data && q.data.length > 0 && (
          <ul className="rows" style={{ paddingTop: 6 }}>
            {q.data.map((r) => {
              const st = EXIT_REQUEST[r.status]
              return (
                <li key={r.id} className="req-row">
                  <div>
                    <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'baseline', gap: '4px 12px' }}>
                      <Link to={`/ips/${r.subscriptionId}`} style={{ fontWeight: 600, color: 'var(--ink)' }}>
                        {r.subscriptionName}
                      </Link>
                      <span className="req-meta mono">{r.no}</span>
                    </div>
                    <div className="req-ips" style={{ marginTop: 6 }}>
                      <span className="mono">{r.oldIp}</span>
                      <ArrowRightOutlined style={{ color: 'var(--ink-3)', fontSize: 12 }} />
                      <span className="mono" style={{ color: r.newIp ? 'var(--ink)' : 'var(--ink-3)' }}>
                        {r.newIp ?? (r.status === 'failed' || r.status === 'cancelled' ? '未更换' : '待分配')}
                      </span>
                    </div>
                    <div className="req-meta" style={{ marginTop: 4 }}>
                      {EXIT_REASON[r.reason]}
                      {r.note && `：${r.note}`} · {r.alsoChangeAccess ? '同时更换接入' : '接入不变'} · {r.charge === 'free' ? '免费次数' : '待管理员确认费用'} · 提交于{' '}
                      {fmtDate(r.createdAt)}
                    </div>
                    {r.failureReason && (
                      <div className="meter-note tone-stop" style={{ marginTop: 6 }}>
                        {r.failureReason}
                      </div>
                    )}
                  </div>
                  <div>
                    <span className={`chip tone-${st.tone}`}>
                      <StateMark shape={st.shape} />
                      {st.label}
                    </span>
                    <div className="req-meta" style={{ marginTop: 6 }}>
                      更新于 {fmtDate(r.updatedAt)}
                    </div>
                  </div>
                </li>
              )
            })}
          </ul>
        )}
      </Plate>
    </>
  )
}
