import { act, renderHook, waitFor } from '@testing-library/react'
import { afterEach, expect, it, vi } from 'vitest'
import type { UserProfile } from '../desktop/backend'
import { memoryBackend } from '../desktop/memoryBackend'
import { useUserProfile } from './useUserProfile'

afterEach(() => vi.restoreAllMocks())

it('关闭资料错误后同一故障及迟到回包不重复弹出，恢复后新故障仍提示', async () => {
  const load = vi.spyOn(memoryBackend, 'loadUserProfile').mockRejectedValue(new Error('接口不存在'))
  const { result } = renderHook(useUserProfile)
  await waitFor(() => expect(result.current.error).toContain('接口不存在'))
  let fail!: (error: Error) => void
  load.mockImplementationOnce(
    () =>
      new Promise((_resolve, reject) => {
        fail = reject
      }),
  )
  act(() => window.dispatchEvent(new Event('focus')))
  act(() => result.current.dismissError())
  expect(result.current.error).toBe('')
  await act(async () => fail(new Error('接口不存在')))
  expect(result.current.error).toBe('')
  act(() => window.dispatchEvent(new Event('focus')))
  await waitFor(() => expect(load).toHaveBeenCalledTimes(3))
  expect(result.current.error).toBe('')
  load.mockResolvedValueOnce(memoryBackend.userProfile)
  act(() => window.dispatchEvent(new Event('online')))
  await waitFor(() => expect(result.current.profile).toBeDefined())
  act(() => window.dispatchEvent(new Event('focus')))
  await waitFor(() => expect(result.current.error).toContain('接口不存在'))
})

it('启动读取本人资料，切回客户端更新昵称头像，失败保留资料并提示，联网后恢复', async () => {
  const load = vi.spyOn(memoryBackend, 'loadUserProfile')
  const { result } = renderHook(useUserProfile)
  await waitFor(() => expect(result.current.profile?.displayName).toBe('测试用户'))
  memoryBackend.userProfile = {
    username: 'owner',
    displayName: '新昵称',
    avatar: 'data:image/png;base64,cG5n',
  }
  act(() => window.dispatchEvent(new Event('focus')))
  await waitFor(() => expect(result.current.profile).toEqual(memoryBackend.userProfile))
  load.mockRejectedValueOnce(new Error('网络中断'))
  act(() => window.dispatchEvent(new Event('focus')))
  await waitFor(() => expect(result.current.error).toContain('网络中断'))
  expect(result.current.profile?.displayName).toBe('新昵称')
  act(() => window.dispatchEvent(new Event('online')))
  await waitFor(() => expect(result.current.error).toBe(''))
})

it('慢请求期间只排队一次刷新，卸载后不再读取或写入资料', async () => {
  let finish!: (profile: UserProfile) => void
  const load = vi.spyOn(memoryBackend, 'loadUserProfile').mockImplementationOnce(
    () =>
      new Promise((resolve) => {
        finish = resolve
      }),
  )
  const { result, unmount } = renderHook(useUserProfile)
  act(() => {
    window.dispatchEvent(new Event('focus'))
    window.dispatchEvent(new Event('online'))
  })
  expect(load).toHaveBeenCalledTimes(1)
  await act(async () => finish({ username: 'owner', displayName: '旧昵称', avatar: '' }))
  await waitFor(() => expect(result.current.profile?.displayName).toBe('测试用户'))
  expect(load).toHaveBeenCalledTimes(2)
  unmount()
  act(() => window.dispatchEvent(new Event('focus')))
  expect(load).toHaveBeenCalledTimes(2)
})

it('首次读取失败明确显示错误，不展示虚构的用户资料', async () => {
  vi.spyOn(memoryBackend, 'loadUserProfile').mockRejectedValueOnce(new Error('资料不可用'))
  const { result, unmount } = renderHook(useUserProfile)
  await waitFor(() => expect(result.current.error).toContain('资料不可用'))
  expect(result.current.profile).toBeUndefined()
  unmount()
})

it('卸载后忽略迟到响应并取消排队刷新', async () => {
  let finish!: (profile: UserProfile) => void
  const load = vi.spyOn(memoryBackend, 'loadUserProfile').mockImplementationOnce(
    () =>
      new Promise((resolve) => {
        finish = resolve
      }),
  )
  const { unmount } = renderHook(useUserProfile)
  act(() => window.dispatchEvent(new Event('focus')))
  unmount()
  await act(async () => finish(memoryBackend.userProfile))
  expect(load).toHaveBeenCalledTimes(1)
})

it('窗口隐藏时不刷新，恢复可见时同步网页更新的资料', async () => {
  const load = vi.spyOn(memoryBackend, 'loadUserProfile')
  const { result, unmount } = renderHook(useUserProfile)
  await waitFor(() => expect(result.current.profile).toBeDefined())
  const visibility = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden')
  act(() => document.dispatchEvent(new Event('visibilitychange')))
  expect(load).toHaveBeenCalledTimes(1)
  memoryBackend.userProfile.displayName = '网页更新的昵称'
  visibility.mockReturnValue('visible')
  act(() => document.dispatchEvent(new Event('visibilitychange')))
  await waitFor(() => expect(result.current.profile?.displayName).toBe('网页更新的昵称'))
  unmount()
  act(() => document.dispatchEvent(new Event('visibilitychange')))
  expect(load).toHaveBeenCalledTimes(2)
})
