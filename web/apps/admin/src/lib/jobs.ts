// 异步任务与探测的轮询（docs/06 §7）：提交后拿到 Task / Probe，按 1.5 秒轮询到终态为止。
// 任务结束后把相关订阅的缓存标记过期，详情页自然刷新出新版本。
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect } from 'react'
import type { Probe, Task } from '@isp/api-client/admin'
import { qk, useApi } from '../app/api'

export const POLL_MS = 1500

export const taskDone = (t: Pick<Task, 'state'> | undefined) => !!t && (t.state === 'succeeded' || t.state === 'failed' || t.state === 'cancelled')
export const probeDone = (p: Pick<Probe, 'status'> | undefined) => !!p && (p.status === 'done' || p.status === 'expired')

export function useTask(taskId: string | null | undefined, initial?: Task) {
  const api = useApi()
  const qc = useQueryClient()
  const q = useQuery({
    queryKey: qk.task(taskId ?? '-'),
    queryFn: () => api.adminGetTask({ taskId: taskId! }),
    enabled: !!taskId,
    initialData: initial && initial.id === taskId ? initial : undefined,
    staleTime: 0,
    refetchInterval: (query) => (taskDone(query.state.data) ? false : POLL_MS),
  })
  const done = taskDone(q.data)
  const subId = q.data?.subscriptionId
  useEffect(() => {
    if (done && subId) {
      void qc.invalidateQueries({ queryKey: qk.sub(subId) })
      void qc.invalidateQueries({ queryKey: qk.subs, exact: false })
      void qc.invalidateQueries({ queryKey: qk.overview })
    }
  }, [done, subId, qc])
  return q
}

export function useProbe(probeId: string | null | undefined, initial?: Probe) {
  const api = useApi()
  return useQuery({
    queryKey: qk.probe(probeId ?? '-'),
    queryFn: () => api.adminGetProbe({ probeId: probeId! }),
    enabled: !!probeId,
    initialData: initial && initial.id === probeId ? initial : undefined,
    staleTime: 0,
    refetchInterval: (query) => (probeDone(query.state.data) ? false : POLL_MS),
  })
}
