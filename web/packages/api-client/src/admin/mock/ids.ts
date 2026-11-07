// 确定性 ID：形状与真实 ID 一致（前缀_ + 20 位小写 Crockford base32，docs/06 §8.1），种子相同则结果相同，截图与测试稳定。
const ALPHABET = '0123456789abcdefghjkmnpqrstvwxyz'

function hash(seed: string): number {
  let h = 0x811c9dc5
  for (let i = 0; i < seed.length; i++) {
    h ^= seed.charCodeAt(i)
    h = Math.imul(h, 0x01000193)
  }
  return h >>> 0
}

/** 由种子生成的伪随机序列（xorshift32）。 */
export function rng(seed: string): () => number {
  let x = hash(seed) || 1
  return () => {
    x ^= x << 13
    x ^= x >>> 17
    x ^= x << 5
    return (x >>> 0) / 0x1_0000_0000
  }
}

export function fid(prefix: string, seed: string): string {
  const r = rng(`${prefix}:${seed}`)
  let out = ''
  for (let i = 0; i < 20; i++) out += ALPHABET[Math.floor(r() * 32)]
  return `${prefix}_${out}`
}

/** 付款识别码（6 位，去掉易混字符）。 */
export function payRefOf(seed: string): string {
  const r = rng(`payref:${seed}`)
  const chars = 'ABCDEFGHJKMNPQRSTVWXYZ23456789'
  let out = ''
  for (let i = 0; i < 6; i++) out += chars[Math.floor(r() * chars.length)]
  return out
}

export function secretOf(seed: string, len = 12): string {
  const r = rng(`secret:${seed}`)
  const chars = 'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789'
  let out = ''
  for (let i = 0; i < len; i++) out += chars[Math.floor(r() * chars.length)]
  return out
}
