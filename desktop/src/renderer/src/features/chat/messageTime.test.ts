import { describe, expect, it } from 'vitest'
import { formatMessageTime, MESSAGE_TIME_GAP_MS, messageTimeSeparator } from './messageTime'

const now = new Date(2026, 10, 10, 18, 0, 0)

function at(day: number, hour: number, minute: number, year = 2026, month = 10): string {
  return new Date(year, month, day, hour, minute, 0, 0).toISOString()
}

describe('微信式消息时间', () => {
  it('间隔常量是 5 分钟', () => {
    expect(MESSAGE_TIME_GAP_MS).toBe(5 * 60 * 1000)
  })

  it('按今天、昨天、一周内和更早格式化', () => {
    expect(formatMessageTime(new Date(2026, 10, 10, 15, 4), now)).toBe('15:04')
    expect(formatMessageTime(new Date(2026, 10, 9, 9, 5), now)).toBe('昨天 09:05')
    expect(formatMessageTime(new Date(2026, 10, 8, 12, 0), now)).toBe('星期日 12:00')
    expect(formatMessageTime(new Date(2026, 10, 4, 15, 4), now)).toBe('星期三 15:04')
    expect(formatMessageTime(new Date(2026, 10, 3, 8, 0), now)).toBe('2026年11月3日 08:00')
    expect(formatMessageTime(new Date(2025, 11, 1, 7, 6), now)).toBe('2025年12月1日 07:06')
    expect(formatMessageTime(new Date(2026, 10, 12, 1, 2), now)).toBe('2026年11月12日 01:02')
  })

  it('首条必显示，超过 5 分钟才再显示，恰好 5 分钟或更早的时间不插入', () => {
    expect(messageTimeSeparator(at(10, 18, 0), undefined, now)).toBe('18:00')
    const fiveMinutes = new Date(now.getTime() - MESSAGE_TIME_GAP_MS).toISOString()
    expect(messageTimeSeparator(now.toISOString(), fiveMinutes, now)).toBeUndefined()
    const justOver = new Date(now.getTime() - MESSAGE_TIME_GAP_MS - 1).toISOString()
    expect(messageTimeSeparator(now.toISOString(), justOver, now)).toBe('18:00')
    expect(messageTimeSeparator(at(10, 18, 0), at(10, 18, 4), now)).toBeUndefined()
  })

  it('跨天但间隔不超过 5 分钟时不另起一条时间', () => {
    const midnight = new Date(2026, 10, 10, 0, 2, 0)
    const previous = new Date(2026, 10, 9, 23, 58, 0).toISOString()
    expect(messageTimeSeparator(midnight.toISOString(), previous, midnight)).toBeUndefined()
    expect(messageTimeSeparator(previous, undefined, midnight)).toBe('昨天 23:58')
  })
})
