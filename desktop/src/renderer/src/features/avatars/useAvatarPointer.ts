import { useEffect, useRef, useState, type RefObject } from 'react'
import { quantizeLook, resolveLookOffset } from './look'
import { REST_AVATAR_POINTER, type AvatarPointer } from './pose'

/**
 * 侧栏一行里可跟随指针的表面：列表按钮、收缩栏按钮，
 * 以及重命名时不再是按钮的置顶卡片和列表行。
 */
const POINTER_SURFACE = 'button, .agent-button, .pinned-agent'

/**
 * 只在当前这一只猫上记录指针。视线量化后再写入，避免拖着鼠标时整栏重绘。
 * 尺寸读失败时保持悬停，但视线回到正中，并只警告一次。
 */
export function useAvatarPointer(
  tracksPointer: boolean,
  agentId: string | undefined,
): { ref: RefObject<HTMLSpanElement | null>; pointer: AvatarPointer } {
  const ref = useRef<HTMLSpanElement>(null)
  const [pointer, setPointer] = useState<AvatarPointer>(REST_AVATAR_POINTER)

  useEffect(() => {
    if (!tracksPointer) return
    const avatar = ref.current
    const surface = avatar?.closest(POINTER_SURFACE)
    if (!avatar || !(surface instanceof HTMLElement) || surface === avatar) {
      console.warn(`侧栏头像没有可跟随的区域，助手=${agentId ?? '未绑定'}`)
      return
    }
    const avatarElement = avatar

    let hasWarned = false
    function publish(next: AvatarPointer): void {
      setPointer((current) =>
        current.isHovering === next.isHovering &&
        current.isPressed === next.isPressed &&
        current.lookX === next.lookX &&
        current.lookY === next.lookY
          ? current
          : next,
      )
    }
    function readLook(event: PointerEvent): { lookX: number; lookY: number } {
      try {
        const bounds = avatarElement.getBoundingClientRect()
        const look = resolveLookOffset(event.clientX, event.clientY, bounds)
        return { lookX: quantizeLook(look.x), lookY: quantizeLook(look.y) }
      } catch (error) {
        if (!hasWarned) {
          hasWarned = true
          const reason = error instanceof Error ? error.message : '未知错误'
          console.warn(`头像无法跟随指针，助手=${agentId ?? '未绑定'}，原因=${reason}`)
        }
        return { lookX: 0, lookY: 0 }
      }
    }
    function onEnter(event: PointerEvent): void {
      publish({ isHovering: true, isPressed: event.buttons === 1, ...readLook(event) })
    }
    function onMove(event: PointerEvent): void {
      publish({ isHovering: true, isPressed: event.buttons === 1, ...readLook(event) })
    }
    function onDown(event: PointerEvent): void {
      publish({ isHovering: true, isPressed: true, ...readLook(event) })
    }
    function onUp(event: PointerEvent): void {
      publish({ isHovering: true, isPressed: false, ...readLook(event) })
    }
    function onLeave(): void {
      publish(REST_AVATAR_POINTER)
    }

    surface.addEventListener('pointerenter', onEnter)
    surface.addEventListener('pointermove', onMove)
    surface.addEventListener('pointerdown', onDown)
    surface.addEventListener('pointerup', onUp)
    surface.addEventListener('pointerleave', onLeave)
    return () => {
      surface.removeEventListener('pointerenter', onEnter)
      surface.removeEventListener('pointermove', onMove)
      surface.removeEventListener('pointerdown', onDown)
      surface.removeEventListener('pointerup', onUp)
      surface.removeEventListener('pointerleave', onLeave)
    }
  }, [tracksPointer, agentId])

  return { ref, pointer: tracksPointer ? pointer : REST_AVATAR_POINTER }
}
