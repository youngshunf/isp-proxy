// @isp/ui：两端共用的设计系统部件（DESIGN.md）。只放真实出现重复的东西：token、主题、格式化、切角容器、状态点、余量条、国旗。
// 样式：`import '@isp/ui/styles.css'`；字体：`import '@isp/ui/fonts'`。
export type { MarkShape, StateView, Tone } from './tone'
export { Plate } from './Plate'
export { CountryFlag, EXIT_MODE_LABEL, ExitModeChip, SignalBars, StateMark } from './marks'
export { Meter } from './Meter'
export { theme, FONT_MONO, FONT_SANS } from './theme'
export {
  GB,
  MB,
  TZ_LABEL,
  addDays,
  fmtAgo,
  fmtBytes,
  fmtBytesExact,
  fmtBytesFine,
  fmtDate,
  fmtDateTime,
  fmtDay,
  fmtMoney,
  fmtRate,
  fmtSpan,
  fmtTime,
  splitBytes,
  splitDuration,
  type MoneyLike,
} from './format'
