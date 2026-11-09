import { useRef, type PointerEvent } from 'react'

export const SIDEBAR_WIDTH = { default: 300, min: 240, max: 400, collapsed: 88, step: 16 } as const

/** 低于展开阈值直接收缩为头像栏，展开尺寸始终限制在允许范围内。 */
export function resolveSidebarWidth(width: number): number {
  return width < SIDEBAR_WIDTH.min ? SIDEBAR_WIDTH.collapsed : Math.min(width, SIDEBAR_WIDTH.max)
}

/** 使用指针捕获让越界拖动保持连续；取消手势恢复原宽度，同时支持键盘调整。 */
export function SidebarResizer({
  width,
  onChange,
  onResizing,
}: {
  width: number
  onChange: (width: number) => void
  onResizing: (isResizing: boolean) => void
}) {
  const gesture = useRef<{ pointerId: number; width: number; startX: number } | null>(null)
  function updateWidth(event: PointerEvent<HTMLDivElement>): void {
    const start = gesture.current
    if (start?.pointerId === event.pointerId) {
      onChange(resolveSidebarWidth(start.width + event.clientX - start.startX))
    }
  }
  function finishGesture(event: PointerEvent<HTMLDivElement>, isCancelled: boolean): void {
    if (gesture.current?.pointerId !== event.pointerId) return
    if (isCancelled) onChange(gesture.current.width)
    else updateWidth(event)
    gesture.current = null
    onResizing(false)
    event.currentTarget.releasePointerCapture(event.pointerId)
  }
  return (
    <div
      className="sidebar-resizer"
      role="separator"
      tabIndex={0}
      aria-label="调整侧栏宽度"
      aria-orientation="vertical"
      aria-valuemin={SIDEBAR_WIDTH.collapsed}
      aria-valuemax={SIDEBAR_WIDTH.max}
      aria-valuenow={width}
      aria-valuetext={width === SIDEBAR_WIDTH.collapsed ? '已收缩为头像栏' : `${width} 像素`}
      onPointerDown={(event) => {
        if (event.button !== 0 || gesture.current) return
        event.preventDefault()
        gesture.current = { pointerId: event.pointerId, width, startX: event.clientX }
        event.currentTarget.setPointerCapture(event.pointerId)
        onResizing(true)
      }}
      onPointerMove={updateWidth}
      onPointerUp={(event) => finishGesture(event, false)}
      onPointerCancel={(event) => finishGesture(event, true)}
      onLostPointerCapture={() => {
        gesture.current = null
        onResizing(false)
      }}
      onKeyDown={(event) => {
        let nextWidth: number
        switch (event.key) {
          case 'ArrowLeft':
            nextWidth = width - SIDEBAR_WIDTH.step
            break
          case 'ArrowRight':
            nextWidth = Math.max(SIDEBAR_WIDTH.min, width + SIDEBAR_WIDTH.step)
            break
          case 'Home':
            nextWidth = SIDEBAR_WIDTH.collapsed
            break
          case 'End':
            nextWidth = SIDEBAR_WIDTH.max
            break
          default:
            return
        }
        event.preventDefault()
        onChange(resolveSidebarWidth(nextWidth))
      }}
    />
  )
}
