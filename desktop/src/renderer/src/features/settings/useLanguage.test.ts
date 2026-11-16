import { act, renderHook } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { LANGUAGE_STORAGE_KEY, useLanguage } from './useLanguage'

describe('本机语言偏好', () => {
  it('默认跟随系统，保存后重新挂载仍保留选择', () => {
    const first = renderHook(useLanguage)
    expect(first.result.current.language).toBe('system')
    act(() => first.result.current.setLanguage('en'))
    expect(localStorage.getItem(LANGUAGE_STORAGE_KEY)).toBe('en')
    first.unmount()
    expect(renderHook(useLanguage).result.current.language).toBe('en')
  })

  it('损坏的偏好明确提示，重新选择后恢复', () => {
    localStorage.setItem(LANGUAGE_STORAGE_KEY, 'invalid')
    const { result } = renderHook(useLanguage)
    expect(result.current.language).toBe('system')
    expect(result.current.error).toContain('语言偏好无效')
    act(() => result.current.setLanguage('zh-CN'))
    expect(result.current.error).toBe('')
    expect(localStorage.getItem(LANGUAGE_STORAGE_KEY)).toBe('zh-CN')
  })

  it('无法读取时提示，不静默忽略存储故障', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('不可读取')
    })
    expect(renderHook(useLanguage).result.current.error).toContain('无法读取语言偏好')
  })

  it('保存失败保留本次选择并允许重试', () => {
    const save = vi.spyOn(Storage.prototype, 'setItem').mockImplementationOnce(() => {
      throw new Error('不可写入')
    })
    const { result } = renderHook(useLanguage)
    act(() => result.current.setLanguage('en'))
    expect(result.current.language).toBe('en')
    expect(result.current.error).toContain('未能保存')
    expect(localStorage.getItem(LANGUAGE_STORAGE_KEY)).toBeNull()
    act(() => result.current.setLanguage('en'))
    expect(result.current.error).toBe('')
    expect(save).toHaveBeenCalledTimes(2)
    expect(localStorage.getItem(LANGUAGE_STORAGE_KEY)).toBe('en')
  })
})
