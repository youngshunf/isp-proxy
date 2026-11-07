// 幂等键：一次用户意图一个键（docs/05 §6 第 4 步）。
// 同一请求体重试（网络失败、重复点击）复用同一个键，服务端返回首次结果；请求体变了（换了套餐、时长）就换新键，
// 避免“同键不同体”的 409；成功后调用 reset()，下一次提交是新的意图。
import { useCallback, useRef } from 'react'
import { newIdempotencyKey } from '@isp/api-client'

export function useIntentKey() {
  const ref = useRef<{ sig: string; key: string } | null>(null)
  const keyFor = useCallback((body: unknown): string => {
    const sig = JSON.stringify(body)
    if (!ref.current || ref.current.sig !== sig) ref.current = { sig, key: newIdempotencyKey() }
    return ref.current.key
  }, [])
  const reset = useCallback(() => {
    ref.current = null
  }, [])
  return { keyFor, reset }
}
