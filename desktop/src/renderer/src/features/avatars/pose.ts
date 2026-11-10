export const AVATAR_POSES = ['idle', 'working', 'hover'] as const

export type AvatarPose = (typeof AVATAR_POSES)[number]

/** 传输层写入的活动。工作中表示请求尚未结束，不是本地草稿。 */
export type AgentActivity = 'idle' | 'working'

export type AvatarPointer = {
  isHovering: boolean
  isPressed: boolean
  lookX: number
  lookY: number
}

export type AvatarPresentation = {
  pose: AvatarPose
  lookX: number
  lookY: number
  isPressed: boolean
}

/** 没有指针时的输入。渲染层只消费解析结果，不自行猜测姿态。 */
export const REST_AVATAR_POINTER: AvatarPointer = {
  isHovering: false,
  isPressed: false,
  lookX: 0,
  lookY: 0,
}

/**
 * 解析姿态。工作中优先于悬停：传输层表示助手正在处理请求，
 * 指针离开后才会回到跟随；二者都没有时才是静息。
 */
export function resolveAvatarPose(
  activity: AgentActivity,
  pointer: Pick<AvatarPointer, 'isHovering' | 'isPressed'>,
): AvatarPose {
  if (activity !== 'idle' && activity !== 'working') {
    throw new Error('助手活动只能是空闲或工作中')
  }
  if (
    !pointer ||
    typeof pointer.isHovering !== 'boolean' ||
    typeof pointer.isPressed !== 'boolean'
  ) {
    throw new Error('指针状态无效')
  }
  if (activity === 'working') return 'working'
  if (pointer.isHovering || pointer.isPressed) return 'hover'
  return 'idle'
}

/**
 * 得到实际要画的姿态。减弱动效时固定为静止静息，
 * 避免工作动画或眼睛跟随变成无法关闭的运动。
 */
export function resolveAvatarPresentation(input: {
  activity: AgentActivity
  pointer: AvatarPointer
  prefersReducedMotion: boolean
}): AvatarPresentation {
  if (typeof input.prefersReducedMotion !== 'boolean') {
    throw new Error('减弱动效状态无效')
  }
  if (
    !input.pointer ||
    !Number.isFinite(input.pointer.lookX) ||
    !Number.isFinite(input.pointer.lookY)
  ) {
    throw new Error('视线偏移无效')
  }
  const pose = resolveAvatarPose(input.activity, input.pointer)
  if (input.prefersReducedMotion) {
    return { pose: 'idle', lookX: 0, lookY: 0, isPressed: false }
  }
  if (pose !== 'hover') {
    return { pose, lookX: 0, lookY: 0, isPressed: false }
  }
  return {
    pose,
    lookX: clampLook(input.pointer.lookX),
    lookY: clampLook(input.pointer.lookY),
    isPressed: input.pointer.isPressed,
  }
}

function clampLook(value: number): number {
  return Math.min(1, Math.max(-1, value))
}
