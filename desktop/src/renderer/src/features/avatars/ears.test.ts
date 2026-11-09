import { describe, expect, it } from 'vitest'
import {
  ENTER_SETTLE_MS,
  ENTER_TWITCH_MS,
  PERK_HALF_MS,
  WORKING_TWITCH_MS,
  earSideFromLook,
  resolveEarFrame,
  workingTwitchDelayMs,
} from './ears'
import { advanceWorkingTwitch, createWorkingTwitch, readWorkingTwitch } from './twitch'

const ready = { framesReady: true, prefersReducedMotion: false, workingTwitch: null }

describe('耳朵帧', () => {
  it('按指针来向选择耳朵，中线算右侧', () => {
    expect(earSideFromLook(-0.2)).toBe('left')
    expect(earSideFromLook(0)).toBe('right')
    expect(earSideFromLook(0.4)).toBe('right')
    expect(() => earSideFromLook(Number.NaN)).toThrow('视线偏移无效')
  })

  it('悬停时半竖耳、抽进场那一侧，再回到竖耳', () => {
    const entered = { at: 1_000, side: 'left' as const }
    expect(resolveEarFrame({ ...ready, pose: 'hover', now: 1_000, hoverEntry: entered })).toBe(
      'perk-half',
    )
    expect(
      resolveEarFrame({
        ...ready,
        pose: 'hover',
        now: 1_000 + PERK_HALF_MS,
        hoverEntry: entered,
      }),
    ).toBe('twitch-left')
    expect(
      resolveEarFrame({
        ...ready,
        pose: 'hover',
        now: 1_000 + PERK_HALF_MS + ENTER_TWITCH_MS,
        hoverEntry: { at: 1_000, side: 'right' },
      }),
    ).toBe('perk-half')
    expect(
      resolveEarFrame({
        ...ready,
        pose: 'hover',
        now: 1_000 + PERK_HALF_MS + ENTER_TWITCH_MS + ENTER_SETTLE_MS,
        hoverEntry: entered,
      }),
    ).toBe('perk')
  })

  it('工作中保持竖耳，并在时间窗内抽一下', () => {
    expect(
      resolveEarFrame({ ...ready, pose: 'working', now: 0, hoverEntry: null, workingTwitch: null }),
    ).toBe('perk')
    expect(
      resolveEarFrame({
        ...ready,
        pose: 'working',
        now: 0,
        hoverEntry: null,
        workingTwitch: 'right',
      }),
    ).toBe('twitch-right')
  })

  it('静息、帧未就绪和减弱动效都只用原来的耳朵', () => {
    expect(resolveEarFrame({ ...ready, pose: 'idle', now: 0, hoverEntry: null })).toBe('rest')
    expect(
      resolveEarFrame({
        ...ready,
        pose: 'hover',
        framesReady: false,
        now: 5_000,
        hoverEntry: { at: 0, side: 'left' },
      }),
    ).toBe('rest')
    expect(
      resolveEarFrame({
        ...ready,
        pose: 'working',
        prefersReducedMotion: true,
        now: 0,
        hoverEntry: null,
        workingTwitch: 'left',
      }),
    ).toBe('rest')
  })

  it('工作中的抽动只在窗口内出现，结束后才排下一次', () => {
    const twitch = createWorkingTwitch(0, () => 0)
    expect(twitch.side).toBe('left')
    expect(twitch.at).toBe(workingTwitchDelayMs(0))
    expect(readWorkingTwitch(twitch, twitch.at - 1)).toBeNull()
    expect(readWorkingTwitch(twitch, twitch.at)).toBe('left')
    expect(readWorkingTwitch(twitch, twitch.at + WORKING_TWITCH_MS - 1)).toBe('left')
    const next = advanceWorkingTwitch(twitch, twitch.at + WORKING_TWITCH_MS, () => 0.8)
    expect(next.at).toBeGreaterThan(twitch.at + WORKING_TWITCH_MS)
    expect(next.side).toBe('right')
    expect(() => workingTwitchDelayMs(-0.1)).toThrow('随机数无效')
    expect(() => readWorkingTwitch(twitch, Number.NaN)).toThrow('猫咪动作时钟无效')
  })
})
