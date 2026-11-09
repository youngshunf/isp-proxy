// 国家与地区：后台录入上游、套餐时选择。代码是 ISO 3166-1 alpha-2 小写——出口验证按 IP 归属地比对这个代码，必须写对。
// 常用列表按上游供应商常见的地区排列；其余国家和地区的中文名由浏览器 Intl.DisplayNames 给出。

export interface Country {
  value: string
  label: string
}

/** 常用国家和地区（排在下拉最前面）。 */
export const COMMON_COUNTRIES: Country[] = [
  { value: 'us', label: '美国' },
  { value: 'nl', label: '荷兰' },
  { value: 'gb', label: '英国' },
  { value: 'ca', label: '加拿大' },
  { value: 'es', label: '西班牙' },
  { value: 'fr', label: '法国' },
  { value: 'de', label: '德国' },
  { value: 'au', label: '澳大利亚' },
  { value: 'br', label: '巴西' },
  { value: 'jp', label: '日本' },
  { value: 'it', label: '意大利' },
  { value: 'id', label: '印度尼西亚' },
  { value: 'in', label: '印度' },
  { value: 'vn', label: '越南' },
  { value: 'hk', label: '中国香港' },
  { value: 'ae', label: '阿联酋（迪拜）' },
  { value: 'th', label: '泰国' },
  { value: 'my', label: '马来西亚' },
  { value: 'tw', label: '中国台湾' },
  { value: 'ph', label: '菲律宾' },
  { value: 'mx', label: '墨西哥' },
  { value: 'kr', label: '韩国' },
  { value: 'sg', label: '新加坡' },
  { value: 'sa', label: '沙特阿拉伯' },
]

// ISO 3166-1 alpha-2 全部正式代码。
const ISO_CODES =
  'ad ae af ag ai al am ao aq ar as at au aw ax az ba bb bd be bf bg bh bi bj bl bm bn bo bq br bs bt bv bw by bz ' +
  'ca cc cd cf cg ch ci ck cl cm cn co cr cu cv cw cx cy cz de dj dk dm do dz ec ee eg eh er es et fi fj fk fm fo fr ' +
  'ga gb gd ge gf gg gh gi gl gm gn gp gq gr gs gt gu gw gy hk hm hn hr ht hu id ie il im in io iq ir is it je jm jo jp ' +
  'ke kg kh ki km kn kp kr kw ky kz la lb lc li lk lr ls lt lu lv ly ma mc md me mf mg mh mk ml mm mn mo mp mq mr ms mt ' +
  'mu mv mw mx my mz na nc ne nf ng ni nl no np nr nu nz om pa pe pf pg ph pk pl pm pn pr ps pt pw py qa re ro rs ru rw ' +
  'sa sb sc sd se sg sh si sj sk sl sm sn so sr ss st sv sx sy sz tc td tf tg th tj tk tl tm tn to tr tt tv tw tz ua ug ' +
  'um us uy uz va vc ve vg vi vn vu wf ws ye yt za zm zw'

// 浏览器给的名称里有几个不合适，统一改写。
const NAME_OVERRIDE: Record<string, string> = { mo: '中国澳门', tw: '中国台湾', hk: '中国香港' }

const COMMON_LABEL = new Map(COMMON_COUNTRIES.map((c) => [c.value, c.label]))

let display: Intl.DisplayNames | null | undefined
function intlName(code: string): string | undefined {
  if (display === undefined) {
    try {
      display = new Intl.DisplayNames(['zh-CN'], { type: 'region' })
    } catch {
      display = null
    }
  }
  try {
    const n = display?.of(code.toUpperCase())
    return n && n.toUpperCase() !== code.toUpperCase() ? n : undefined
  } catch {
    return undefined
  }
}

/** 代码对应的中文名；不认识的代码返回 undefined。 */
export function countryName(code: string | undefined | null): string | undefined {
  if (!code) return undefined
  const c = code.toLowerCase()
  return COMMON_LABEL.get(c) ?? NAME_OVERRIDE[c] ?? intlName(c)
}

/** 常用之外的其他国家和地区，按中文名排序。 */
export const OTHER_COUNTRIES: Country[] = ISO_CODES.split(' ')
  .filter((c) => !COMMON_LABEL.has(c))
  .map((c) => ({ value: c, label: countryName(c) ?? c.toUpperCase() }))
  .sort((a, b) => a.label.localeCompare(b.label, 'zh-CN'))

const KNOWN = new Set(ISO_CODES.split(' '))

/** 是否是列表里的代码（常用或其他）。 */
export function isListedCountry(code: string | undefined): boolean {
  return !!code && KNOWN.has(code.toLowerCase())
}

/** 下拉选项：常用在前，其余按中文名排序。 */
export const COUNTRY_OPTIONS = [
  { label: '常用', options: COMMON_COUNTRIES },
  { label: '其他国家和地区', options: OTHER_COUNTRIES },
]

/** 下拉搜索：中文名或代码都能搜。分组标题本身一律不算命中（命中会把整组都留下），只按组内选项过滤。 */
export function matchCountry(input: string, option?: { value?: unknown; label?: unknown }): boolean {
  const q = input.trim().toLowerCase()
  if (!option || typeof option.value !== 'string') return false
  if (!q) return true
  return option.value.includes(q) || String(option.label ?? '').toLowerCase().includes(q)
}
