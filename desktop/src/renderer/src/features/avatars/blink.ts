export type EyeFrame = 'open' | 'half' | 'closed'

export type BlinkPhase = 'waiting' | 'half-in' | 'closed' | 'half-out' | 'pause'

export type BlinkSchedule = {
  phase: BlinkPhase
  phaseEndsAt: number
  doubleBlinkPending: boolean
}

export const BLINK_HALF_IN_MS = 50
export const BLINK_CLOSED_MS = 60
export const BLINK_HALF_OUT_MS = 50
export const BLINK_GAP_MIN_MS = 3_000
export const BLINK_GAP_MAX_MS = 7_000
export const DOUBLE_BLINK_CHANCE = 0.25
export const DOUBLE_BLINK_PAUSE_MS = 130

/** 睁眼 → 半闭 → 闭合 → 半闭 → 睁眼，整段落在 150 到 200 毫秒之间。 */
export const BLINK_SEQUENCE_MS = BLINK_HALF_IN_MS + BLINK_CLOSED_MS + BLINK_HALF_OUT_MS

export function eyeForPhase(phase: BlinkPhase): EyeFrame {
  if (phase === 'half-in' || phase === 'half-out') return 'half'
  if (phase === 'closed') return 'closed'
  return 'open'
}

export function blinkDelayMs(unit: number): number {
  assertUnit(unit)
  return BLINK_GAP_MIN_MS + Math.floor(unit * (BLINK_GAP_MAX_MS - BLINK_GAP_MIN_MS))
}

/** 用标识打散第一下眨眼，避免一列猫同时闭眼。间隔仍落在 3 到 7 秒。 */
export function initialBlinkDelayMs(id: string): number {
  if (id.trim().length === 0) throw new Error('猫咪动作标识无效')
  return blinkDelayMs(stableUnit(id))
}

export function createBlinkSchedule(now: number, delayMs: number): BlinkSchedule {
  assertTime(now)
  if (!Number.isFinite(delayMs) || delayMs < 0) throw new Error('眨眼间隔无效')
  return { phase: 'waiting', phaseEndsAt: now + delayMs, doubleBlinkPending: false }
}

/** 时钟没到就返回原对象。一次最多走完一轮，避免时间跳变时连眨很多下。 */
export function advanceBlinkSchedule(
  schedule: BlinkSchedule,
  now: number,
  randomUnit: () => number,
): BlinkSchedule {
  assertTime(now)
  let current = schedule
  for (let step = 0; step < 8 && now >= current.phaseEndsAt; step += 1) {
    current = stepBlink(current, randomUnit)
  }
  return current
}

function stepBlink(schedule: BlinkSchedule, randomUnit: () => number): BlinkSchedule {
  switch (schedule.phase) {
    case 'waiting':
      return {
        phase: 'half-in',
        phaseEndsAt: schedule.phaseEndsAt + BLINK_HALF_IN_MS,
        doubleBlinkPending: randomUnit() < DOUBLE_BLINK_CHANCE,
      }
    case 'half-in':
      return {
        ...schedule,
        phase: 'closed',
        phaseEndsAt: schedule.phaseEndsAt + BLINK_CLOSED_MS,
      }
    case 'closed':
      return {
        ...schedule,
        phase: 'half-out',
        phaseEndsAt: schedule.phaseEndsAt + BLINK_HALF_OUT_MS,
      }
    case 'half-out':
      if (schedule.doubleBlinkPending) {
        return {
          phase: 'pause',
          phaseEndsAt: schedule.phaseEndsAt + DOUBLE_BLINK_PAUSE_MS,
          doubleBlinkPending: false,
        }
      }
      return {
        phase: 'waiting',
        phaseEndsAt: schedule.phaseEndsAt + blinkDelayMs(randomUnit()),
        doubleBlinkPending: false,
      }
    case 'pause':
      return {
        phase: 'half-in',
        phaseEndsAt: schedule.phaseEndsAt + BLINK_HALF_IN_MS,
        doubleBlinkPending: false,
      }
  }
}

function stableUnit(id: string): number {
  let hash = 2166136261
  for (let index = 0; index < id.length; index += 1) {
    hash ^= id.charCodeAt(index)
    hash = Math.imul(hash, 16777619)
  }
  return (hash >>> 0) / 4294967296
}

function assertUnit(unit: number): void {
  if (!Number.isFinite(unit) || unit < 0 || unit >= 1) throw new Error('随机数无效')
}

function assertTime(now: number): void {
  if (!Number.isFinite(now)) throw new Error('猫咪动作时钟无效')
}
