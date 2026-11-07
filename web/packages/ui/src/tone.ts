/** 语义色调：ok 成立、warn 留意、stop 阻断、wait 处理中/等待（深青）。只报告状态，不做按钮底色。 */
export type Tone = 'ok' | 'warn' | 'stop' | 'wait'

/** 状态点形状：实心=成立，空心=不成立，半实心=处理中。形状本身携带含义，不只靠颜色。 */
export type MarkShape = 'filled' | 'hollow' | 'half'

export interface StateView {
  label: string
  tone: Tone
  shape: MarkShape
}
