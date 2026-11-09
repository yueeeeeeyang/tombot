import { describe, expect, it } from 'vitest'
import {
  REST_AVATAR_POINTER,
  resolveAvatarPose,
  resolveAvatarPresentation,
  type AvatarPointer,
} from './pose'

const hovering: AvatarPointer = { isHovering: true, isPressed: false, lookX: 0.25, lookY: -0.5 }

describe('猫咪姿态', () => {
  it('没有活动和指针时保持静息', () => {
    expect(resolveAvatarPose('idle', REST_AVATAR_POINTER)).toBe('idle')
    expect(
      resolveAvatarPresentation({
        activity: 'idle',
        pointer: REST_AVATAR_POINTER,
        prefersReducedMotion: false,
      }),
    ).toEqual({ pose: 'idle', lookX: 0, lookY: 0, isPressed: false })
  })

  it('悬停或按下进入跟随，并夹住越界视线', () => {
    expect(resolveAvatarPose('idle', { isHovering: false, isPressed: true })).toBe('hover')
    expect(
      resolveAvatarPresentation({
        activity: 'idle',
        pointer: { ...hovering, lookX: 1.8, lookY: -2 },
        prefersReducedMotion: false,
      }),
    ).toEqual({ pose: 'hover', lookX: 1, lookY: -1, isPressed: false })
  })

  it('工作中优先于指针，按下也不会改成蹲伏', () => {
    const pointer: AvatarPointer = { isHovering: true, isPressed: true, lookX: 0.4, lookY: 0.2 }
    expect(resolveAvatarPose('working', pointer)).toBe('working')
    expect(
      resolveAvatarPresentation({ activity: 'working', pointer, prefersReducedMotion: false }),
    ).toEqual({ pose: 'working', lookX: 0, lookY: 0, isPressed: false })
  })

  it('减弱动效时无论工作和悬停都退回静止静息', () => {
    expect(
      resolveAvatarPresentation({
        activity: 'working',
        pointer: { isHovering: true, isPressed: true, lookX: 1, lookY: 1 },
        prefersReducedMotion: true,
      }),
    ).toEqual({ pose: 'idle', lookX: 0, lookY: 0, isPressed: false })
  })

  it('拒绝无效活动、指针、视线和减弱动效标记', () => {
    expect(() => resolveAvatarPose('busy' as 'idle', REST_AVATAR_POINTER)).toThrow(
      '助手活动只能是空闲或工作中',
    )
    expect(() =>
      resolveAvatarPose('idle', { isHovering: 'yes' } as unknown as AvatarPointer),
    ).toThrow('指针状态无效')
    expect(() =>
      resolveAvatarPresentation({
        activity: 'idle',
        pointer: { ...REST_AVATAR_POINTER, lookX: Number.NaN },
        prefersReducedMotion: false,
      }),
    ).toThrow('视线偏移无效')
    expect(() =>
      resolveAvatarPresentation({
        activity: 'working',
        pointer: REST_AVATAR_POINTER,
        prefersReducedMotion: 'reduce' as unknown as boolean,
      }),
    ).toThrow('减弱动效状态无效')
  })
})
