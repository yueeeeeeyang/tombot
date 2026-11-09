import { useEffect, useSyncExternalStore } from 'react'
import {
  advanceBlinkSchedule,
  createBlinkSchedule,
  eyeForPhase,
  initialBlinkDelayMs,
  type BlinkSchedule,
  type EyeFrame,
} from './blink'
import {
  createWorkingTwitch,
  advanceWorkingTwitch,
  readWorkingTwitch,
  type WorkingTwitch,
} from './twitch'
import { listCatFrameSources } from './frames'

export type CatFrameStatus = 'loading' | 'ready' | 'failed'

const blinks = new Map<string, BlinkSchedule>()
const eyes = new Map<string, EyeFrame>()
const twitches = new Map<string, WorkingTwitch>()
const twitchSides = new Map<string, 'left' | 'right' | null>()
const retains = new Map<string, number>()
const listeners = new Set<() => void>()
const frameListeners = new Set<() => void>()
let frameStatus: CatFrameStatus = 'loading'
let preloadTask: Promise<void> | null = null
let loopHandle = 0
let hiddenLatch = false
let retainCount = 0
let lastTick = 0

export function catMotionId(agentId: string | undefined, preset: string): string {
  const id = agentId && agentId.trim().length > 0 ? `agent:${agentId}` : `preset:${preset}`
  if (id.endsWith(':') || id.length < 8) throw new Error('猫咪动作标识无效')
  return id
}

export function readCatEye(id: string): EyeFrame {
  return eyes.get(id) ?? 'open'
}

export function readCatWorkingTwitch(id: string): 'left' | 'right' | null {
  return twitchSides.get(id) ?? null
}

export function retainCatMotion(id: string, now = performance.now()): void {
  const count = retains.get(id) ?? 0
  retains.set(id, count + 1)
  if (count === 0) {
    blinks.set(id, createBlinkSchedule(now, initialBlinkDelayMs(id)))
    eyes.set(id, 'open')
  }
  retainCount += 1
  ensureLoop()
}

export function releaseCatMotion(id: string): void {
  const count = retains.get(id) ?? 0
  if (count > 1) retains.set(id, count - 1)
  else {
    retains.delete(id)
    blinks.delete(id)
    eyes.delete(id)
    twitches.delete(id)
  }
  retainCount = Math.max(0, retainCount - 1)
  if (retainCount === 0) stopLoop()
}

export function syncWorkingTwitch(id: string, active: boolean, now = performance.now()): void {
  if (!active) {
    twitches.delete(id)
    if (twitchSides.delete(id)) notifyMotion()
    return
  }
  if (!twitches.has(id)) {
    twitches.set(id, createWorkingTwitch(now, Math.random))
    twitchSides.set(id, null)
  }
}

/** 推进全部猫咪的眨眼和抽耳。页面隐藏时只记下一次，恢复后重新排期，不补眨。 */
export function advanceCatMotion(now: number, hidden = false): void {
  if (!Number.isFinite(now)) throw new Error('猫咪动作时钟无效')
  if (hidden) {
    hiddenLatch = true
    return
  }
  if (hiddenLatch) {
    rebaseCatMotion(now)
    hiddenLatch = false
    notifyMotion()
    return
  }
  const previous = lastTick
  lastTick = now
  let changed = false
  for (const [id, schedule] of blinks) {
    const next = advanceBlinkSchedule(schedule, now, Math.random)
    if (next === schedule) continue
    blinks.set(id, next)
    const eye = eyeForPhase(next.phase)
    if (eyes.get(id) !== eye) {
      eyes.set(id, eye)
      changed = true
    }
  }
  for (const [id, twitch] of twitches) {
    const before = readWorkingTwitch(twitch, previous)
    const next = advanceWorkingTwitch(twitch, now, Math.random)
    const after = readWorkingTwitch(next, now)
    if (next !== twitch) twitches.set(id, next)
    if (before !== after) {
      twitchSides.set(id, after)
      changed = true
    }
  }
  if (changed) notifyMotion()
}

export function subscribeCatMotion(listener: () => void): () => void {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

export function getCatFrameStatus(): CatFrameStatus {
  return frameStatus
}

export function subscribeCatFrameStatus(listener: () => void): () => void {
  frameListeners.add(listener)
  return () => frameListeners.delete(listener)
}

export function preloadCatFrames(
  load: (src: string) => Promise<void> = decodeCatFrame,
): Promise<void> {
  if (frameStatus === 'ready') return Promise.resolve()
  if (preloadTask && frameStatus === 'loading') return preloadTask
  frameStatus = 'loading'
  notifyFrameStatus()
  const sources = listCatFrameSources()
  preloadTask = Promise.all(sources.map((src) => load(src)))
    .then(() => {
      frameStatus = 'ready'
      notifyFrameStatus()
    })
    .catch((error: unknown) => {
      frameStatus = 'failed'
      preloadTask = null
      console.error(`猫咪帧预加载失败，原因=${error instanceof Error ? error.message : '未知错误'}`)
      notifyFrameStatus()
      throw error
    })
  return preloadTask
}

export function decodeCatFrame(src: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const image = new Image()
    image.onload = () => {
      const decoding = image.decode?.()
      if (decoding) decoding.then(resolve).catch(reject)
      else resolve()
    }
    image.onerror = () => reject(new Error(`猫咪帧无法解码 ${src}`))
    image.src = src
  })
}

export function useCatEye(id: string): EyeFrame {
  useEffect(() => {
    retainCatMotion(id)
    return () => releaseCatMotion(id)
  }, [id])
  return useSyncExternalStore(
    subscribeCatMotion,
    () => readCatEye(id),
    () => 'open',
  )
}

export function useCatFrameStatus(): CatFrameStatus {
  return useSyncExternalStore(subscribeCatFrameStatus, getCatFrameStatus, () => 'loading')
}

export function useWorkingTwitch(id: string, active: boolean): 'left' | 'right' | null {
  useEffect(() => {
    if (!active) {
      syncWorkingTwitch(id, false)
      return
    }
    syncWorkingTwitch(id, true)
    return () => syncWorkingTwitch(id, false)
  }, [id, active])
  return useSyncExternalStore(
    subscribeCatMotion,
    () => readCatWorkingTwitch(id),
    () => null,
  )
}

/** 清掉动作表和预加载状态，避免单例串到下一次使用。 */
export function clearCatMotion(): void {
  blinks.clear()
  eyes.clear()
  twitches.clear()
  twitchSides.clear()
  retains.clear()
  hiddenLatch = false
  retainCount = 0
  lastTick = 0
  stopLoop()
  frameStatus = 'loading'
  preloadTask = null
  notifyMotion()
  notifyFrameStatus()
}

function rebaseCatMotion(now: number): void {
  for (const [id, schedule] of blinks) {
    const remaining = schedule.phase === 'waiting' ? schedule.phaseEndsAt - now : 0
    blinks.set(id, createBlinkSchedule(now, remaining > 0 ? remaining : initialBlinkDelayMs(id)))
    eyes.set(id, 'open')
  }
  for (const [id] of twitches) {
    twitches.set(id, createWorkingTwitch(now, Math.random))
    twitchSides.set(id, null)
  }
}

function ensureLoop(): void {
  if (loopHandle || typeof requestAnimationFrame !== 'function') return
  const step = (now: number) => {
    advanceCatMotion(now, document.hidden)
    loopHandle = requestAnimationFrame(step)
  }
  loopHandle = requestAnimationFrame(step)
  document.addEventListener('visibilitychange', onVisibility)
}

function stopLoop(): void {
  if (loopHandle) cancelAnimationFrame(loopHandle)
  loopHandle = 0
  document.removeEventListener('visibilitychange', onVisibility)
}

function onVisibility(): void {
  if (document.hidden) {
    hiddenLatch = true
    if (loopHandle) cancelAnimationFrame(loopHandle)
    loopHandle = 0
    return
  }
  if (retainCount > 0) ensureLoop()
}

function notifyMotion(): void {
  for (const listener of listeners) listener()
}

function notifyFrameStatus(): void {
  for (const listener of frameListeners) listener()
}
