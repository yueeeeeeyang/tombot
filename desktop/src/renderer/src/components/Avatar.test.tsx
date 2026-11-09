import { act, fireEvent, render } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  AGENT_ACTIVITY_EVENT,
  installAgentActivityEvents,
  setAgentActivity,
} from '../features/avatars/activity'
import { CAT_AVATAR_IDS } from '../features/avatars/presets'
import { getCatPalette } from '../features/avatars/palette'
import { REDUCED_MOTION_QUERY } from '../features/avatars/usePrefersReducedMotion'
import { Avatar } from './Avatar'

function mockBounds(element: HTMLElement): void {
  vi.spyOn(element, 'getBoundingClientRect').mockReturnValue({
    x: 100,
    y: 200,
    left: 100,
    top: 200,
    right: 134,
    bottom: 234,
    width: 34,
    height: 34,
    toJSON() {
      return {}
    },
  })
}

describe('猫咪头像', () => {
  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('八款预设都画出可动的眼睛和耳朵，并标上自己的毛色', () => {
    for (const preset of CAT_AVATAR_IDS) {
      const view = render(<Avatar preset={preset} />)
      const avatar = view.container.querySelector('.avatar')
      expect(avatar).toHaveAttribute('data-avatar', preset)
      expect(avatar).toHaveAttribute('data-pose', 'idle')
      expect(avatar).toHaveAttribute('data-pressed', 'false')
      expect(view.container.querySelector('svg')).toHaveAttribute(
        'data-fur',
        getCatPalette(preset).fur,
      )
      expect(view.container.querySelectorAll('.cat-pupil')).toHaveLength(2)
      expect(view.container.querySelectorAll('.cat-lid')).toHaveLength(2)
      expect(view.container.querySelector('.cat-ear-left')).not.toBeNull()
      expect(view.container.querySelector('.cat-ear-right')).not.toBeNull()
      expect(view.container.querySelector('.cat-face')).not.toBeNull()
      view.unmount()
    }
  })

  it('侧栏一行里跟随指针，离开、越界和按下都有明确结果', () => {
    const view = render(
      <button>
        <Avatar preset="bombay" agentId="assistant" tracksPointer />
      </button>,
    )
    const button = view.container.querySelector('button')!
    const avatar = view.container.querySelector('.avatar') as HTMLElement
    mockBounds(avatar)

    fireEvent.pointerMove(button, { clientX: 100, clientY: 217 })
    expect(avatar).toHaveAttribute('data-pose', 'hover')
    expect(avatar.style.getPropertyValue('--look-x')).toBe('-1')
    expect(avatar.style.getPropertyValue('--look-y')).toBe('0')

    fireEvent.pointerMove(button, { clientX: 125.5, clientY: 183 })
    expect(avatar.style.getPropertyValue('--look-x')).toBe('0.5')
    expect(avatar.style.getPropertyValue('--look-y')).toBe('-1')

    fireEvent.pointerDown(button, { clientX: 134, clientY: 217 })
    expect(avatar).toHaveAttribute('data-pressed', 'true')
    expect(avatar.style.getPropertyValue('--look-x')).toBe('1')
    fireEvent.pointerUp(button, { clientX: 134, clientY: 217 })
    expect(avatar).toHaveAttribute('data-pressed', 'false')
    expect(avatar).toHaveAttribute('data-pose', 'hover')

    fireEvent.pointerLeave(button)
    expect(avatar).toHaveAttribute('data-pose', 'idle')
    expect(avatar.style.getPropertyValue('--look-x')).toBe('0')
  })

  it('头像还没有尺寸时不假装看向某个方向', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {})
    const view = render(
      <button>
        <Avatar preset="orange-tabby" agentId="developer" tracksPointer />
      </button>,
    )
    const button = view.container.querySelector('button')!
    fireEvent.pointerMove(button, { clientX: 10, clientY: 10 })
    fireEvent.pointerMove(button, { clientX: 12, clientY: 12 })
    const avatar = view.container.querySelector('.avatar')
    expect(avatar).toHaveAttribute('data-pose', 'hover')
    expect((avatar as HTMLElement).style.getPropertyValue('--look-x')).toBe('0')
    expect(warn).toHaveBeenCalledTimes(1)
    expect(warn).toHaveBeenCalledWith(expect.stringContaining('头像尺寸无效'))
    expect(warn).toHaveBeenCalledWith(expect.stringContaining('助手=developer'))
  })

  it('没有侧栏行时不能静默开始跟随', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {})
    render(<Avatar preset="persian" tracksPointer />)
    expect(warn).toHaveBeenCalledWith(expect.stringContaining('没有可跟随的区域'))
    expect(warn).toHaveBeenCalledWith(expect.stringContaining('助手=未绑定'))
  })

  it('未开启跟随时，指针划过父按钮不会改变姿态', () => {
    const view = render(
      <button>
        <Avatar preset="ragdoll" agentId="news" />
      </button>,
    )
    fireEvent.pointerMove(view.container.querySelector('button')!, { clientX: 4, clientY: 4 })
    expect(view.container.querySelector('.avatar')).toHaveAttribute('data-pose', 'idle')
  })

  it('工作中盖过悬停，活动事件能驱动已绑定的猫', () => {
    const release = installAgentActivityEvents()
    const view = render(
      <button>
        <Avatar preset="bombay" agentId="assistant" tracksPointer />
      </button>,
    )
    const button = view.container.querySelector('button')!
    const avatar = view.container.querySelector('.avatar') as HTMLElement
    mockBounds(avatar)
    fireEvent.pointerMove(button, { clientX: 100, clientY: 217 })
    expect(avatar).toHaveAttribute('data-pose', 'hover')

    act(() => {
      window.dispatchEvent(
        new CustomEvent(AGENT_ACTIVITY_EVENT, {
          detail: { agentId: 'assistant', activity: 'working' },
        }),
      )
    })
    expect(avatar).toHaveAttribute('data-pose', 'working')
    expect(avatar).toHaveAttribute('data-pressed', 'false')
    expect(avatar.style.getPropertyValue('--look-x')).toBe('0')

    act(() => setAgentActivity('assistant', 'idle'))
    expect(avatar).toHaveAttribute('data-pose', 'hover')
    expect(avatar.style.getPropertyValue('--look-x')).toBe('-1')
    release()
  })

  it('减弱动效时工作和悬停都画成静止静息，并在系统切换后立即生效', () => {
    let reduced = false
    const listeners = new Set<() => void>()
    vi.spyOn(window, 'matchMedia').mockImplementation((query: string) => {
      const isMotion = query === REDUCED_MOTION_QUERY
      return {
        get matches() {
          return isMotion ? reduced : false
        },
        media: query,
        onchange: null,
        addEventListener: (_event: string, listener: () => void) => {
          if (isMotion) listeners.add(listener)
        },
        removeEventListener: (_event: string, listener: () => void) => {
          if (isMotion) listeners.delete(listener)
        },
        addListener: vi.fn(),
        removeListener: vi.fn(),
        dispatchEvent: vi.fn(),
      } as unknown as MediaQueryList
    })

    const view = render(
      <button>
        <Avatar preset="siamese" agentId="audio" tracksPointer />
      </button>,
    )
    const avatar = view.container.querySelector('.avatar') as HTMLElement
    mockBounds(avatar)
    act(() => setAgentActivity('audio', 'working'))
    fireEvent.pointerMove(view.container.querySelector('button')!, { clientX: 100, clientY: 217 })
    expect(avatar).toHaveAttribute('data-pose', 'working')

    act(() => {
      reduced = true
      for (const listener of listeners) listener()
    })
    expect(avatar).toHaveAttribute('data-pose', 'idle')
    expect(avatar.style.getPropertyValue('--look-x')).toBe('0')
    expect(avatar).toHaveAttribute('data-pressed', 'false')

    view.unmount()
    expect(listeners.size).toBe(0)
  })
})
