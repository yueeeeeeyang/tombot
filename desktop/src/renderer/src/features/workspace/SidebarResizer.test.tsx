import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { SidebarResizer, resolveSidebarWidth } from './SidebarResizer'

describe('侧栏拖拽', () => {
  it.each([
    [239, 88],
    [240, 240],
    [400, 400],
    [600, 400],
    [-50, 88],
  ])('宽度 %i 对应有效宽度 %i', (width, expected) =>
    expect(resolveSidebarWidth(width)).toBe(expected),
  )

  it('取消指针手势恢复起始宽度，忽略右键与其他指针', () => {
    const change = vi.fn()
    const resizing = vi.fn()
    render(<SidebarResizer width={300} onChange={change} onResizing={resizing} />)
    const separator = screen.getByRole('separator')
    separator.setPointerCapture = vi.fn()
    separator.releasePointerCapture = vi.fn()
    fireEvent.pointerDown(separator, { pointerId: 1, button: 2, clientX: 300 })
    expect(resizing).not.toHaveBeenCalled()
    fireEvent.pointerDown(separator, { pointerId: 1, button: 0, clientX: 300 })
    fireEvent.pointerMove(separator, { pointerId: 2, clientX: 350 })
    expect(change).not.toHaveBeenCalled()
    fireEvent.pointerMove(separator, { pointerId: 1, clientX: 380 })
    expect(change).toHaveBeenLastCalledWith(380)
    fireEvent.pointerCancel(separator, { pointerId: 1 })
    expect(change).toHaveBeenLastCalledWith(300)
    expect(resizing).toHaveBeenLastCalledWith(false)
    expect(separator.releasePointerCapture).toHaveBeenCalledWith(1)
    fireEvent.pointerMove(separator, { pointerId: 1, clientX: 390 })
    expect(change).toHaveBeenCalledTimes(2)
  })
})
