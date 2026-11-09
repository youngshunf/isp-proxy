// 国家选择：可搜索的下拉（常用在前，中文名或代码都能搜）；列表里没有时切到手动输入代码与名称。
// 代码必须是 ISO 3166-1 两位字母：出口验证按 IP 归属地比对它。
import { Button, Input, Select } from 'antd'
import { useState } from 'react'
import { COUNTRY_OPTIONS, countryName, isListedCountry, matchCountry } from '../lib/countries'

export interface CountryValue {
  code: string
  name: string
}

export const CODE_RE = /^[a-z]{2}$/

/** 值是否可以提交：代码两位字母，名称非空。 */
export function countryReady(v: CountryValue): boolean {
  return CODE_RE.test(v.code) && v.name.trim().length > 0
}

export function CountryField({ value, onChange, style }: { value: CountryValue; onChange: (v: CountryValue) => void; style?: React.CSSProperties }) {
  // 已有值不在列表里（以前手动输入的）时直接显示手动输入。
  const [manual, setManual] = useState(() => !!value.code && !isListedCountry(value.code))
  const codeBad = value.code !== '' && !CODE_RE.test(value.code)
  return (
    <div className="field" style={style}>
      <span className="field-label">
        国家
        <Button
          type="link"
          size="small"
          style={{ padding: 0, height: 'auto', marginLeft: 8 }}
          onClick={() => {
            // 切到手动输入时从空白开始（列表里没有才手动）；切回列表时丢掉列表里没有的代码。
            if (!manual || !isListedCountry(value.code)) onChange({ code: '', name: '' })
            setManual(!manual)
          }}
        >
          {manual ? '从列表选择' : '列表里没有？手动输入'}
        </Button>
      </span>
      {manual ? (
        <div style={{ display: 'flex', gap: 8 }}>
          <Input
            aria-label="国家代码"
            className="mono"
            placeholder="代码，如 ph"
            maxLength={2}
            value={value.code}
            status={codeBad ? 'error' : undefined}
            onChange={(e) => {
              const code = e.target.value.trim().toLowerCase()
              // 认识的代码顺手填上中文名；名称已手动改过就不覆盖。
              const auto = countryName(code)
              const prevAuto = countryName(value.code)
              onChange({ code, name: auto && (!value.name || value.name === prevAuto) ? auto : value.name })
            }}
            style={{ width: 110 }}
          />
          <Input aria-label="国家名称" placeholder="显示名称，如 菲律宾" maxLength={32} value={value.name} onChange={(e) => onChange({ ...value, name: e.target.value })} />
        </div>
      ) : (
        <Select
          aria-label="国家"
          showSearch
          placeholder="搜索中文名或代码"
          value={value.code || undefined}
          filterOption={matchCountry}
          onChange={(code: string) => onChange({ code, name: countryName(code) ?? code.toUpperCase() })}
          options={COUNTRY_OPTIONS}
          optionRender={(o) => (
            <span>
              {o.label} <span className="muted small mono">{String(o.value).toUpperCase()}</span>
            </span>
          )}
        />
      )}
      {manual && (
        <span className="muted small" style={{ marginTop: 4 }}>
          代码是 ISO 两位字母（菲律宾 ph、阿联酋 ae）。验证出口时按 IP 归属地比对这个代码，填错会验证不通过。
        </span>
      )}
    </div>
  )
}
