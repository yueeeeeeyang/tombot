import { describe, expect, it } from 'vitest'
import { isLookSpringSettled, stepLookSpring, type SpringState } from './spring'

const rest: SpringState = { value: 0, velocity: 0 }

describe('视线弹簧', () => {
  it('一小步会靠近目标，但不会一帧贴上去', () => {
    const next = stepLookSpring(rest, 1, 0.016)
    expect(next.value).toBeGreaterThan(0)
    expect(next.value).toBeLessThan(0.2)
    expect(next.velocity).toBeGreaterThan(0)
  })

  it('连续跟随后停在目标附近，超调有限', () => {
    let state = rest
    let peak = 0
    for (let step = 0; step < 90; step += 1) {
      state = stepLookSpring(state, 1, 0.016)
      peak = Math.max(peak, state.value)
    }
    expect(peak).toBeLessThan(1.2)
    expect(isLookSpringSettled(state, 1)).toBe(true)
  })

  it('过长的时间间隔只走上限那一步', () => {
    const capped = stepLookSpring(rest, 1, 10)
    const oneStep = stepLookSpring(rest, 1, 0.032)
    expect(capped).toEqual(oneStep)
    expect(capped.value).toBeLessThan(0.5)
  })

  it('零步长保持原状态', () => {
    const moving = { value: 0.4, velocity: 1.2 }
    expect(stepLookSpring(moving, 1, 0)).toEqual(moving)
  })

  it('拒绝无效状态和负步长', () => {
    expect(() => stepLookSpring({ value: Number.NaN, velocity: 0 }, 1, 0.016)).toThrow(
      '视线弹簧状态无效',
    )
    expect(() => stepLookSpring(rest, Number.POSITIVE_INFINITY, 0.016)).toThrow('视线弹簧状态无效')
    expect(() => stepLookSpring(rest, 1, -0.016)).toThrow('视线弹簧步长无效')
    expect(() => isLookSpringSettled({ value: 0, velocity: Number.NaN }, 0)).toThrow(
      '视线弹簧状态无效',
    )
  })
})
