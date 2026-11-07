import type { ThemeConfig } from 'antd'

// 与 styles/global.css 的 token 保持同值。
const FONT =
  "'Barlow', 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei UI', 'Microsoft YaHei', 'Noto Sans CJK SC', 'Source Han Sans SC', system-ui, sans-serif"
const MONO = "'Red Hat Mono', ui-monospace, 'SF Mono', Menlo, Consolas, monospace"

export const theme: ThemeConfig = {
  token: {
    colorPrimary: '#00505c',
    colorInfo: '#00505c',
    colorLink: '#00505c',
    colorSuccess: '#1d7a4f',
    colorWarning: '#8f5500',
    colorError: '#b3261e',
    colorText: '#14231f',
    colorTextSecondary: '#43524d',
    colorTextTertiary: '#5c6b66',
    colorTextPlaceholder: '#6b7a75',
    colorBorder: '#c3cdc9',
    colorBorderSecondary: '#e4e9e7',
    colorBgLayout: '#eef1f0',
    colorBgContainer: '#ffffff',
    colorFillAlter: '#f5f8f7',
    borderRadius: 2,
    borderRadiusLG: 2,
    borderRadiusSM: 2,
    fontFamily: FONT,
    fontFamilyCode: MONO,
    fontSize: 14,
    controlHeight: 36,
    controlOutline: 'rgba(0, 80, 92, 0.18)',
    motionDurationMid: '0.18s',
  },
  components: {
    Button: { primaryShadow: 'none', defaultShadow: 'none', dangerShadow: 'none', fontWeight: 500 },
    Modal: { titleFontSize: 18 },
    Alert: { colorInfoBg: '#e3eef0', colorInfoBorder: '#cde2e5', colorInfo: '#00505c' },
    Input: { activeShadow: '0 0 0 3px rgba(0, 80, 92, 0.14)' },
  },
}
