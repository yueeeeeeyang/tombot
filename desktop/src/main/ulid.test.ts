import { describe, expect, it } from 'vitest'
import { ulid } from './ulid'

const pattern = /^[0-9A-HJKMNP-TV-Z]{26}$/

describe('客户端 ULID', () => {
  it('输出 26 位 Crockford Base32，不含歧义字符', () => {
    const value = ulid()
    expect(value).toMatch(pattern)
    expect(value).not.toMatch(/[ILOU]/)
  })

  it('按时间递增且同毫秒内不重复', () => {
    const earlier = ulid(1_700_000_000_000)
    const later = ulid(1_700_000_001_000)
    expect(later > earlier).toBe(true)
    const batch = new Set(Array.from({ length: 200 }, () => ulid(1_700_000_000_000)))
    expect(batch.size).toBe(200)
  })

  it('时间前缀可还原为毫秒时间戳', () => {
    const now = 1_760_000_000_123
    const encoded = ulid(now).slice(0, 10)
    let decoded = 0
    for (const character of encoded) {
      decoded = decoded * 32 + '0123456789ABCDEFGHJKMNPQRSTVWXYZ'.indexOf(character)
    }
    expect(decoded).toBe(now)
  })
})
