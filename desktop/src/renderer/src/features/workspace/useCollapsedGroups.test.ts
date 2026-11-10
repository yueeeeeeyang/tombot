import { act, renderHook } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { useCollapsedGroups } from './useCollapsedGroups'

const key = 'tombot.collapsed-groups.v1'

describe('分组折叠状态', () => {
  it('各分组独立折叠，重新加载仍保留，可再次展开', () => {
    const error = vi.fn()
    const view = renderHook(() => useCollapsedGroups(error))
    act(() => view.result.current.toggle('cats:创作'))
    act(() => view.result.current.toggle('conversations'))
    expect(view.result.current.collapsed).toEqual(['cats:创作', 'conversations'])
    view.unmount()
    const restored = renderHook(() => useCollapsedGroups(error))
    expect(restored.result.current.collapsed).toEqual(['cats:创作', 'conversations'])
    act(() => restored.result.current.toggle('cats:创作'))
    expect(restored.result.current.collapsed).toEqual(['conversations'])
    expect(error).not.toHaveBeenCalled()
  })
  it('损坏内容保留原文并显式报错', () => {
    localStorage.setItem(key, '{')
    const error = vi.fn()
    const view = renderHook(() => useCollapsedGroups(error))
    expect(view.result.current.error).toContain('无法读取')
    act(() => view.result.current.toggle('conversations'))
    expect(error).toHaveBeenCalled()
    expect(localStorage.getItem(key)).toBe('{')
  })
  it('保存失败不改变折叠状态', () => {
    const error = vi.fn()
    const view = renderHook(() => useCollapsedGroups(error))
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('存储不可用')
    })
    act(() => view.result.current.toggle('conversations'))
    expect(error).toHaveBeenCalledWith(expect.stringContaining('无法保存'))
    expect(view.result.current.collapsed).toEqual([])
  })
})
