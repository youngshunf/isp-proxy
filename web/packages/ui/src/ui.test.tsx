import { cleanup, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it } from 'vitest'
import { CountryFlag, Meter, StateMark, fmtBytes, fmtBytesFine, fmtRate, fmtSpan } from './index'

afterEach(cleanup)

describe('格式化', () => {
  it('字节按十进制 GB，负值带减号', () => {
    expect(fmtBytes(61_800_000_000)).toBe('61.8 GB')
    expect(fmtBytes(0)).toBe('0 GB')
    expect(fmtBytes(-10_000_000_000)).toBe('−10 GB')
    expect(fmtBytesFine(40_000_000)).toBe('40.0 MB')
  })
  it('时长条目带符号', () => {
    expect(fmtSpan(30 * 86_400, true)).toBe('+30 天')
    expect(fmtSpan(-2 * 3_600)).toBe('−2 小时')
    expect(fmtSpan(26 * 3_600)).toBe('1 天 2 小时')
  })
  it('速率按比特', () => {
    expect(fmtRate(2_500_000)).toBe('20.0 Mbps')
  })
})

describe('部件', () => {
  it('余量条带完整读数的 meter 角色', () => {
    render(<Meter label="剩余流量" value="38.2" unit="GB" total="100 GB" ratio={0.382} />)
    expect(screen.getByRole('meter', { name: '剩余流量：38.2 GB，共 100 GB' })).toBeTruthy()
  })
  it('国旗有国家名，未知国家显示代码', () => {
    render(<CountryFlag code="zz" name="未知地区" />)
    expect(screen.getByRole('img', { name: '未知地区' }).textContent).toBe('ZZ')
  })
  it('状态点只是装饰，不进入无障碍树', () => {
    const { container } = render(<StateMark shape="half" />)
    expect(container.querySelector('svg')?.getAttribute('aria-hidden')).toBe('true')
  })
})
