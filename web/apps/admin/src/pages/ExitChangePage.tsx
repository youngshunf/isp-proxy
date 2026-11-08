// 换出口（docs/05 F-W3）：候选录入 → 验证 → 比较 → 提交 → 查结果。
// 库存里没有合适候选时可以当场录入：按订阅的节点与套餐地区预填，录入后直接选中。
// 候选必须由节点经实际链路测出出口 IP；与当前出口相同（“上游已更新但出口没变”）、平台内已占用、国家不符都会被拒，并写明原因。
// 提交时带比较探测的 ID 和幂等键：同一次比较只能提交一次，重试不会建出两个任务。
// 共享订阅（D-24）的候选只列有空位的共享资源；独享订阅的候选不含已有共享占用的资源。
import { ArrowLeftOutlined, CheckOutlined, CloseOutlined, LoadingOutlined, PlusOutlined } from '@ant-design/icons'
import { useQuery } from '@tanstack/react-query'
import { Button, Checkbox, Input, Radio, Switch, Table } from 'antd'
import { useEffect, useMemo, useReducer, useRef, useState } from 'react'
import { Link, useParams, useSearchParams } from 'react-router'
import type { AdminSubscriptionDetail, Upstream } from '@isp/api-client/admin'
import { CountryFlag, ExitModeChip, Plate, StateMark, fmtDateTime } from '@isp/ui'
import { errorText, newIdempotencyKey, qk, useApi } from '../app/api'
import { Callout, LoadError, Loading, Mono, PageHead, State } from '../components/bits'
import { UpstreamDialog } from '../components/UpstreamDialog'
import { STEPS, canSubmit, candidateFits, cityMatches, initialWizard, makeWizardReducer, type WizardState } from '../lib/exitWizard'
import { useProbe, useTask } from '../lib/jobs'
import { EXIT_REASON, TASK_KIND, TASK_STATE, UPSTREAM } from '../lib/status'

export default function ExitChangePage() {
  const { id = '' } = useParams()
  const api = useApi()
  const q = useQuery({ queryKey: qk.sub(id), queryFn: () => api.adminGetSubscription({ subscriptionId: id }) })
  if (q.isPending) return <Loading rows={8} />
  if (q.isError) return <LoadError error={q.error} retry={() => void q.refetch()} />
  const s = q.data
  if (!s.plan) {
    // 服务器直出代理（D-25）没有 ISP 出口，不能换出口。
    return (
      <>
        <PageHead back={{ to: `/subscriptions/${s.id}`, label: s.name }} title="换出口" />
        <Callout tone="stop">服务器直出代理经节点自身地址出站，没有出口可换。</Callout>
      </>
    )
  }
  return <Wizard s={{ ...s, plan: s.plan }} />
}

type IspDetail = AdminSubscriptionDetail & { plan: NonNullable<AdminSubscriptionDetail['plan']> }

function Wizard({ s }: { s: IspDetail }) {
  const api = useApi()
  const [sp] = useSearchParams()
  const requestId = sp.get('request')
  const req = useQuery({
    queryKey: [...qk.exitRequests, requestId],
    queryFn: () => api.adminGetExitRequest({ exitRequestId: requestId! }),
    enabled: !!requestId,
  })
  const reducer = useMemo(() => makeWizardReducer(s.plan.countryCode, s.plan.exitMode), [s.plan.countryCode, s.plan.exitMode])
  const [st, dispatch] = useReducer(reducer, initialWizard)
  const stepIndex = STEPS.findIndex((x) => x.key === st.step)
  const live = s.lifecycle === 'active' || s.lifecycle === 'retained'

  return (
    <>
      <PageHead
        back={{ to: `/subscriptions/${s.id}`, label: s.name }}
        title="换出口"
        sub={
          <>
            <span>
              当前出口 <Mono>{s.exit?.verifiedIp ?? '无'}</Mono>
            </span>
            <span>
              套餐国家 <CountryFlag code={s.plan.countryCode} name={s.plan.country} size="sm" /> {s.plan.country}
            </span>
            <span>接入 v{s.access?.version ?? '—'}</span>
            <span>
              出口类型 <ExitModeChip mode={s.plan.exitMode} />
            </span>
          </>
        }
      />
      <div className="stack">
        {!live && <Callout tone="stop">只有“有效”或“保留期”的订阅可以换出口。</Callout>}
        {s.activeTask && st.step !== 'result' && <Callout tone="warn">这个订阅已有进行中的任务（{TASK_KIND[s.activeTask.kind]}），完成后再换出口。</Callout>}
        {requestId && req.data && (
          <Callout tone={req.data.status === 'processing' ? 'info' : 'warn'}>
            关联申请 <Mono>{req.data.no}</Mono>：{EXIT_REASON[req.data.reason]}
            {req.data.note ? `，“${req.data.note}”` : ''}；{req.data.alsoChangeAccess ? '客户要求同时换接入' : '只换出口'}。
            {req.data.status !== 'processing' && ' 申请尚未受理，提交前请先在“换出口申请”里受理。'}
          </Callout>
        )}
        <Plate>
          <ol className="wizard-steps" aria-label="换出口步骤">
            {STEPS.map((x, i) => {
              const failed = (x.key === 'compare' && st.step === 'compare' && (st.rejection?.length ?? 0) > 0) || (x.key === 'result' && st.task?.state === 'failed')
              const cls = failed ? 'failed' : i < stepIndex ? 'done' : i === stepIndex ? 'current' : ''
              return (
                <li key={x.key} className={cls} aria-current={i === stepIndex ? 'step' : undefined}>
                  <span className="n">{i < stepIndex ? <CheckOutlined /> : failed ? <CloseOutlined /> : i + 1}</span>
                  {x.label}
                </li>
              )
            })}
          </ol>
          <div className="wizard-body">
            {st.step === 'candidate' && <CandidateStep s={s} st={st} dispatch={dispatch} disabled={!live || !!s.activeTask} />}
            {st.step === 'verify' && <VerifyStep st={st} dispatch={dispatch} />}
            {st.step === 'compare' && <CompareStep s={s} st={st} dispatch={dispatch} />}
            {st.step === 'submit' && <SubmitStep s={s} st={st} dispatch={dispatch} requestId={req.data?.status === 'processing' ? req.data.id : undefined} defaultAlso={req.data?.alsoChangeAccess ?? false} />}
            {st.step === 'result' && <ResultStep s={s} st={st} dispatch={dispatch} />}
          </div>
        </Plate>
      </div>
    </>
  )
}

type Dispatch = (a: Parameters<ReturnType<typeof makeWizardReducer>>[1]) => void

function CandidateStep({ s, st, dispatch, disabled }: { s: IspDetail; st: WizardState; dispatch: Dispatch; disabled: boolean }) {
  const api = useApi()
  const [sameCountry, setSameCountry] = useState(true)
  const [busy, setBusy] = useState(false)
  const [adding, setAdding] = useState(false)
  const nodes = useQuery({ queryKey: qk.nodes, queryFn: () => api.adminListNodes(), enabled: adding })
  const cands = useQuery({
    queryKey: [...qk.upstreams, 'candidates', sameCountry ? s.plan.countryCode : 'all'],
    queryFn: () => api.adminListUpstreams({ query: { status: ['available', 'pending_verify'], countryCode: sameCountry ? s.plan.countryCode : undefined, limit: 100 } }),
  })
  const start = async () => {
    if (!st.resourceId || busy) return
    setBusy(true)
    try {
      const p = await api.adminCompareExitCandidate({ subscriptionId: s.id, body: { resourceId: st.resourceId } })
      dispatch({ type: 'compareStarted', probe: p })
    } catch (e) {
      dispatch({ type: 'compareRefused', message: errorText(e) })
    } finally {
      setBusy(false)
    }
  }
  const shared = s.plan.exitMode === 'shared'
  // 与订阅同城（或订阅不限城市）的候选排在前面；不同城的仍可选，地区列标注“城市不同”（D-27）。
  const items = (cands.data?.items ?? [])
    .filter((u) => u.id !== s.exit?.resourceId && candidateFits(u, s.plan.exitMode))
    .sort((a, b) => Number(cityMatches(s.plan.city, b.city)) - Number(cityMatches(s.plan.city, a.city)))
  return (
    <>
      <div className="usage-head">
        <div>
          <h3>选择候选上游</h3>
          <p className="muted small">
            {shared
              ? '共享订阅：只列出绑定上限 ≥2、没有独享订阅且还有空位的资源（不能换到独享出口）。'
              : '只列出“可售”或“待验证”且未被占用的资源（已有共享订阅的资源不能给独享订阅）。'}
            候选会经节点实际链路测出出口 IP，再与当前出口比较。
          </p>
        </div>
        <span className="actions" style={{ alignItems: 'center' }}>
          <label className="small" style={{ display: 'inline-flex', gap: 6, alignItems: 'center' }}>
            <Switch size="small" checked={sameCountry} onChange={setSameCountry} aria-label="只看套餐国家" />
            只看 {s.plan.countryCode.toUpperCase()}
          </label>
          <Button size="small" icon={<PlusOutlined />} onClick={() => setAdding(true)} disabled={disabled}>
            录入新候选
          </Button>
        </span>
      </div>
      {cands.isPending ? (
        <Loading />
      ) : (
        <Radio.Group value={st.resourceId} onChange={(e) => dispatch({ type: 'select', resourceId: e.target.value })} style={{ display: 'block' }} aria-label="候选上游">
          <Table<Upstream>
            size="small"
            rowKey="id"
            pagination={false}
            scroll={{ x: 'max-content' }}
            dataSource={items}
            locale={{ emptyText: shared ? '没有有空位的共享资源：先在上游库存录入资源（绑定上限 ≥2）并验证' : '没有可用候选：先在上游库存录入并验证新资源' }}
            onRow={(u) => ({ onClick: () => dispatch({ type: 'select', resourceId: u.id }), style: { cursor: 'pointer' } })}
            columns={[
              { title: '', width: 40, render: (_, u) => <Radio value={u.id} aria-label={`选择 ${u.protocol}://${u.host}:${u.port}`} /> },
              {
                title: '上游',
                render: (_, u) => (
                  <div className="cell-stack">
                    <Mono>{`${u.protocol}://${u.host}:${u.port}`}</Mono>
                    <span className="muted">
                      {u.provider} · <Mono>{u.id}</Mono>
                    </span>
                  </div>
                ),
              },
              {
                title: '地区',
                render: (_, u) => (
                  <span style={{ display: 'inline-flex', gap: 6, alignItems: 'center' }}>
                    <CountryFlag code={u.countryCode} name={u.country} size="sm" />
                    {u.city ?? '—'}
                    {!cityMatches(s.plan.city, u.city) && <span className="muted small">城市不同</span>}
                  </span>
                ),
              },
              { title: '状态', render: (_, u) => <State view={UPSTREAM[u.status]} strong={false} /> },
              {
                title: '占用',
                render: (_, u) =>
                  u.occupancy.mode === 'shared' ? (
                    <span>
                      共享 <b className="num">{u.occupancy.used}/{u.maxTenants}</b>
                    </span>
                  ) : (
                    <span className="muted">{u.maxTenants === 1 ? '空闲 · 仅独享' : `空闲 · 上限 ${u.maxTenants}`}</span>
                  ),
              },
              {
                title: '最近验证',
                render: (_, u) =>
                  u.lastVerify ? (
                    <div className="cell-stack">
                      <span>{u.lastVerify.ok ? <Mono>{u.lastVerify.exitIp}</Mono> : <span className="state tone-stop">失败</span>}</span>
                      <span className="muted">{fmtDateTime(u.lastVerify.at)}</span>
                    </div>
                  ) : (
                    <span className="muted">从未验证</span>
                  ),
              },
              { title: '节点', dataIndex: 'nodeId', render: (v: string) => <Mono>{v}</Mono> },
            ]}
          />
        </Radio.Group>
      )}
      {st.rejection && st.rejection.length > 0 && (
        <Callout tone="stop">
          <b>候选被拒绝</b>
          <ul className="reasons">
            {st.rejection.map((r) => (
              <li key={r}>{r}</li>
            ))}
          </ul>
        </Callout>
      )}
      <div className="actions">
        <Button type="primary" onClick={start} disabled={!st.resourceId || disabled} loading={busy}>
          开始验证
        </Button>
      </div>
      <UpstreamDialog
        value={adding ? 'new' : null}
        nodes={(nodes.data ?? []).map((n) => ({ value: n.id, label: n.name }))}
        defaults={{ ...(s.nodeId ? { nodeId: s.nodeId } : {}), countryCode: s.plan.countryCode, city: s.plan.city ?? '' }}
        onClose={() => setAdding(false)}
        onCreated={(u) => dispatch({ type: 'select', resourceId: u.id })}
      />
    </>
  )
}

function VerifyStep({ st, dispatch }: { st: WizardState; dispatch: Dispatch }) {
  const q = useProbe(st.probe!.id, st.probe!)
  useEffect(() => {
    if (q.data) dispatch({ type: 'probe', probe: q.data })
  }, [q.data, dispatch])
  const p = q.data ?? st.probe!
  return (
    <>
      <Callout tone="info" icon={<LoadingOutlined />}>
        节点 <Mono>{p.nodeId}</Mono> 正经候选上游测出口 IP…（{p.status === 'queued' ? '排队中' : '已下发'}，期限 {fmtDateTime(p.deadlineAt)}）
      </Callout>
      {q.isError && <Callout tone="stop">读取验证进度失败：{errorText(q.error)}</Callout>}
      <div className="actions">
        <Button icon={<ArrowLeftOutlined />} onClick={() => dispatch({ type: 'back' })}>
          换一个候选
        </Button>
      </div>
    </>
  )
}

function Check({ ok, children }: { ok: boolean; children: React.ReactNode }) {
  return (
    <span className={`state tone-${ok ? 'ok' : 'stop'}`}>
      <StateMark shape={ok ? 'filled' : 'hollow'} />
      {children}
    </span>
  )
}

function CompareStep({ s, st, dispatch }: { s: IspDetail; st: WizardState; dispatch: Dispatch }) {
  const p = st.probe!
  const c = p.comparison
  const rejected = (st.rejection?.length ?? 0) > 0
  return (
    <>
      <h3>比较结果</h3>
      <table className="cmp-table">
        <thead>
          <tr>
            <th>项目</th>
            <th>当前出口</th>
            <th>候选实测</th>
            <th>结论</th>
          </tr>
        </thead>
        <tbody>
          <tr>
            <td>验证</td>
            <td>—</td>
            <td>{p.result === 'failed' ? `失败：${p.error}` : p.status === 'expired' ? '探测过期' : `${p.result === 'slow' ? '延迟偏高' : '正常'} · ${p.latencyMs ?? '—'} ms`}</td>
            <td>
              <Check ok={p.status === 'done' && p.result !== 'failed'}>{p.status === 'done' && p.result !== 'failed' ? '通过' : '未通过'}</Check>
            </td>
          </tr>
          <tr>
            <td>出口 IP</td>
            <td>
              <Mono>{c?.currentIp ?? s.exit?.verifiedIp ?? '—'}</Mono>
            </td>
            <td>
              <Mono>{p.observedIp ?? '—'}</Mono>
            </td>
            <td>{c ? <Check ok={c.differsFromCurrent}>{c.differsFromCurrent ? '与当前不同' : '与当前相同'}</Check> : '—'}</td>
          </tr>
          <tr>
            <td>平台内独占</td>
            <td>—</td>
            <td>{c ? (c.uniqueOnPlatform ? '无其他订阅使用' : '已被其他订阅使用') : '—'}</td>
            <td>{c ? <Check ok={c.uniqueOnPlatform}>{c.uniqueOnPlatform ? '独占' : '冲突'}</Check> : '—'}</td>
          </tr>
          <tr>
            <td>国家</td>
            <td>{s.plan.countryCode}（套餐）</td>
            <td>
              {p.observedCountryCode ?? '—'} {p.observedCity ?? ''}
            </td>
            <td>{c ? <Check ok={c.countryMatches}>{c.countryMatches ? '一致' : '不符'}</Check> : '—'}</td>
          </tr>
        </tbody>
      </table>
      {rejected ? (
        <Callout tone="stop">
          <b>候选被拒绝，不能提交</b>
          <ul className="reasons">
            {st.rejection!.map((r) => (
              <li key={r}>{r}</li>
            ))}
          </ul>
        </Callout>
      ) : (
        <Callout tone="info">三项都通过：候选出口 IP 与当前不同、平台内独占、国家与套餐一致。</Callout>
      )}
      <div className="actions">
        <Button icon={<ArrowLeftOutlined />} onClick={() => dispatch({ type: 'back' })}>
          换一个候选
        </Button>
        {!rejected && (
          <Button type="primary" onClick={() => dispatch({ type: 'proceed' })}>
            下一步：提交
          </Button>
        )}
      </div>
    </>
  )
}

function SubmitStep({ s, st, dispatch, requestId, defaultAlso }: { s: IspDetail; st: WizardState; dispatch: Dispatch; requestId?: string; defaultAlso: boolean }) {
  const api = useApi()
  const [reason, setReason] = useState('')
  const [also, setAlso] = useState(defaultAlso)
  const [busy, setBusy] = useState(false)
  const inflight = useRef(false)
  // 一次比较对应一次提交意图
  const key = useMemo(() => newIdempotencyKey(), [])
  const p = st.probe!
  const submit = async () => {
    if (inflight.current || !canSubmit(st) || !reason.trim()) return
    inflight.current = true
    setBusy(true)
    try {
      const t = await api.adminSubmitExitChange({
        subscriptionId: s.id,
        idempotencyKey: key,
        body: { resourceId: st.resourceId!, compareProbeId: p.id, alsoChangeAccess: also, exitRequestId: requestId, reason: reason.trim() },
      })
      dispatch({ type: 'submitted', task: t })
    } catch (e) {
      dispatch({ type: 'submitFailed', message: errorText(e) })
    } finally {
      inflight.current = false
      setBusy(false)
    }
  }
  return (
    <>
      <Callout tone="warn">
        <div className="impact-title">将会发生的变化</div>
        <ul className="impact-list">
          <li>
            出口 <Mono>{s.exit?.verifiedIp ?? '—'}</Mono> → <Mono>{p.observedIp}</Mono>（出口 v{s.exit?.version ?? 0} → v{(s.exit?.version ?? 0) + 1}）。切换期间订阅显示“变更中”。
          </li>
          <li>原上游释放并进入冷却期；候选资源被占用。切换后经客户入口复核，失败则回到原绑定，服务状态不变。</li>
          <li>{also ? `同时更换接入：端口与密码一起换（接入 v${s.access?.version} → v${(s.access?.version ?? 0) + 1}），旧凭据作废。` : '接入（端口与密码）不变。'}</li>
          {requestId && <li>完成后关联的换出口申请自动标记为成功。</li>}
        </ul>
      </Callout>
      <Checkbox checked={also} onChange={(e) => setAlso(e.target.checked)}>
        同时更换接入（组合更换）
      </Checkbox>
      <label className="field">
        <span className="field-label">
          原因<em>必填，写入审计与订阅事件</em>
        </span>
        <Input.TextArea aria-label="换出口原因" value={reason} onChange={(e) => setReason(e.target.value)} autoSize={{ minRows: 2 }} maxLength={300} />
      </label>
      {st.submitError && <Callout tone="stop">提交没有成功：{st.submitError}</Callout>}
      <div className="actions">
        <Button icon={<ArrowLeftOutlined />} onClick={() => dispatch({ type: 'back' })} disabled={busy}>
          换一个候选
        </Button>
        <Button type="primary" onClick={submit} loading={busy} disabled={!reason.trim() || !canSubmit(st)}>
          提交换出口
        </Button>
      </div>
    </>
  )
}

function ResultStep({ s, st, dispatch }: { s: IspDetail; st: WizardState; dispatch: Dispatch }) {
  const q = useTask(st.task!.id, st.task!)
  useEffect(() => {
    if (q.data) dispatch({ type: 'task', task: q.data })
  }, [q.data, dispatch])
  const t = q.data ?? st.task!
  const view = TASK_STATE[t.state]
  const running = t.state === 'queued' || t.state === 'running'
  return (
    <>
      <div className={`task-strip tone-${view.tone}`} role="status" aria-live="polite">
        <span className="state strong">
          {running ? <LoadingOutlined /> : <StateMark shape={view.shape} />}
          {TASK_KIND[t.kind]} · {view.label}
        </span>
        <span className="small">
          任务 <Mono>{t.id}</Mono>
        </span>
      </div>
      {t.state === 'succeeded' && (
        <Callout tone="info">
          换出口完成：新出口 <Mono>{st.probe?.observedIp}</Mono> 已经客户入口复核并生效（出口 v{t.newExitVersion}）{t.newAccessVersion ? `，接入已换到 v${t.newAccessVersion}` : ''}。
        </Callout>
      )}
      {t.state === 'failed' && (
        <Callout tone="stop">
          换出口失败（<Mono>{t.failureCode}</Mono>）：{t.failureReason}
        </Callout>
      )}
      <div className="actions">
        <Link to={`/subscriptions/${s.id}`}>
          <Button type={running ? 'default' : 'primary'}>回到订阅</Button>
        </Link>
        {t.state === 'failed' && <Button onClick={() => dispatch({ type: 'reset' })}>重新选择候选</Button>}
      </div>
    </>
  )
}
