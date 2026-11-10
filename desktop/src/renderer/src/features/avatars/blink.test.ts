import { describe, expect, it } from 'vitest'
import {
  BLINK_CLOSED_MS,
  BLINK_GAP_MAX_MS,
  BLINK_GAP_MIN_MS,
  BLINK_HALF_IN_MS,
  BLINK_SEQUENCE_MS,
  DOUBLE_BLINK_PAUSE_MS,
  advanceBlinkSchedule,
  blinkDelayMs,
  createBlinkSchedule,
  eyeForPhase,
  initialBlinkDelayMs,
} from './blink'

describe('眨眼节奏', () => {
  it('单次眨眼按半闭、闭合、半闭走完，总时长在 150 到 200 毫秒', () => {
    expect(BLINK_SEQUENCE_MS).toBeGreaterThanOrEqual(150)
    expect(BLINK_SEQUENCE_MS).toBeLessThanOrEqual(200)
    let schedule = createBlinkSchedule(0, 1_000)
    expect(eyeForPhase(schedule.phase)).toBe('open')
    schedule = advanceBlinkSchedule(schedule, 999, () => 0.9)
    expect(schedule.phase).toBe('waiting')
    schedule = advanceBlinkSchedule(schedule, 1_000, () => 0.9)
    expect(eyeForPhase(schedule.phase)).toBe('half')
    schedule = advanceBlinkSchedule(schedule, 1_000 + BLINK_HALF_IN_MS, () => 0.9)
    expect(eyeForPhase(schedule.phase)).toBe('closed')
    schedule = advanceBlinkSchedule(schedule, 1_000 + BLINK_HALF_IN_MS + BLINK_CLOSED_MS, () => 0.9)
    expect(eyeForPhase(schedule.phase)).toBe('half')
    const openedAt = 1_000 + BLINK_SEQUENCE_MS
    schedule = advanceBlinkSchedule(schedule, openedAt, () => 0.9)
    expect(schedule.phase).toBe('waiting')
    expect(schedule.phaseEndsAt - openedAt).toBe(blinkDelayMs(0.9))
  })

  it('偶尔连眨两下，中间会睁开一小段', () => {
    let schedule = createBlinkSchedule(0, 100)
    schedule = advanceBlinkSchedule(schedule, 100, () => 0)
    expect(schedule.doubleBlinkPending).toBe(true)
    schedule = advanceBlinkSchedule(schedule, schedule.phaseEndsAt, () => 0)
    schedule = advanceBlinkSchedule(schedule, schedule.phaseEndsAt, () => 0)
    schedule = advanceBlinkSchedule(schedule, schedule.phaseEndsAt, () => 0)
    expect(schedule.phase).toBe('pause')
    expect(eyeForPhase(schedule.phase)).toBe('open')
    expect(schedule.phaseEndsAt).toBe(100 + BLINK_SEQUENCE_MS + DOUBLE_BLINK_PAUSE_MS)
    schedule = advanceBlinkSchedule(schedule, schedule.phaseEndsAt, () => 0)
    expect(schedule.phase).toBe('half-in')
    expect(schedule.doubleBlinkPending).toBe(false)
  })

  it('不同猫咪的第一下眨眼错开，间隔限制在 3 到 7 秒', () => {
    const assistant = initialBlinkDelayMs('agent:assistant')
    const developer = initialBlinkDelayMs('agent:developer')
    expect(assistant).not.toBe(developer)
    for (const delay of [assistant, developer, blinkDelayMs(0), blinkDelayMs(0.999)]) {
      expect(delay).toBeGreaterThanOrEqual(BLINK_GAP_MIN_MS)
      expect(delay).toBeLessThan(BLINK_GAP_MAX_MS)
    }
  })

  it('拒绝无效时钟、间隔和随机数', () => {
    expect(() => createBlinkSchedule(Number.NaN, 10)).toThrow('猫咪动作时钟无效')
    expect(() => createBlinkSchedule(0, -1)).toThrow('眨眼间隔无效')
    expect(() => blinkDelayMs(1)).toThrow('随机数无效')
    expect(() => initialBlinkDelayMs('   ')).toThrow('猫咪动作标识无效')
    expect(() => advanceBlinkSchedule(createBlinkSchedule(0, 10), Number.NaN, () => 0.2)).toThrow(
      '猫咪动作时钟无效',
    )
  })
})
