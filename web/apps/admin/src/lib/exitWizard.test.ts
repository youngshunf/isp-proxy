// 换出口状态机：每一步能否前进、候选被拒的原因。
import { describe, expect, it } from 'vitest'
import type { Probe, Task, Upstream } from '@isp/api-client/admin'
import { canSubmit, candidateFits, initialWizard, makeWizardReducer, rejectionReasons, type WizardState } from './exitWizard'

const reduce = makeWizardReducer('us')

const probe = (p: Partial<Probe> = {}): Probe => ({
  id: 'prb_1',
  kind: 'candidate_compare',
  nodeId: 'nod_1',
  subscriptionId: 'sub_1',
  resourceId: 'res_1',
  status: 'done',
  result: 'ok',
  observedIp: '203.0.113.77',
  observedCountryCode: 'us',
  observedCity: '洛杉矶',
  latencyMs: 180,
  error: null,
  comparison: { currentIp: '203.0.113.48', differsFromCurrent: true, uniqueOnPlatform: true, countryMatches: true, passed: true },
  createdAt: '2026-11-07T02:00:00Z',
  completedAt: '2026-11-07T02:00:02Z',
  deadlineAt: '2026-11-07T02:01:00Z',
  ...p,
})

const task = (state: Task['state']): Task => ({
  id: 'tsk_1',
  subscriptionId: 'sub_1',
  kind: 'exit_change',
  state,
  requestedBy: { kind: 'admin', id: 'adm_1', email: 'ops@example.com' },
  orderId: null,
  exitRequestId: null,
  newAccessVersion: null,
  newExitVersion: 2,
  targetVersion: null,
  attempts: 1,
  failureCode: state === 'failed' ? 'exit_unverified' : null,
  failureReason: state === 'failed' ? '复核失败' : null,
  createdAt: '2026-11-07T02:00:00Z',
  startedAt: null,
  finishedAt: null,
})

function run(...actions: Parameters<typeof reduce>[1][]): WizardState {
  return actions.reduce(reduce, initialWizard)
}

describe('换出口状态机', () => {
  it('完整路径：候选录入 → 验证 → 比较 → 提交 → 查结果', () => {
    let s = run({ type: 'select', resourceId: 'res_1' })
    expect(s.step).toBe('candidate')
    s = reduce(s, { type: 'compareStarted', probe: probe({ status: 'queued', result: null, comparison: null, observedIp: null }) })
    expect(s.step).toBe('verify')
    s = reduce(s, { type: 'probe', probe: probe({ status: 'dispatched', result: null, comparison: null }) })
    expect(s.step).toBe('verify')
    s = reduce(s, { type: 'probe', probe: probe() })
    expect(s.step).toBe('compare')
    expect(s.rejection).toEqual([])
    s = reduce(s, { type: 'proceed' })
    expect(s.step).toBe('submit')
    expect(canSubmit(s)).toBe(true)
    s = reduce(s, { type: 'submitted', task: task('queued') })
    expect(s.step).toBe('result')
    s = reduce(s, { type: 'task', task: task('succeeded') })
    expect(s.task?.state).toBe('succeeded')
    // 已提交后不能回退到候选
    expect(reduce(s, { type: 'back' }).step).toBe('result')
  })

  it('没选候选不能开始验证；验证中不能换选', () => {
    expect(run({ type: 'compareStarted', probe: probe() }).step).toBe('candidate')
    const verifying = run({ type: 'select', resourceId: 'res_1' }, { type: 'compareStarted', probe: probe({ status: 'queued', comparison: null }) })
    expect(reduce(verifying, { type: 'select', resourceId: 'res_2' }).resourceId).toBe('res_1')
  })

  it('其他探测的回报不会推进当前流程', () => {
    const s = run({ type: 'select', resourceId: 'res_1' }, { type: 'compareStarted', probe: probe({ status: 'queued', comparison: null }) })
    expect(reduce(s, { type: 'probe', probe: probe({ id: 'prb_other' }) }).step).toBe('verify')
  })

  it('服务端拒绝发起比较：留在候选录入并写出原因', () => {
    const s = run({ type: 'select', resourceId: 'res_1' }, { type: 'compareRefused', message: '候选资源当前状态为“bound”，不能作为候选' })
    expect(s.step).toBe('candidate')
    expect(s.rejection).toEqual(['候选资源当前状态为“bound”，不能作为候选'])
    // 换一个候选后清掉旧原因
    expect(reduce(s, { type: 'select', resourceId: 'res_2' }).rejection).toBeNull()
  })

  it.each([
    ['与当前出口相同（上游已更新，出口未变）', { comparison: { currentIp: '203.0.113.77', differsFromCurrent: false, uniqueOnPlatform: true, countryMatches: true, passed: false } }, /与当前出口相同/],
    ['平台内已被占用', { comparison: { currentIp: '203.0.113.48', differsFromCurrent: true, uniqueOnPlatform: false, countryMatches: true, passed: false } }, /已被平台内其他订阅使用/],
    ['国家不符', { observedCountryCode: 'gb', comparison: { currentIp: '203.0.113.48', differsFromCurrent: true, uniqueOnPlatform: true, countryMatches: false, passed: false } }, /国家不符：候选实测在 gb，套餐要求 us/],
    ['节点验证失败', { result: 'failed' as const, error: '上游认证失败（407）', comparison: null, observedIp: null }, /候选验证失败：上游认证失败（407）/],
    ['探测过期', { status: 'expired' as const, result: null, comparison: null }, /探测已过期/],
  ])('候选被拒：%s', (_, patch, reason) => {
    const s = run({ type: 'select', resourceId: 'res_1' }, { type: 'compareStarted', probe: probe({ status: 'queued', comparison: null }) }, { type: 'probe', probe: probe(patch) })
    expect(s.step).toBe('compare')
    expect(s.rejection!.join('；')).toMatch(reason)
    // 被拒的候选不能进入提交
    const after = reduce(s, { type: 'proceed' })
    expect(after.step).toBe('compare')
    expect(canSubmit(after)).toBe(false)
    // 回到候选录入时保留原因，便于对照
    const back = reduce(s, { type: 'back' })
    expect(back.step).toBe('candidate')
    expect(back.rejection).toEqual(s.rejection)
  })

  it('同时多项不通过时逐条列出', () => {
    const reasons = rejectionReasons(
      probe({ observedCountryCode: 'gb', comparison: { currentIp: '203.0.113.77', differsFromCurrent: false, uniqueOnPlatform: false, countryMatches: false, passed: false } }),
      'us',
    )
    expect(reasons).toHaveLength(3)
  })

  it('提交失败留在提交步骤并显示原因；重置回到起点', () => {
    let s = run({ type: 'select', resourceId: 'res_1' }, { type: 'compareStarted', probe: probe() }, { type: 'proceed' })
    expect(s.step).toBe('submit')
    s = reduce(s, { type: 'submitFailed', message: '已有进行中的任务，完成后再试' })
    expect(s.step).toBe('submit')
    expect(s.submitError).toMatch(/进行中的任务/)
    expect(reduce(s, { type: 'reset' })).toEqual(initialWizard)
  })
})

describe('共享出口（D-24）', () => {
  const up = (maxTenants: number, occupancy: Upstream['occupancy']) => ({ maxTenants, occupancy }) as Upstream
  const none = { mode: 'none', used: 0, bound: 0, reserved: 0, cooldown: 0 } as const
  it('候选按订阅的出口类型过滤', () => {
    expect(candidateFits(up(1, none), 'shared')).toBe(false)
    expect(candidateFits(up(3, none), 'shared')).toBe(true)
    expect(candidateFits(up(3, { mode: 'shared', used: 2, bound: 1, reserved: 0, cooldown: 1 }), 'shared')).toBe(true)
    expect(candidateFits(up(3, { mode: 'shared', used: 3, bound: 3, reserved: 0, cooldown: 0 }), 'shared')).toBe(false)
    expect(candidateFits(up(3, { mode: 'exclusive', used: 1, bound: 1, reserved: 0, cooldown: 0 }), 'shared')).toBe(false)
    expect(candidateFits(up(3, none), 'exclusive')).toBe(true)
    expect(candidateFits(up(3, { mode: 'shared', used: 1, bound: 1, reserved: 0, cooldown: 0 }), 'exclusive')).toBe(false)
  })
  it('共享订阅的平台内独占被拒原因写明是与独享出口同 IP', () => {
    const p = probe({ comparison: { currentIp: '203.0.113.48', differsFromCurrent: true, uniqueOnPlatform: false, countryMatches: true, passed: false } })
    expect(rejectionReasons(p, 'us', 'shared')[0]).toMatch(/已被平台内独享订阅使用/)
    expect(rejectionReasons(p, 'us')[0]).toMatch(/违反独占/)
  })
})
