// 进行中 / 刚结束的任务条：轮询到终态，失败时写出失败码与原因。
import { LoadingOutlined } from '@ant-design/icons'
import type { Task } from '@isp/api-client/admin'
import { StateMark } from '@isp/ui'
import { useTask } from '../lib/jobs'
import { TASK_KIND, TASK_STATE } from '../lib/status'
import { Ago, Mono } from './bits'

export function TaskStrip({ taskId, initial }: { taskId: string; initial?: Task }) {
  const q = useTask(taskId, initial)
  const t = q.data
  if (!t) return null
  const view = TASK_STATE[t.state]
  const running = t.state === 'queued' || t.state === 'running'
  return (
    <div className={`task-strip tone-${view.tone}`} role="status" aria-live="polite">
      <span className="state strong">
        {running ? <LoadingOutlined /> : <StateMark shape={view.shape} />}
        {TASK_KIND[t.kind]} · {view.label}
      </span>
      <span className="small">
        任务 <Mono>{t.id}</Mono> · 第 {t.attempts} 次尝试 · 创建 <Ago at={t.createdAt} />
        {t.newAccessVersion ? ` · 接入 → v${t.newAccessVersion}` : ''}
        {t.newExitVersion ? ` · 出口 → v${t.newExitVersion}` : ''}
      </span>
      {t.state === 'failed' && (
        <span className="small">
          失败 <Mono>{t.failureCode ?? '—'}</Mono>：{t.failureReason}
        </span>
      )}
    </div>
  )
}
