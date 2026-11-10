import type { AvatarPose } from './pose'

export type EarFrame = 'rest' | 'perk-half' | 'perk' | 'twitch-left' | 'twitch-right'
export type EarSide = 'left' | 'right'

export const PERK_HALF_MS = 110
export const ENTER_TWITCH_MS = 140
export const ENTER_SETTLE_MS = 110
export const WORKING_TWITCH_MS = 140
export const WORKING_TWITCH_GAP_MIN_MS = 4_500
export const WORKING_TWITCH_GAP_MAX_MS = 8_000

export type HoverEntry = {
  at: number
  side: EarSide
}

/**
 * 指针在头像中线右侧时抽右耳，左侧抽左耳。
 * 正好在中线上算右侧，这样左右都有明确结果。
 */
export function earSideFromLook(lookX: number): EarSide {
  if (!Number.isFinite(lookX)) throw new Error('视线偏移无效')
  return lookX >= 0 ? 'right' : 'left'
}

export function workingTwitchDelayMs(unit: number): number {
  if (!Number.isFinite(unit) || unit < 0 || unit >= 1) throw new Error('随机数无效')
  return (
    WORKING_TWITCH_GAP_MIN_MS +
    Math.floor(unit * (WORKING_TWITCH_GAP_MAX_MS - WORKING_TWITCH_GAP_MIN_MS))
  )
}

/**
 * 悬停：半竖耳 → 进场那一侧抽一下 → 再半竖耳 → 保持竖耳。
 * 工作中保持竖耳，抽动由外部时间窗决定。静息耳朵不动。
 */
export function resolveEarFrame(input: {
  pose: AvatarPose
  framesReady: boolean
  prefersReducedMotion: boolean
  now: number
  hoverEntry: HoverEntry | null
  workingTwitch: EarSide | null
}): EarFrame {
  if (!Number.isFinite(input.now)) throw new Error('猫咪动作时钟无效')
  if (input.prefersReducedMotion || !input.framesReady) return 'rest'
  if (input.pose === 'working') {
    if (input.workingTwitch === 'left') return 'twitch-left'
    if (input.workingTwitch === 'right') return 'twitch-right'
    return 'perk'
  }
  if (input.pose !== 'hover' || !input.hoverEntry) return 'rest'
  const elapsed = input.now - input.hoverEntry.at
  if (elapsed < PERK_HALF_MS) return 'perk-half'
  if (elapsed < PERK_HALF_MS + ENTER_TWITCH_MS) {
    return input.hoverEntry.side === 'left' ? 'twitch-left' : 'twitch-right'
  }
  if (elapsed < PERK_HALF_MS + ENTER_TWITCH_MS + ENTER_SETTLE_MS) return 'perk-half'
  return 'perk'
}
