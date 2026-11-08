// 订阅详情（R-12）：余量、出口、接入、进行中任务、用量趋势，以及账本、接入版本、出口版本、任务、事件、订单。
// 明文密码只在管理员填写原因后由“查看连接”接口返回（进入审计），页面不缓存、60 秒后自动隐藏。
import {
  DeleteOutlined,
  EyeInvisibleOutlined,
  EyeOutlined,
  KeyOutlined,
  PauseCircleOutlined,
  PlayCircleOutlined,
  RadarChartOutlined,
  RedoOutlined,
  SwapOutlined,
  TransactionOutlined,
  WalletOutlined,
} from '@ant-design/icons'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { App, Button, Dropdown, Table, Tabs } from 'antd'
import { useEffect, useState, type ReactNode } from 'react'
import { Link, useNavigate, useParams } from 'react-router'
import type { AccessSecret, AdminSubscriptionDetail, Probe, Task } from '@isp/api-client/admin'
import { CountryFlag, Meter, Plate, fmtBytes, fmtBytesExact, fmtDateTime, fmtMoney, fmtSpan, splitBytes, splitDuration } from '@isp/ui'
import { errorText, qk, useApi } from '../app/api'
import { Ago, BlockerChips, Callout, CopyButton, Facts, LoadError, Loading, Mono, PageHead, State } from '../components/bits'
import { ConfirmAction } from '../components/ConfirmAction'
import { ChangeAccessDialog, DeleteDialog, LedgerDialog, PauseDialog, ResumeDialog, RetryFulfillDialog, TransferDialog } from '../components/subActions'
import { TaskStrip } from '../components/TaskStrip'
import { UsageTrend } from '../components/UsageTrend'
import { useProbe } from '../lib/jobs'
import {
  ACCESS_CHANGE,
  ACCESS_STATE,
  ACTOR_KIND,
  EVENT_KIND,
  EXIT_CHECK,
  FULFILLMENT,
  LEDGER_SOURCE,
  LIFECYCLE,
  ORDER_KIND,
  PAYMENT,
  TASK_KIND,
  TASK_STATE,
  primaryState,
} from '../lib/status'

type DialogKind = 'pause' | 'resume' | 'access' | 'ledger' | 'transfer' | 'delete' | 'retry' | 'reveal' | null

export default function SubscriptionDetailPage() {
  const { id = '' } = useParams()
  const api = useApi()
  const q = useQuery({ queryKey: qk.sub(id), queryFn: () => api.adminGetSubscription({ subscriptionId: id }) })
  const nodes = useQuery({ queryKey: qk.nodes, queryFn: () => api.adminListNodes() })
  const [dialog, setDialog] = useState<DialogKind>(null)
  const [task, setTask] = useState<Task | null>(null)
  const [probe, setProbe] = useState<Probe | null>(null)
  const { message } = App.useApp()

  if (q.isPending) return <Loading rows={10} />
  if (q.isError) return <LoadError error={q.error} retry={() => void q.refetch()} />
  const s = q.data
  const node = nodes.data?.find((n) => n.id === s.nodeId)
  const live = s.lifecycle === 'active' || s.lifecycle === 'retained'
  const shownTask = task ?? s.activeTask
  const close = () => setDialog(null)

  const checkExit = async () => {
    try {
      setProbe(await api.adminCheckExit({ subscriptionId: s.id }))
    } catch (e) {
      message.error(errorText(e))
    }
  }

  const direct = s.kind === 'direct'
  // 直出代理（D-25）不入账、不划转、不在这里删除（到用户详情收回赠送），开通失败也在用户详情重新赠送。
  const more = direct
    ? []
    : [
    { key: 'ledger', icon: <WalletOutlined />, label: '账本入账（调整 / 赠送 / 补偿）', disabled: s.lifecycle === 'deleted' },
    { key: 'transfer', icon: <TransactionOutlined />, label: '划转剩余流量到同一用户的其他订阅', disabled: s.traffic.remainingBytes <= 0 },
    { key: 'retry', icon: <RedoOutlined />, label: '重试开通', disabled: !(s.lifecycle === 'fulfill_failed' || s.lifecycle === 'pending') || !!s.activeTask },
    { type: 'divider' as const },
    { key: 'delete', icon: <DeleteOutlined />, label: '删除订阅', danger: true, disabled: !(s.lifecycle === 'active' || s.lifecycle === 'released') },
  ]

  return (
    <>
      <PageHead
        back={{ to: '/subscriptions', label: '订阅' }}
        title={
          <>
            {s.name}
            <State view={primaryState(s)} />
            <BlockerChips blockers={s.blockers} skipFirst />
          </>
        }
        sub={
          <>
            <Mono>{s.id}</Mono>
            <Link to={`/users/${s.user.id}`}>{s.user.email}</Link>
            <span>创建于 {fmtDateTime(s.createdAt)}</span>
          </>
        }
        extra={
          <>
            {s.adminPaused ? (
              <Button icon={<PlayCircleOutlined />} onClick={() => setDialog('resume')}>
                恢复
              </Button>
            ) : (
              <Button icon={<PauseCircleOutlined />} onClick={() => setDialog('pause')} disabled={!live && s.lifecycle !== 'pending'}>
                暂停
              </Button>
            )}
            <Button icon={<KeyOutlined />} onClick={() => setDialog('access')} disabled={!live || !s.access || !!s.activeTask}>
              换接入
            </Button>
            {direct ? (
              <Link to={`/users/${s.user.id}`}>
                <Button>在用户详情管理直出</Button>
              </Link>
            ) : (
              <Link to={`/subscriptions/${s.id}/exit-change`}>
                <Button type="primary" icon={<SwapOutlined />} disabled={!live || !!s.activeTask}>
                  换出口
                </Button>
              </Link>
            )}
            {more.length > 0 && (
            <Dropdown
              menu={{
                items: more,
                onClick: ({ key }) => setDialog(key as DialogKind),
              }}
              trigger={['click']}
            >
              <Button>更多</Button>
            </Dropdown>
            )}
          </>
        }
      />

      <div className="stack">
        {s.adminPaused && (
          <Callout tone="stop" icon={<PauseCircleOutlined />}>
            管理员已暂停：{s.pausedReason}
          </Callout>
        )}
        {s.lifecycleReason && s.lifecycle !== 'active' && (
          <Callout tone={s.lifecycle === 'fulfill_failed' ? 'stop' : 'info'}>
            {LIFECYCLE[s.lifecycle]}：{s.lifecycleReason}
          </Callout>
        )}
        {shownTask && <TaskStrip key={shownTask.id} taskId={shownTask.id} initial={shownTask} />}
        {s.desiredVersion !== s.appliedVersion && (
          <Callout tone="warn">
            节点配置未追平：期望版本 {s.desiredVersion}，节点已应用 {s.appliedVersion}。过时配置不会让已停用的订阅重新可用。
          </Callout>
        )}

        <div className="grid-5-7">
          <Plate title="余量" extra={<span className="muted small">以账本为准</span>}>
            <div className="plate-pad stack" style={{ gap: 18 }}>
              {!s.plan ? (
                <Facts
                  items={[
                    ['类型', '服务器直出（管理员赠送）'],
                    ['规格', `${s.direct?.maxConnections} 并发 · ${s.direct?.bandwidthMbps} Mbps · 不限流量`],
                    ['可用截止', s.direct?.paidUntil ? `跟随付费订阅，至 ${fmtDateTime(s.direct.paidUntil)}` : '没有时长有效的付费订阅'],
                    ['累计用量', fmtBytes(s.traffic.usedBytes)],
                    ['生命周期', LIFECYCLE[s.lifecycle]],
                    ['首次可用', s.firstReadyAt ? fmtDateTime(s.firstReadyAt) : null],
                    ['规则版本', <Mono key="r">{s.rulesVersion}</Mono>],
                    ['自助换接入', `今日剩 ${s.quotas.accessChangesLeft} / ${s.quotas.accessChangesPerDay} 次`],
                  ]}
                />
              ) : (
                <>
              <div className="sub-meters">
                <TimeMeter s={s} />
                <TrafficMeter s={s} />
              </div>
              <Facts
                items={[
                  [
                    '套餐',
                    <span key="p" style={{ display: 'inline-flex', gap: 8, alignItems: 'center' }}>
                      <CountryFlag code={s.plan.countryCode} name={s.plan.country} size="sm" />
                      {s.plan.name} <span className="muted">v{s.plan.planVersion}</span>
                    </span>,
                  ],
                  ['规格', `${s.plan.concurrency} 并发 · ${s.plan.bandwidthMbps} Mbps · 周期 ${s.time.periodDays} 天`],
                  ['生命周期', LIFECYCLE[s.lifecycle]],
                  ['首次可用', s.firstReadyAt ? fmtDateTime(s.firstReadyAt) : null],
                  ['保留期至', s.time.retentionEndsAt ? fmtDateTime(s.time.retentionEndsAt) : null],
                  ['规则版本', <Mono key="r">{s.rulesVersion}</Mono>],
                  ['自助换接入', `今日剩 ${s.quotas.accessChangesLeft} / ${s.quotas.accessChangesPerDay} 次`],
                  ['免费换出口', `剩 ${s.quotas.freeExitChangesLeft} 次${s.quotas.freeExitWindowEndsAt ? `（窗口至 ${fmtDateTime(s.quotas.freeExitWindowEndsAt)}）` : ''}`],
                  ['来源订单', s.createdFromOrderId ? <Link key="o" to={`/orders/${s.createdFromOrderId}`}>查看订单</Link> : null],
                ]}
              />
                </>
              )}
            </div>
          </Plate>

          <div className="stack">
            <Plate
              title="出口"
              extra={
                <Button size="small" icon={<RadarChartOutlined />} onClick={checkExit} disabled={!s.exit}>
                  检测出口
                </Button>
              }
            >
              <div className="plate-pad stack" style={{ gap: 12 }}>
                {probe && <ProbeLine key={probe.id} probe={probe} subId={s.id} />}
                {s.exit ? (
                  <Facts
                    cols={2}
                    items={[
                      ['出口 IP', <Mono key="ip">{s.exit.verifiedIp ?? '未验证'}</Mono>],
                      ['检测', s.exit.check ? <State key="c" view={EXIT_CHECK[s.exit.check]} strong={false} /> : '未检测'],
                      ['地区', `${s.exit.country} · ${s.exit.city}`],
                      ['延迟', s.exit.latencyMs === null ? null : `${s.exit.latencyMs} ms`],
                      ['检测时间', s.exit.checkedAt ? <Ago key="t" at={s.exit.checkedAt} /> : null],
                      ['检测错误', s.exit.checkError],
                      ['出口版本', <span key="v" className="version-chip">v{s.exit.version}</span>],
                      ['绑定时间', s.exit.boundAt ? fmtDateTime(s.exit.boundAt) : null],
                      ['供应商', s.exit.provider],
                      ['上游', <Mono key="u">{`${s.exit.upstreamProtocol}://${s.exit.upstreamAddr}`}</Mono>],
                      ['资源', <Link key="r" to={`/upstreams?q=${encodeURIComponent(s.exit.resourceId)}`}><Mono>{s.exit.resourceId}</Mono></Link>],
                      ['节点', node ? `${node.name}${node.online ? '' : '（离线）'}` : s.nodeId],
                    ]}
                  />
                ) : direct ? (
                  <p className="muted">服务器直出：经节点自身地址出站，不经 ISP 上游，没有出口资源。</p>
                ) : (
                  <p className="muted">没有绑定出口（{LIFECYCLE[s.lifecycle]}）。</p>
                )}
              </div>
            </Plate>
            <Plate
              title="接入"
              extra={
                s.access ? (
                  <Button size="small" icon={<EyeOutlined />} onClick={() => setDialog('reveal')}>
                    查看连接
                  </Button>
                ) : null
              }
            >
              <div className="plate-pad stack" style={{ gap: 12 }}>
                {s.access ? (
                  <Facts
                    cols={2}
                    items={[
                      ['主机', <Mono key="h">{s.access.host}</Mono>],
                      ['用户名', <Mono key="u">{s.access.username}</Mono>],
                      ['端口', <Mono key="p">{`HTTP ${s.access.ports.http} · HTTPS ${s.access.ports.https} · SOCKS5 ${s.access.ports.socks5}`}</Mono>],
                      ['密码', <span key="pw" className="muted">不显示（查看连接需填写原因）</span>],
                      ['接入版本', <span key="v" className="version-chip">v{s.access.version}</span>],
                      ['状态', <State key="s" view={ACCESS_STATE[s.access.state]} strong={false} />],
                      ['签发', fmtDateTime(s.access.issuedAt)],
                      ['生效', s.access.activatedAt ? fmtDateTime(s.access.activatedAt) : null],
                    ]}
                  />
                ) : (
                  <p className="muted">没有生效的接入。</p>
                )}
                <Facts
                  cols={2}
                  items={[
                    ['活动连接', s.activeConns === null ? <span key="c" className="muted">无数据（节点未上报）</span> : s.activeConns],
                    ['用量上报', <Ago key="r" at={s.usageReportedAt} empty="从未上报" />],
                  ]}
                />
              </div>
            </Plate>
          </div>
        </div>

        <Plate>
          <div className="plate-pad">
            <UsageTrend subscriptionId={s.id} nodeOnline={node ? node.online : null} />
          </div>
        </Plate>

        <Plate className="tabs-plate">
          <SubTabs s={s} />
        </Plate>
      </div>

      <PauseDialog sub={s} open={dialog === 'pause'} onClose={close} />
      <ResumeDialog sub={s} open={dialog === 'resume'} onClose={close} />
      <ChangeAccessDialog sub={s} open={dialog === 'access'} onClose={close} onTask={setTask} />
      <LedgerDialog sub={s} open={dialog === 'ledger'} onClose={close} />
      <TransferDialog sub={s} open={dialog === 'transfer'} onClose={close} />
      <DeleteDialog sub={s} open={dialog === 'delete'} onClose={close} />
      <RetryFulfillDialog sub={s} open={dialog === 'retry'} onClose={close} onTask={setTask} />
      <RevealDialog sub={s} open={dialog === 'reveal'} onClose={close} />
    </>
  )
}

function TimeMeter({ s }: { s: AdminSubscriptionDetail }) {
  const r = splitDuration(Math.max(0, s.time.remainingSeconds))
  const total = s.time.periodDays * 86_400
  const blocked = s.blockers.includes('expired')
  return (
    <Meter
      label="剩余时长"
      value={r.value}
      unit={r.unit}
      rest={r.rest}
      total={s.time.expiresAt ? `到期 ${fmtDateTime(s.time.expiresAt)}` : '未开始'}
      ratio={total > 0 ? s.time.remainingSeconds / total : 0}
      tone={blocked ? 'stop' : s.time.remainingSeconds < 3 * 86_400 ? 'warn' : 'ok'}
    />
  )
}

function TrafficMeter({ s }: { s: AdminSubscriptionDetail }) {
  const rem = Math.max(0, s.traffic.remainingBytes)
  const r = splitBytes(rem)
  const blocked = s.blockers.includes('traffic_exhausted')
  return (
    <Meter
      label="剩余流量"
      value={r.value}
      unit={r.unit}
      total={`${fmtBytes(s.traffic.totalBytes)}，已用 ${fmtBytes(s.traffic.usedBytes)}`}
      ratio={s.traffic.totalBytes > 0 ? rem / s.traffic.totalBytes : 0}
      tone={blocked ? 'stop' : s.traffic.totalBytes > 0 && rem / s.traffic.totalBytes < 0.1 ? 'warn' : 'ok'}
    />
  )
}

function ProbeLine({ probe, subId }: { probe: Probe; subId: string }) {
  const q = useProbe(probe.id, probe)
  const qc = useQueryClient()
  const p = q.data ?? probe
  const done = p.status === 'done' || p.status === 'expired'
  useEffect(() => {
    if (done) void qc.invalidateQueries({ queryKey: qk.sub(subId) })
  }, [done, qc, subId])
  if (!done)
    return (
      <Callout tone="info">
        节点 <Mono>{p.nodeId}</Mono> 正在经客户入口检测出口…（{p.status === 'queued' ? '排队中' : '已下发'}）
      </Callout>
    )
  if (p.status === 'expired') return <Callout tone="warn">检测超时：节点没有在期限内回报。</Callout>
  return (
    <Callout tone={p.result === 'failed' ? 'stop' : p.result === 'slow' ? 'warn' : 'info'}>
      检测完成：{p.result === 'failed' ? `失败（${p.error}）` : <>出口 <Mono>{p.observedIp}</Mono> · {p.observedCountryCode} {p.observedCity} · {p.latencyMs} ms</>}
    </Callout>
  )
}

function RevealDialog({ sub, open, onClose }: { sub: AdminSubscriptionDetail; open: boolean; onClose: () => void }) {
  const api = useApi()
  const [secret, setSecret] = useState<AccessSecret | null>(null)
  const [show, setShow] = useState(false)
  // 明文只在内存里停留 60 秒
  useEffect(() => {
    if (!secret) return
    const t = setTimeout(() => setSecret(null), 60_000)
    return () => clearTimeout(t)
  }, [secret])
  return (
    <>
      <ConfirmAction
        open={open && !secret}
        onClose={onClose}
        tone="info"
        title={`查看连接 · ${sub.name}`}
        okText="查看"
        reasonLabel="查看原因（工单号或客户请求）"
        impacts={[<>返回当前接入 v{sub.access?.version} 的明文密码；本次查看与原因写入审计（不记录明文）。</>, <>60 秒后自动隐藏；关闭页面即清除。</>]}
        onConfirm={(reason) => api.adminRevealAccess({ subscriptionId: sub.id, body: { reason } })}
        onDone={(r) => {
          setShow(false)
          setSecret(r as AccessSecret)
        }}
      />
      {secret && (
        <div className="reveal-float" role="dialog" aria-label="明文连接">
          <Plate title={`明文连接 · 接入 v${secret.version}`} extra={<Button size="small" onClick={() => setSecret(null)}>隐藏</Button>}>
            <div className="plate-pad">
              <div className="secret-box">
                {(['http', 'https', 'socks5'] as const).map((proto) => {
                  const url = `${proto}://${secret.username}:${show ? secret.password : '••••••••'}@${secret.host}:${secret.ports[proto]}`
                  const real = `${proto}://${secret.username}:${secret.password}@${secret.host}:${secret.ports[proto]}`
                  return (
                    <div key={proto} className="secret-line">
                      <span className="muted">{proto.toUpperCase()}</span>
                      <Mono>{url}</Mono>
                      <CopyButton text={real} label={`复制 ${proto.toUpperCase()} 连接`} />
                    </div>
                  )
                })}
              </div>
              <Button size="small" type="link" icon={show ? <EyeInvisibleOutlined /> : <EyeOutlined />} onClick={() => setShow((x) => !x)} style={{ paddingLeft: 0, marginTop: 8 }}>
                {show ? '遮住密码' : '显示密码'}
              </Button>
            </div>
          </Plate>
        </div>
      )}
    </>
  )
}

function SubTabs({ s }: { s: AdminSubscriptionDetail }) {
  const api = useApi()
  const [tab, setTab] = useState('ledger')
  const ledger = useQuery({ queryKey: qk.subPart(s.id, 'ledger'), queryFn: () => api.adminListLedger({ subscriptionId: s.id }), enabled: tab === 'ledger' })
  const access = useQuery({ queryKey: qk.subPart(s.id, 'access'), queryFn: () => api.adminListAccessVersions({ subscriptionId: s.id }), enabled: tab === 'access' })
  const exits = useQuery({ queryKey: qk.subPart(s.id, 'exits'), queryFn: () => api.adminListExitBindings({ subscriptionId: s.id }), enabled: tab === 'exits' })
  const tasks = useQuery({ queryKey: qk.subPart(s.id, 'tasks'), queryFn: () => api.adminListSubscriptionTasks({ subscriptionId: s.id }), enabled: tab === 'tasks' })
  const events = useQuery({ queryKey: qk.subPart(s.id, 'events'), queryFn: () => api.adminListSubscriptionEvents({ subscriptionId: s.id }), enabled: tab === 'events' })
  const orders = useQuery({
    queryKey: [...qk.orders, 'by-sub', s.id],
    queryFn: () => api.adminListOrders({ query: { subscriptionId: s.id, limit: 50 } }),
    enabled: tab === 'orders',
  })
  const navigate = useNavigate()

  const wrap = (q: { isPending: boolean; isError: boolean; error: unknown; refetch: () => unknown }, node: () => ReactNode) =>
    q.isPending ? <Loading /> : q.isError ? <LoadError error={q.error} retry={() => void q.refetch()} /> : node()

  return (
    <Tabs
      activeKey={tab}
      onChange={setTab}
      items={[
        {
          key: 'ledger',
          label: '账本',
          children: wrap(ledger, () => (
            <>
              <div className="tab-pad small" style={{ paddingBottom: 8 }}>
                入账合计 流量 <b>{fmtBytes(ledger.data!.entitledBytesTotal)}</b> · 已用 <b>{fmtBytes(ledger.data!.usedBytesTotal)}</b> · 时长{' '}
                <b>{fmtSpan(ledger.data!.entitledSecondsTotal)}</b>
              </div>
              <Table
                className="plain-table"
                size="small"
                rowKey="id"
                pagination={false}
                scroll={{ x: 'max-content' }}
                dataSource={ledger.data!.entries}
                columns={[
                  { title: '时间', dataIndex: 'createdAt', render: (v: string) => fmtDateTime(v) },
                  { title: '来源', dataIndex: 'sourceType', render: (v: keyof typeof LEDGER_SOURCE) => LEDGER_SOURCE[v] },
                  { title: '项目', dataIndex: 'entitlementKind', render: (v: string) => (v === 'traffic' ? '流量' : '时长') },
                  {
                    title: '数量',
                    align: 'right',
                    render: (_, e) => (
                      <span className={`num ${e.amount < 0 ? 'state tone-stop' : ''}`} title={e.unit === 'bytes' ? fmtBytesExact(e.amount) : `${e.amount} 秒`}>
                        {e.unit === 'bytes' ? `${e.amount > 0 ? '+' : ''}${fmtBytes(e.amount)}` : fmtSpan(e.amount, true)}
                      </span>
                    ),
                  },
                  { title: '原因', dataIndex: 'reason' },
                  { title: '操作人', render: (_, e) => e.admin?.email ?? <span className="muted">系统</span> },
                  { title: '关联', render: (_, e) => (e.orderId ? <Link to={`/orders/${e.orderId}`}>订单</Link> : e.transferId ? <Mono>{e.transferId}</Mono> : '—') },
                ]}
              />
            </>
          )),
        },
        {
          key: 'access',
          label: '接入版本',
          children: wrap(access, () => (
            <Table
              className="plain-table"
              size="small"
              rowKey="id"
              pagination={false}
              scroll={{ x: 'max-content' }}
              dataSource={access.data!}
              columns={[
                { title: '版本', dataIndex: 'version', render: (v: number) => <span className="version-chip">v{v}</span> },
                { title: '类型', dataIndex: 'changeKind', render: (v: keyof typeof ACCESS_CHANGE) => ACCESS_CHANGE[v] },
                { title: '状态', dataIndex: 'state', render: (v: keyof typeof ACCESS_STATE) => <State view={ACCESS_STATE[v]} strong={false} /> },
                { title: '用户名', dataIndex: 'username', render: (v: string) => <Mono>{v}</Mono> },
                { title: '端口', render: (_, a) => <Mono>{`${a.ports.http} / ${a.ports.https} / ${a.ports.socks5}`}</Mono> },
                { title: '签发', dataIndex: 'issuedAt', render: (v: string) => fmtDateTime(v) },
                { title: '撤销', render: (_, a) => (a.revokedAt ? `${fmtDateTime(a.revokedAt)} · ${a.revokeReason ?? ''}` : '—') },
              ]}
            />
          )),
        },
        {
          key: 'exits',
          label: '出口版本',
          children: wrap(exits, () => (
            <Table
              className="plain-table"
              size="small"
              rowKey="id"
              pagination={false}
              scroll={{ x: 'max-content' }}
              dataSource={exits.data!}
              columns={[
                { title: '版本', dataIndex: 'version', render: (v: number) => <span className="version-chip">v{v}</span> },
                { title: '状态', dataIndex: 'state', render: (v: string) => ({ pending: '待生效', active: '生效中', unbound: '已解绑', failed: '失败' })[v] ?? v },
                { title: '出口 IP', dataIndex: 'verifiedIp', render: (v: string | null) => (v ? <Mono>{v}</Mono> : '未验证') },
                { title: '国家', dataIndex: 'verifiedCountryCode', render: (v: string | null) => v ?? '—' },
                { title: '检测', render: (_, b) => (b.check ? `${b.check.result} · ${b.check.latencyMs ?? '—'} ms · ${b.check.asn ?? ''}` : '—') },
                { title: '资源', dataIndex: 'resourceId', render: (v: string) => <Mono>{v}</Mono> },
                { title: '绑定', dataIndex: 'boundAt', render: (v: string | null) => fmtDateTime(v) },
                { title: '解绑', render: (_, b) => (b.unboundAt ? `${fmtDateTime(b.unboundAt)} · ${b.unboundReason ?? ''}` : '—') },
              ]}
            />
          )),
        },
        {
          key: 'tasks',
          label: '任务',
          children: wrap(tasks, () => (
            <Table
              className="plain-table"
              size="small"
              rowKey="id"
              pagination={false}
              scroll={{ x: 'max-content' }}
              dataSource={tasks.data!}
              locale={{ emptyText: '没有任务' }}
              columns={[
                { title: '任务', dataIndex: 'id', render: (v: string) => <Mono>{v}</Mono> },
                { title: '类型', dataIndex: 'kind', render: (v: keyof typeof TASK_KIND) => TASK_KIND[v] },
                { title: '状态', dataIndex: 'state', render: (v: keyof typeof TASK_STATE) => <State view={TASK_STATE[v]} strong={false} /> },
                { title: '发起', render: (_, t) => `${ACTOR_KIND[t.requestedBy.kind]}${t.requestedBy.email ? ` ${t.requestedBy.email}` : ''}` },
                { title: '尝试', dataIndex: 'attempts', align: 'right' },
                { title: '失败原因', render: (_, t) => (t.failureCode ? `${t.failureCode}：${t.failureReason ?? ''}` : '—') },
                { title: '创建', dataIndex: 'createdAt', render: (v: string) => fmtDateTime(v) },
                { title: '结束', dataIndex: 'finishedAt', render: (v: string | null) => fmtDateTime(v) },
              ]}
            />
          )),
        },
        {
          key: 'events',
          label: '事件',
          children: wrap(events, () => (
            <Table
              className="plain-table"
              size="small"
              rowKey="id"
              pagination={{ pageSize: 20, size: 'small', hideOnSinglePage: true }}
              scroll={{ x: 'max-content' }}
              dataSource={events.data!}
              columns={[
                { title: '时间', dataIndex: 'at', render: (v: string) => fmtDateTime(v) },
                { title: '事件', dataIndex: 'kind', render: (v: string) => EVENT_KIND[v] ?? v },
                { title: '内容', dataIndex: 'detail' },
                { title: '客户可见', dataIndex: 'customerVisible', render: (v: boolean) => (v ? '是' : <span className="muted">否</span>) },
                { title: '操作方', render: (_, e) => `${ACTOR_KIND[e.actor.kind]}${e.actor.email ? ` ${e.actor.email}` : ''}` },
              ]}
            />
          )),
        },
        {
          key: 'orders',
          label: '订单',
          children: wrap(orders, () => (
            <Table
              className="plain-table"
              size="small"
              rowKey="id"
              pagination={false}
              scroll={{ x: 'max-content' }}
              dataSource={orders.data!.items}
              locale={{ emptyText: '没有关联订单' }}
              onRow={(o) => ({ onClick: () => navigate(`/orders/${o.id}`), style: { cursor: 'pointer' } })}
              columns={[
                { title: '订单号', dataIndex: 'no', render: (v: string, o) => <Link to={`/orders/${o.id}`}><Mono>{v}</Mono></Link> },
                { title: '类型', dataIndex: 'kind', render: (v: keyof typeof ORDER_KIND) => ORDER_KIND[v] },
                { title: '金额', dataIndex: 'amount', align: 'right', render: (v) => fmtMoney(v) },
                { title: '收款', dataIndex: 'payment', render: (v: keyof typeof PAYMENT) => <State view={PAYMENT[v]} strong={false} /> },
                { title: '履约', dataIndex: 'fulfillment', render: (v: keyof typeof FULFILLMENT) => <State view={FULFILLMENT[v]} strong={false} /> },
                { title: '下单', dataIndex: 'createdAt', render: (v: string) => fmtDateTime(v) },
              ]}
            />
          )),
        },
      ]}
    />
  )
}
