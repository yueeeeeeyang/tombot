import { act, renderHook } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { memoryBackend } from '../desktop/memoryBackend'
import { CONNECTION_CHECK_MS } from './sessionProbe'
import { useConnectionStatus } from './useConnectionStatus'

afterEach(() => {
  vi.useRealTimers()
  vi.restoreAllMocks()
})

describe('服务端连接状态', () => {
  it('连接正常、请求失败、鉴权失效和恢复均更新状态', async () => {
    vi.useFakeTimers()
    const { result, unmount } = renderHook(useConnectionStatus)
    await act(async () => {})
    expect(result.current).toBe('connected')
    memoryBackend.sessionError = new Error('无法连接')
    await act(() => vi.advanceTimersByTimeAsync(CONNECTION_CHECK_MS))
    expect(result.current).toBe('disconnected')
    memoryBackend.sessionError = null
    memoryBackend.phase = 'login'
    await act(() => vi.advanceTimersByTimeAsync(CONNECTION_CHECK_MS))
    expect(result.current).toBe('disconnected')
    memoryBackend.phase = 'ready'
    await act(() => vi.advanceTimersByTimeAsync(CONNECTION_CHECK_MS))
    expect(result.current).toBe('connected')
    unmount()
    expect(vi.getTimerCount()).toBe(0)
  })
  it('慢请求期间不叠加探测，卸载后不启动下一次探测', async () => {
    vi.useFakeTimers()
    let complete!: (value: { phase: 'ready' }) => void
    const session = vi.spyOn(memoryBackend, 'session').mockImplementation(
      () =>
        new Promise((resolve) => {
          complete = resolve
        }),
    )
    const { unmount } = renderHook(useConnectionStatus)
    act(() => window.dispatchEvent(new Event('focus')))
    await act(() => vi.advanceTimersByTimeAsync(CONNECTION_CHECK_MS * 3))
    expect(session).toHaveBeenCalledTimes(1)
    unmount()
    await act(async () => complete({ phase: 'ready' }))
    expect(vi.getTimerCount()).toBe(0)
  })

  it('探测期间网络恢复，合并多次唤醒并在旧请求结束后立即重新探测', async () => {
    vi.useFakeTimers()
    let complete!: (value: { phase: 'offline' }) => void
    const session = vi
      .spyOn(memoryBackend, 'session')
      .mockImplementationOnce(
        () =>
          new Promise((resolve) => {
            complete = resolve
          }),
      )
      .mockResolvedValue({ phase: 'ready' })
    const { result } = renderHook(useConnectionStatus)
    act(() => {
      window.dispatchEvent(new Event('offline'))
      window.dispatchEvent(new Event('online'))
      window.dispatchEvent(new Event('focus'))
    })
    expect(result.current).toBe('disconnected')
    expect(session).toHaveBeenCalledTimes(1)
    await act(async () => complete({ phase: 'offline' }))
    await act(() => vi.advanceTimersByTimeAsync(0))
    expect(session).toHaveBeenCalledTimes(2)
    expect(result.current).toBe('connected')
  })
})
