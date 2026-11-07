// 格式化来自 @isp/ui（两端共用）；这里只把“几分钟前”绑定到前台的服务端时间。
import { fmtAgo as fmtAgoAt } from '@isp/ui'
import { serverNow } from '../app/api'

export {
  TZ_LABEL,
  addDays,
  fmtBytes,
  fmtDate,
  fmtDay,
  fmtMoney,
  splitBytes,
  splitDuration,
} from '@isp/ui'

export function fmtAgo(isoStr: string): string {
  return fmtAgoAt(isoStr, serverNow())
}
