import { afterEach, describe, expect, it, vi } from 'vitest'
import { initialBlinkDelayMs } from './blink'
import {
  advanceCatMotion,
  clearCatMotion,
  getCatFrameStatus,
  preloadCatFrames,
  readCatEye,
  readCatWorkingTwitch,
  retainCatMotion,
  syncWorkingTwitch,
} from './catMotion'
import { listCatFrameSources } from './frames'

describe('猫咪动作调度', () => {
  afterEach(() => {
    clearCatMotion()
    vi.restoreAllMocks()
  })

  it('共享时钟推进眨眼，两只猫不会同时开始', () => {
    retainCatMotion('agent:assistant', 0)
    retainCatMotion('agent:developer', 0)
    const assistantAt = initialBlinkDelayMs('agent:assistant')
    const developerAt = initialBlinkDelayMs('agent:developer')
    expect(assistantAt).not.toBe(developerAt)
    const earlier = Math.min(assistantAt, developerAt)
    advanceCatMotion(earlier)
    const eyes = [readCatEye('agent:assistant'), readCatEye('agent:developer')]
    expect(eyes.filter((eye) => eye === 'half')).toHaveLength(1)
    expect(eyes.filter((eye) => eye === 'open')).toHaveLength(1)
  })

  it('页面隐藏时不补眨，恢复后重新睁眼排期', () => {
    retainCatMotion('agent:assistant', 0)
    const at = initialBlinkDelayMs('agent:assistant')
    advanceCatMotion(at, true)
    expect(readCatEye('agent:assistant')).toBe('open')
    advanceCatMotion(at + 20)
    expect(readCatEye('agent:assistant')).toBe('open')
    advanceCatMotion(at + 21)
    expect(readCatEye('agent:assistant')).toBe('open')
  })

  it('预加载失败会留下错误并可以重试', async () => {
    const error = vi.spyOn(console, 'error').mockImplementation(() => {})
    await expect(preloadCatFrames(async () => Promise.reject(new Error('坏帧')))).rejects.toThrow(
      '坏帧',
    )
    expect(getCatFrameStatus()).toBe('failed')
    expect(error).toHaveBeenCalledWith(expect.stringContaining('坏帧'))
    let loads = 0
    await preloadCatFrames(async () => {
      loads += 1
    })
    expect(getCatFrameStatus()).toBe('ready')
    expect(loads).toBe(listCatFrameSources().length)
    await preloadCatFrames(async () => {
      loads += 1
    })
    expect(loads).toBe(listCatFrameSources().length)
  })

  it('工作中的抽动由同一时钟驱动', () => {
    vi.spyOn(Math, 'random').mockReturnValue(0)
    syncWorkingTwitch('agent:assistant', true, 0)
    expect(readCatWorkingTwitch('agent:assistant')).toBeNull()
    advanceCatMotion(4_500)
    expect(readCatWorkingTwitch('agent:assistant')).toBe('left')
    advanceCatMotion(4_640)
    expect(readCatWorkingTwitch('agent:assistant')).toBeNull()
  })

  it('拒绝无效时钟', () => {
    expect(() => advanceCatMotion(Number.NaN)).toThrow('猫咪动作时钟无效')
  })
})
