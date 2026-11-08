// 换出口流程的状态机（docs/05 F-W3）：候选录入 → 验证 → 比较 → 提交 → 查结果。
// 只做纯状态迁移，接口调用在页面里；这样每一步能否前进、被拒原因怎么写都能单独测试。
// 规则（docs/01 §8）：候选必须经节点实测；出口 IP 与当前不同、平台内独占、国家与套餐一致，三项都过才能提交。
// 共享订阅（D-24）的“平台内独占”指不与独享订阅的出口同 IP；同一共享资源上的其他共享订阅不算冲突。
import type { ExitMode, Probe, Task, Upstream } from '@isp/api-client/admin'

export type WizardStep = 'candidate' | 'verify' | 'compare' | 'submit' | 'result'

export const STEPS: { key: WizardStep; label: string }[] = [
  { key: 'candidate', label: '候选录入' },
  { key: 'verify', label: '验证' },
  { key: 'compare', label: '比较' },
  { key: 'submit', label: '提交' },
  { key: 'result', label: '查结果' },
]

export interface WizardState {
  step: WizardStep
  resourceId: string | null
  probe: Probe | null
  /** 候选被拒的原因：发起比较时服务端直接拒绝，或比较结果没通过。空数组表示通过。 */
  rejection: string[] | null
  submitError: string | null
  task: Task | null
}

export type WizardAction =
  | { type: 'select'; resourceId: string | null }
  | { type: 'compareStarted'; probe: Probe }
  | { type: 'compareRefused'; message: string }
  | { type: 'probe'; probe: Probe }
  | { type: 'proceed' }
  | { type: 'submitted'; task: Task }
  | { type: 'submitFailed'; message: string }
  | { type: 'task'; task: Task }
  | { type: 'back' }
  | { type: 'reset' }

export const initialWizard: WizardState = { step: 'candidate', resourceId: null, probe: null, rejection: null, submitError: null, task: null }

/** 比较结果 → 被拒原因（逐条、可读）。通过时返回空数组。 */
export function rejectionReasons(probe: Probe, planCountryCode: string, exitMode: ExitMode = 'exclusive'): string[] {
  if (probe.status === 'expired') return ['节点没有在期限内回报验证结果（探测已过期），请稍后重试或换一个候选']
  if (probe.result === 'failed') return [`候选验证失败：${probe.error ?? '节点没有给出原因'}`]
  const c = probe.comparison
  if (!c) return ['没有拿到比较结果，请重新验证']
  const ip = probe.observedIp ?? '—'
  const out: string[] = []
  if (!c.differsFromCurrent) out.push(`候选出口 IP ${ip} 与当前出口相同：上游更新后出口没变，不能当作新出口`)
  if (!c.uniqueOnPlatform)
    out.push(
      exitMode === 'shared'
        ? `出口 IP ${ip} 已被平台内独享订阅使用：共享出口不能与独享出口同 IP`
        : `出口 IP ${ip} 已被平台内其他订阅使用，违反独占`,
    )
  if (!c.countryMatches) out.push(`国家不符：候选实测在 ${probe.observedCountryCode ?? '未知'}，套餐要求 ${planCountryCode}`)
  if (out.length === 0 && !c.passed) out.push('比较没有通过')
  return out
}

const finished = (p: Probe) => p.status === 'done' || p.status === 'expired'

/**
 * 候选是否符合订阅的出口类型（与服务端规则一致，docs/06 §9.1）：
 * 共享订阅只换到绑定上限 ≥2、没有独享占用且有空位的资源；独享订阅只换到没有任何占用的资源。
 */
export function candidateFits(u: Upstream, exitMode: ExitMode): boolean {
  const o = u.occupancy
  if (exitMode === 'shared') return u.maxTenants >= 2 && o.mode !== 'exclusive' && o.used < u.maxTenants
  return o.mode === 'none'
}

export function makeWizardReducer(planCountryCode: string, exitMode: ExitMode = 'exclusive') {
  return function reduce(s: WizardState, a: WizardAction): WizardState {
    switch (a.type) {
      case 'select':
        if (s.step !== 'candidate') return s
        return { ...s, resourceId: a.resourceId, rejection: null, probe: null }
      case 'compareStarted':
        if (s.step !== 'candidate' || !s.resourceId) return s
        return { ...s, step: finished(a.probe) ? 'compare' : 'verify', probe: a.probe, rejection: finished(a.probe) ? rejectionReasons(a.probe, planCountryCode, exitMode) : null }
      case 'compareRefused':
        if (s.step !== 'candidate') return s
        return { ...s, rejection: [a.message] }
      case 'probe':
        if (s.step !== 'verify' || s.probe?.id !== a.probe.id) return s
        if (!finished(a.probe)) return { ...s, probe: a.probe }
        return { ...s, step: 'compare', probe: a.probe, rejection: rejectionReasons(a.probe, planCountryCode, exitMode) }
      case 'proceed':
        if (s.step !== 'compare' || !s.rejection || s.rejection.length > 0) return s
        return { ...s, step: 'submit', submitError: null }
      case 'submitted':
        if (s.step !== 'submit') return s
        return { ...s, step: 'result', task: a.task, submitError: null }
      case 'submitFailed':
        if (s.step !== 'submit') return s
        return { ...s, submitError: a.message }
      case 'task':
        if (s.step !== 'result' || s.task?.id !== a.task.id) return s
        return { ...s, task: a.task }
      case 'back':
        if (s.step === 'result') return s
        return { ...s, step: 'candidate', probe: null, submitError: null, rejection: s.step === 'compare' ? s.rejection : null }
      case 'reset':
        return initialWizard
    }
  }
}

export function canSubmit(s: WizardState): boolean {
  return s.step === 'submit' && !!s.probe && s.probe.comparison?.passed === true
}
