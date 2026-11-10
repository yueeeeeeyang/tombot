import { WORKING_TWITCH_MS, workingTwitchDelayMs, type EarSide } from './ears'

export type WorkingTwitch = {
  at: number
  side: EarSide
}

export function createWorkingTwitch(now: number, randomUnit: () => number): WorkingTwitch {
  if (!Number.isFinite(now)) throw new Error('猫咪动作时钟无效')
  return {
    at: now + workingTwitchDelayMs(randomUnit()),
    side: randomUnit() < 0.5 ? 'left' : 'right',
  }
}

/** 抽动窗口结束才排下一次，窗口内保持同一侧。 */
export function advanceWorkingTwitch(
  twitch: WorkingTwitch,
  now: number,
  randomUnit: () => number,
): WorkingTwitch {
  if (!Number.isFinite(now)) throw new Error('猫咪动作时钟无效')
  if (now < twitch.at + WORKING_TWITCH_MS) return twitch
  return createWorkingTwitch(now, randomUnit)
}

export function readWorkingTwitch(twitch: WorkingTwitch, now: number): EarSide | null {
  if (!Number.isFinite(now)) throw new Error('猫咪动作时钟无效')
  if (now < twitch.at || now >= twitch.at + WORKING_TWITCH_MS) return null
  return twitch.side
}
