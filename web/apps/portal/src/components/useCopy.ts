import { useCallback, useEffect, useRef, useState } from 'react'

async function writeClipboard(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    // 非安全上下文或权限被拒时退回到选区复制。
    const ta = document.createElement('textarea')
    ta.value = text
    ta.setAttribute('readonly', '')
    ta.style.position = 'fixed'
    ta.style.opacity = '0'
    document.body.appendChild(ta)
    ta.select()
    const ok = document.execCommand('copy')
    ta.remove()
    return ok
  }
}

/** 复制并在 1.6 秒内标记“已复制”的键；同时给读屏器一条播报。 */
export function useCopy() {
  const [copied, setCopied] = useState<string | null>(null)
  const [announce, setAnnounce] = useState('')
  const timer = useRef<number | undefined>(undefined)
  useEffect(() => () => window.clearTimeout(timer.current), [])
  const copy = useCallback(async (key: string, text: string, spoken: string) => {
    const ok = await writeClipboard(text)
    setCopied(ok ? key : null)
    setAnnounce(ok ? `已复制${spoken}` : '复制失败，请长按或手动选择文本')
    window.clearTimeout(timer.current)
    timer.current = window.setTimeout(() => setCopied(null), 1600)
    return ok
  }, [])
  return { copied, announce, copy }
}
