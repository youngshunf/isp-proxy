import { Button } from 'antd'
import { useNavigate } from 'react-router'
import { remainingBytes, type Subscription } from '@isp/api-client'
import { fmtBytes, fmtDate, fmtDay, splitBytes, splitDuration } from '../lib/format'
import { isExpiring, isLowTraffic } from '../lib/status'
import { Meter } from '@isp/ui'

function GoButton({ to, children, primary }: { to: string; children: string; primary?: boolean }) {
  const navigate = useNavigate()
  return (
    <Button size="small" type={primary ? 'primary' : 'default'} onClick={() => navigate(to)}>
      {children}
    </Button>
  )
}

/** 套餐余量：时长与流量分两条，各自给出补救按钮。卡片与详情页共用。 */
export function Balances({ sub, detailed = false }: { sub: Subscription; detailed?: boolean }) {
  const renewTime = `/buy/renew/${sub.id}?kind=time`
  const buyTraffic = `/buy/renew/${sub.id}?kind=traffic`
  const pending = sub.lifecycle === 'pending' || sub.lifecycle === 'fulfill_failed'
  const expired = sub.blockers.includes('expired') || sub.blockers.includes('released')
  const exhausted = sub.blockers.includes('traffic_exhausted')

  const periodSeconds = sub.time.periodDays * 86_400
  const d = splitDuration(sub.time.remainingSeconds)
  const time = pending ? (
    <Meter
      label="剩余时长"
      value={String(sub.time.periodDays)}
      unit="天"
      ratio={1}
      tone="wait"
      note={<div className="meter-note tone-wait">开通就绪后开始计时</div>}
    />
  ) : (
    <Meter
      label="剩余时长"
      value={d.value}
      unit={d.unit}
      rest={d.rest}
      total={`${sub.time.periodDays} 天`}
      ratio={sub.time.remainingSeconds / periodSeconds}
      tone={expired ? 'stop' : isExpiring(sub) ? 'warn' : 'ok'}
      note={
        expired ? (
          <div className="meter-note tone-stop">
            <span>
              {fmtDay(sub.time.expiresAt)} 已到期
              {sub.time.retentionEndsAt ? ` · 出口与接入保留到 ${fmtDay(sub.time.retentionEndsAt)}` : ''}
            </span>
            <GoButton to={renewTime} primary>
              续时长
            </GoButton>
          </div>
        ) : isExpiring(sub) ? (
          <div className="meter-note tone-warn">
            <span>{fmtDate(sub.time.expiresAt)} 到期</span>
            <GoButton to={renewTime}>续时长</GoButton>
          </div>
        ) : detailed ? (
          <div className="meter-note">
            <span className="plate-note">到期时间 {fmtDate(sub.time.expiresAt)}</span>
          </div>
        ) : null
      }
    />
  )

  const left = remainingBytes(sub)
  const b = splitBytes(left)
  const traffic = (
    <Meter
      label="剩余流量"
      value={b.value}
      unit={b.unit}
      total={fmtBytes(sub.traffic.totalBytes)}
      ratio={sub.traffic.totalBytes > 0 ? left / sub.traffic.totalBytes : 0}
      tone={pending ? 'wait' : exhausted ? 'stop' : isLowTraffic(sub) ? 'warn' : 'ok'}
      note={
        exhausted ? (
          <div className="meter-note tone-stop">
            <span>已用尽 · 到期时间不变</span>
            <GoButton to={buyTraffic} primary>
              买流量
            </GoButton>
          </div>
        ) : isLowTraffic(sub) ? (
          <div className="meter-note tone-warn">
            <span>剩余不足 10%</span>
            <GoButton to={buyTraffic}>买流量</GoButton>
          </div>
        ) : expired ? (
          <div className="meter-note">
            <span className="plate-note">余量保留，不随到期清零</span>
          </div>
        ) : detailed ? (
          <div className="meter-note">
            <span className="plate-note">
              已用 {fmtBytes(sub.traffic.usedBytes)} · 长期保留，不按月清零
            </span>
          </div>
        ) : null
      }
    />
  )

  return (
    <>
      {time}
      {traffic}
    </>
  )
}
