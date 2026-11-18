import { act, fireEvent, render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { ConsoleBackend } from '../../../../shared/console'
import { ComputerScreen } from './ComputerScreen'

const instances: FakeRFB[] = []
class FakeRFB extends EventTarget {
  viewOnly = true
  scaleViewport = false
  resizeSession = true
  showDotCursor = false
  background = ''
  sendKey = vi.fn()
  focus = vi.fn()
  disconnect = vi.fn()
  constructor() {
    super()
    instances.push(this)
  }
}
vi.mock('@novnc/novnc', () => ({
  default: class extends EventTarget {
    constructor() {
      super()
      return new FakeRFB()
    }
  },
}))
const api = {
  connectConsole: vi.fn(() => ({ send: vi.fn(), close: vi.fn() })),
} as unknown as ConsoleBackend
beforeEach(() => {
  instances.length = 0
  vi.clearAllMocks()
})
describe('noVNC 电脑画面', () => {
  it('同一 RFB 连接切换观看和接管，桌面分辨率不随窗口变化', () => {
    const props = {
      backend: api,
      consoleId: 'view',
      name: '小黑',
      onConnected: vi.fn(),
      onError: vi.fn(),
    }
    const view = render(<ComputerScreen {...props} isControlling={false} />)
    const rfb = instances[0]!
    expect(rfb.viewOnly).toBe(true)
    expect(rfb.resizeSession).toBe(false)
    expect(rfb.scaleViewport).toBe(true)
    expect(screen.queryByRole('textbox')).toBeNull()
    view.rerender(<ComputerScreen {...props} isControlling />)
    expect(instances).toHaveLength(1)
    expect(rfb.viewOnly).toBe(false)
    expect(props.onConnected).not.toHaveBeenCalled()
    act(() => rfb.dispatchEvent(new Event('connect')))
    expect(props.onConnected).toHaveBeenCalledOnce()
    fireEvent.change(screen.getByRole('textbox'), { target: { value: '猫a' } })
    fireEvent.click(screen.getByRole('button', { name: '输入' }))
    expect(rfb.sendKey.mock.calls).toEqual([[0x0100732b], [97]])
    view.rerender(<ComputerScreen {...props} isControlling={false} />)
    expect(rfb.viewOnly).toBe(true)
    view.unmount()
    expect(rfb.disconnect).toHaveBeenCalledOnce()
  })
  it('连接中断立即通知界面，输入不重放', () => {
    const onError = vi.fn()
    render(
      <ComputerScreen
        backend={api}
        consoleId="view"
        name="小黑"
        isControlling
        onConnected={vi.fn()}
        onError={onError}
      />,
    )
    act(() => instances[0]!.dispatchEvent(new Event('disconnect')))
    expect(onError).toHaveBeenCalledWith(expect.stringContaining('连接已结束'))
    expect(instances[0]!.sendKey).not.toHaveBeenCalled()
  })
})
