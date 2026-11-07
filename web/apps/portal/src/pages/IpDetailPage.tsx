import { ArrowLeftOutlined, EditOutlined, StopOutlined } from '@ant-design/icons'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { App, Button, Input, Result, Segmented, Skeleton } from 'antd'
import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router'
import type { Subscription, SubscriptionEvent, SubscriptionEventKind } from '@isp/api-client'
import { api, errorText, qk } from '../app/api'
import { AccessPanel } from '../components/AccessPanel'
import { Balances } from '../components/Balances'
import { ExitPanel } from '../components/ExitPanel'
import { CountryFlag, StateMark } from '../components/marks'
import { Plate } from '../components/Plate'
import { ExitFace, StateCell, serial } from '../components/SimCard'
import { fmtDate } from '../lib/format'
import { blockerLabel, blockerText, remedyFor } from '../lib/status'

function Rename({ sub }: { sub: Subscription }) {
  const [editing, setEditing] = useState(false)
  const [value, setValue] = useState(sub.name)
  const qc = useQueryClient()
  const { message } = App.useApp()
  const save = useMutation({
    mutationFn: () => api.renameSubscription(sub.id, value),
    onSuccess: (s) => {
      qc.setQueryData(qk.sub(sub.id), s)
      void qc.invalidateQueries({ queryKey: qk.subs })
      setEditing(false)
    },
    onError: (e) => message.error(errorText(e)),
  })
  if (!editing) {
    return (
      <div className="title-row">
        <h1>{sub.name}</h1>
        <Button
          type="text"
          icon={<EditOutlined />}
          aria-label="改名"
          onClick={() => {
            setValue(sub.name)
            setEditing(true)
          }}
        />
      </div>
    )
  }
  return (
    <form
      className="title-row"
      onSubmit={(e) => {
        e.preventDefault()
        save.mutate()
      }}
    >
      <Input
        className="rename-input"
        size="large"
        autoFocus
        value={value}
        maxLength={24}
        onChange={(e) => setValue(e.target.value)}
        onKeyDown={(e) => e.key === 'Escape' && setEditing(false)}
        aria-label="IP 名称"
      />
      <Button type="primary" htmlType="submit" loading={save.isPending}>
        保存
      </Button>
      <Button onClick={() => setEditing(false)}>取消</Button>
    </form>
  )
}

const EVENT_LABEL: Record<SubscriptionEventKind, string> = {
  ready: '开通就绪',
  time_added: '续时长',
  traffic_added: '买流量',
  password_reset: '重置密码',
  access_changed: '更换接入',
  exit_changed: '更换出口',
  expired: '时长到期',
  exhausted: '流量用尽',
  renamed: '改名',
}
const BENEFIT: SubscriptionEventKind[] = ['ready', 'time_added', 'traffic_added', 'expired', 'exhausted']
const CHANGE: SubscriptionEventKind[] = ['password_reset', 'access_changed', 'exit_changed']

function History({ sub }: { sub: Subscription }) {
  const [filter, setFilter] = useState<'all' | 'benefit' | 'change'>('all')
  const events = useQuery({ queryKey: qk.events(sub.id), queryFn: () => api.listSubscriptionEvents(sub.id) })
  const shown = (events.data ?? []).filter((e: SubscriptionEvent) =>
    filter === 'all' ? true : filter === 'benefit' ? BENEFIT.includes(e.kind) : CHANGE.includes(e.kind),
  )
  return (
    <Plate
      title="记录"
      labelledBy="history-h"
      extra={
        <Segmented
          size="small"
          value={filter}
          onChange={(v) => setFilter(v as typeof filter)}
          options={[
            { label: '全部', value: 'all' },
            { label: '时长与流量', value: 'benefit' },
            { label: '接入与出口', value: 'change' },
          ]}
        />
      }
    >
      <div className="plate-pad">
        {events.isPending && <Skeleton active paragraph={{ rows: 3 }} title={false} />}
        {events.data && shown.length === 0 && <p className="plate-note">暂无记录</p>}
        {shown.length > 0 && (
          <ol className="timeline">
            {shown.map((e) => {
              const bad = e.kind === 'expired' || e.kind === 'exhausted'
              return (
                <li key={e.id}>
                  <span className="t-mark" style={{ color: bad ? 'var(--stop)' : 'var(--teal)' }}>
                    <StateMark shape={bad ? 'hollow' : 'filled'} size={14} />
                  </span>
                  <div>
                    <div className="t-title">{EVENT_LABEL[e.kind]}</div>
                    <div>{e.detail}</div>
                    <div className="t-meta">
                      {fmtDate(e.at)}
                      {e.orderNo && <> · 订单 <span className="mono">{e.orderNo}</span></>}
                    </div>
                  </div>
                </li>
              )
            })}
          </ol>
        )}
      </div>
    </Plate>
  )
}

function Blockers({ sub }: { sub: Subscription }) {
  const navigate = useNavigate()
  if (sub.blockers.length === 0) return null
  return (
    <div style={{ display: 'grid', gap: 10, marginBottom: 24 }}>
      {sub.blockers.map((b) => {
        const r = remedyFor(b, sub)
        const tone = b === 'pending' || b === 'changing' ? 'info' : 'stop'
        return (
          <div key={b} className={`callout ${tone}`} style={{ alignItems: 'center', background: tone === 'info' ? 'var(--card)' : undefined }}>
            <StopOutlined style={{ marginTop: 0 }} />
            <div style={{ flex: 1 }}>
              <b>{blockerLabel(b)}</b>：{blockerText(b, sub)}
            </div>
            {r && (
              <Button type={tone === 'stop' ? 'primary' : 'default'} onClick={() => navigate(r.to)}>
                {r.label}
              </Button>
            )}
          </div>
        )
      })}
    </div>
  )
}

export default function IpDetailPage() {
  const { id = '' } = useParams()
  const navigate = useNavigate()
  const q = useQuery({ queryKey: qk.sub(id), queryFn: () => api.getSubscription(id) })

  if (q.isPending) {
    return (
      <Plate>
        <div className="plate-pad">
          <Skeleton active paragraph={{ rows: 8 }} />
        </div>
      </Plate>
    )
  }
  if (q.isError) {
    return (
      <Plate>
        <Result
          status="warning"
          title="这个 IP 没有加载出来"
          subTitle={errorText(q.error)}
          extra={<Button onClick={() => navigate('/ips')}>返回我的 IP</Button>}
        />
      </Plate>
    )
  }
  const sub = q.data
  const canBuy = sub.lifecycle !== 'pending' && sub.lifecycle !== 'fulfill_failed'

  return (
    <>
      <Link className="back-link" to="/ips">
        <ArrowLeftOutlined /> 我的 IP
      </Link>
      <div className="page-head">
        <div>
          <Rename sub={sub} />
          <div className="page-head-sub">
            <span>{sub.plan.name}</span>
            <span>
              并发 {sub.plan.concurrency} · 带宽 {sub.plan.bandwidthMbps} Mbps
            </span>
          </div>
        </div>
      </div>

      <Blockers sub={sub} />

      <div className="detail">
        <div className="detail-col">
          <div className="sim-wrap">
            <section className="plate sim" aria-label="套餐余量">
              <div className="plate-body">
                <StateCell sub={sub} />
                <div className="sim-face">
                  <div className="sim-top">
                    <CountryFlag code={sub.exit?.countryCode ?? sub.plan.countryCode} name={sub.exit?.country ?? sub.plan.country} />
                    <div className="sim-title">
                      <div className="sim-name" style={{ cursor: 'default' }}>
                        出口与套餐余量
                      </div>
                      <div className="sim-plan">
                        <span className="mono">{serial(sub.id)}</span> · 时长与流量属于这个 IP，换接入、换出口都不影响
                      </div>
                    </div>
                  </div>
                  <ExitFace sub={sub} large />
                </div>
                <div className="sim-tray">
                  <Balances sub={sub} detailed />
                </div>
                {canBuy && (
                  <div className="sim-foot">
                    <div className="actions">
                      <Button onClick={() => navigate(`/buy/renew/${sub.id}?kind=time`)}>续时长</Button>
                      <Button onClick={() => navigate(`/buy/renew/${sub.id}?kind=traffic`)}>买流量</Button>
                    </div>
                    <span className="foot-hint">两项分别购买，互不影响</span>
                  </div>
                )}
              </div>
            </section>
          </div>
          <History sub={sub} />
        </div>
        <div className="detail-col">
          <AccessPanel sub={sub} />
          <ExitPanel sub={sub} />
        </div>
      </div>
    </>
  )
}
