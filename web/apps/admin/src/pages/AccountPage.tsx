// 我的账号：查看身份与角色，凭验证码重新生成恢复码（旧恢复码全部作废，新码只显示一次）。
import { useQuery } from '@tanstack/react-query'
import { Button, Input } from 'antd'
import { useState } from 'react'
import { Plate, fmtDateTime } from '@isp/ui'
import { errorText, qk, useApi } from '../app/api'
import { Callout, CopyButton, Facts, Loading, PageHead } from '../components/bits'

export default function AccountPage() {
  const api = useApi()
  const me = useQuery({ queryKey: qk.me, queryFn: () => api.adminMe() })
  const [code, setCode] = useState('')
  const [codes, setCodes] = useState<string[] | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  if (!me.data) return <Loading />
  const a = me.data
  const regen = async () => {
    setBusy(true)
    setError(null)
    try {
      const r = await api.adminRegenerateRecoveryCodes({ body: { code } })
      setCodes(r.codes)
      setCode('')
    } catch (e) {
      setError(errorText(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <>
      <PageHead title="我的账号" />
      <div className="grid-2">
        <Plate title="身份">
          <div className="plate-pad">
            <Facts
              items={[
                ['名称', a.displayName],
                ['邮箱', a.email],
                ['角色', a.role === 'viewer' ? '只读' : '管理员'],
                ['验证器', a.totpEnrolled ? '已绑定' : '未绑定'],
                ['上次登录', fmtDateTime(a.lastLoginAt)],
              ]}
            />
          </div>
        </Plate>
        <Plate title="恢复码">
          <div className="plate-pad stack" style={{ gap: 12 }}>
            <p className="small">重新生成后，之前的恢复码全部作废。新恢复码只显示一次。</p>
            {codes ? (
              <>
                <ol className="codes" aria-label="新恢复码">
                  {codes.map((c) => (
                    <li key={c}>{c}</li>
                  ))}
                </ol>
                <span>
                  <CopyButton text={codes.join('\n')} label="复制全部恢复码" />
                </span>
              </>
            ) : (
              <div className="actions" style={{ alignItems: 'center' }}>
                <Input
                  aria-label="当前验证码"
                  className="otp-input"
                  inputMode="numeric"
                  maxLength={6}
                  placeholder="当前验证码"
                  value={code}
                  onChange={(e) => setCode(e.target.value.replace(/\D/g, ''))}
                  style={{ width: 180 }}
                />
                <Button onClick={regen} loading={busy} disabled={code.length !== 6}>
                  重新生成
                </Button>
              </div>
            )}
            {error && <Callout tone="stop">{error}</Callout>}
          </div>
        </Plate>
      </div>
    </>
  )
}
