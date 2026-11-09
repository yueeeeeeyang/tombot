import { act, renderHook } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { THEME_STORAGE_KEY, useTheme } from './useTheme'

describe('主题偏好', () => {
  it('跟随系统实时更新，明确选择覆盖系统，卸载时释放监听', () => {
    const system = new EventTarget()
    const media = {
      matches: false,
      addEventListener: vi.fn(system.addEventListener.bind(system)),
      removeEventListener: vi.fn(system.removeEventListener.bind(system)),
    }
    vi.spyOn(window, 'matchMedia').mockReturnValue(media as unknown as MediaQueryList)
    const { result, unmount } = renderHook(useTheme)
    expect(document.documentElement.dataset.theme).toBe('light')
    act(() => {
      media.matches = true
      system.dispatchEvent(new Event('change'))
    })
    expect(document.documentElement.dataset.theme).toBe('dark')
    act(() => result.current.setTheme('light'))
    expect(document.documentElement.dataset.theme).toBe('light')
    act(() => system.dispatchEvent(new Event('change')))
    expect(document.documentElement.dataset.theme).toBe('light')
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe('light')
    act(() => result.current.setTheme('system'))
    expect(document.documentElement.dataset.theme).toBe('dark')
    unmount()
    expect(media.removeEventListener).toHaveBeenCalledTimes(
      media.addEventListener.mock.calls.length,
    )
  })

  it('重新打开恢复已选择的主题，不修改工作空间存储', () => {
    localStorage.setItem(THEME_STORAGE_KEY, 'dark')
    const { result } = renderHook(useTheme)
    expect(result.current.theme).toBe('dark')
    expect(document.documentElement.dataset.theme).toBe('dark')
    expect(localStorage.length).toBe(1)
  })

  it('无效偏好保留原值并提示，用户明确选择后恢复', () => {
    localStorage.setItem(THEME_STORAGE_KEY, '无效主题')
    const { result } = renderHook(useTheme)
    expect(result.current.error).toContain('主题偏好无效')
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe('无效主题')
    act(() => result.current.setTheme('light'))
    expect(result.current.error).toBe('')
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe('light')
  })

  it('读取和保存失败均显示原因，保存失败保留当前主题并可重试', () => {
    const read = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('读取不可用')
    })
    const { result } = renderHook(useTheme)
    expect(result.current.error).toContain('无法读取')
    read.mockRestore()
    const write = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('存储不可用')
    })
    act(() => result.current.setTheme('dark'))
    expect(result.current.error).toContain('未能保存')
    expect(document.documentElement.dataset.theme).toBe('dark')
    write.mockRestore()
    act(() => result.current.setTheme('dark'))
    expect(result.current.error).toBe('')
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe('dark')
  })
})
