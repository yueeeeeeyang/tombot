import { useEffect, useRef, type RefObject } from 'react'
import { isLookSpringSettled, stepLookSpring, type SpringState } from './spring'

const REST_SPRING: SpringState = { value: 0, velocity: 0 }

/**
 * 把目标视线平滑成实际倾斜。位移和速度留在 ref 里，
 * 指针变化只重开下一帧，不把已经转过的角度清掉。
 * 减弱动效时立刻归零并停帧，避免还在滑向指针。
 */
export function useLookTilt(
  ref: RefObject<HTMLElement | null>,
  targetX: number,
  targetY: number,
  prefersReducedMotion: boolean,
): void {
  const motion = useRef({ x: REST_SPRING, y: REST_SPRING })

  useEffect(() => {
    const node = ref.current
    if (!node) return
    if (prefersReducedMotion) {
      motion.current = { x: REST_SPRING, y: REST_SPRING }
      writeTilt(node, 0, 0)
      return
    }

    let frame = 0
    let last = performance.now()
    const tick = (now: number) => {
      // 个别环境会重复给出同一个时间戳。没有间隔时按一帧继续，避免弹簧停住。
      const elapsed = now - last
      const dt = elapsed > 0 ? elapsed / 1000 : 1 / 60
      last = elapsed > 0 ? now : last + 1000 / 60
      const nextX = stepLookSpring(motion.current.x, targetX, dt)
      const nextY = stepLookSpring(motion.current.y, targetY, dt)
      motion.current = { x: nextX, y: nextY }
      const settled = isLookSpringSettled(nextX, targetX) && isLookSpringSettled(nextY, targetY)
      writeTilt(node, settled ? targetX : nextX.value, settled ? targetY : nextY.value)
      if (!settled) frame = requestAnimationFrame(tick)
    }
    frame = requestAnimationFrame(tick)
    return () => cancelAnimationFrame(frame)
  }, [ref, targetX, targetY, prefersReducedMotion])
}

function writeTilt(node: HTMLElement, x: number, y: number): void {
  node.style.setProperty('--tilt-x', formatTilt(x))
  node.style.setProperty('--tilt-y', formatTilt(y))
}

function formatTilt(value: number): string {
  return value.toFixed(4)
}
