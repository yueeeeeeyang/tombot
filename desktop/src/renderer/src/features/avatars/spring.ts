/** 略欠阻尼，让头部跟着指针时有一点回弹，但不会来回晃个不停。 */
export const LOOK_SPRING = {
  stiffness: 220,
  damping: 20,
  maxStepSeconds: 0.032,
} as const

export type SpringState = {
  value: number
  velocity: number
}

const SETTLE_DISTANCE = 0.001
const SETTLE_VELOCITY = 0.01

/**
 * 朝目标走一小步。步长有上限，避免切回前台时一帧跳到位。
 * 零步长保持原状态；无效数字直接失败，不能把 NaN 写进变换。
 */
export function stepLookSpring(
  state: SpringState,
  target: number,
  deltaSeconds: number,
): SpringState {
  if (
    !Number.isFinite(state?.value) ||
    !Number.isFinite(state?.velocity) ||
    !Number.isFinite(target)
  ) {
    throw new Error('视线弹簧状态无效')
  }
  if (!Number.isFinite(deltaSeconds) || deltaSeconds < 0) {
    throw new Error('视线弹簧步长无效')
  }
  const dt = Math.min(deltaSeconds, LOOK_SPRING.maxStepSeconds)
  if (dt === 0) return { value: state.value, velocity: state.velocity }
  const acceleration =
    (target - state.value) * LOOK_SPRING.stiffness - state.velocity * LOOK_SPRING.damping
  const velocity = state.velocity + acceleration * dt
  return { value: state.value + velocity * dt, velocity }
}

export function isLookSpringSettled(state: SpringState, target: number): boolean {
  if (
    !Number.isFinite(state?.value) ||
    !Number.isFinite(state?.velocity) ||
    !Number.isFinite(target)
  ) {
    throw new Error('视线弹簧状态无效')
  }
  return (
    Math.abs(state.value - target) < SETTLE_DISTANCE && Math.abs(state.velocity) < SETTLE_VELOCITY
  )
}
