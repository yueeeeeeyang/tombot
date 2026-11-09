import { describe, expect, it, vi } from 'vitest'
import {
  SIDEBAR_STORAGE_KEY,
  createSidebarPreferences,
  loadSidebarPreferences,
  persistSidebarPreferences,
  removeSidebarAgent,
  setSidebarFlag,
} from './sidebarPreferences'
import type { WorkspaceStorage } from './storage'

const agentIds = ['assistant', 'developer', 'producer']

describe('侧栏展示偏好', () => {
  it('分别保存置顶、未读和隐藏状态，并为重复切换去重', () => {
    let preferences = createSidebarPreferences()
    preferences = setSidebarFlag(preferences, agentIds, 'developer', 'pinnedIds', true)
    preferences = setSidebarFlag(preferences, agentIds, 'developer', 'unreadIds', true)
    preferences = setSidebarFlag(preferences, agentIds, 'developer', 'hiddenIds', true)
    preferences = setSidebarFlag(preferences, agentIds, 'developer', 'unreadIds', true)
    expect(preferences).toEqual({
      version: 1,
      pinnedIds: ['developer'],
      unreadIds: ['developer'],
      hiddenIds: ['developer'],
    })
    preferences = setSidebarFlag(preferences, agentIds, 'developer', 'unreadIds', false)
    expect(preferences.unreadIds).toEqual([])
  })

  it('拒绝未知猫咪与默认猫咪的固定操作', () => {
    const preferences = createSidebarPreferences()
    expect(() => setSidebarFlag(preferences, agentIds, 'missing', 'unreadIds', true)).toThrow(
      '猫咪不存在',
    )
    expect(() => setSidebarFlag(preferences, agentIds, 'assistant', 'pinnedIds', true)).toThrow(
      '默认猫咪固定置顶',
    )
    expect(() => setSidebarFlag(preferences, agentIds, 'assistant', 'hiddenIds', true)).toThrow(
      '默认猫咪不能隐藏',
    )
  })

  it('更新时清理已删除猫咪的引用，删除猫咪时同时清理全部状态', () => {
    let preferences = createSidebarPreferences()
    preferences = setSidebarFlag(preferences, agentIds, 'producer', 'pinnedIds', true)
    preferences = setSidebarFlag(preferences, agentIds, 'producer', 'unreadIds', true)
    preferences = setSidebarFlag(preferences, agentIds, 'producer', 'hiddenIds', true)
    const remaining = setSidebarFlag(
      preferences,
      agentIds.slice(0, 2),
      'developer',
      'unreadIds',
      true,
    )
    expect(remaining).toEqual({
      version: 1,
      pinnedIds: [],
      unreadIds: ['developer'],
      hiddenIds: [],
    })
    expect(removeSidebarAgent(preferences, 'producer')).toEqual(createSidebarPreferences())
  })

  it('完整校验后保存并读取，损坏内容保留原文且不静默回退', () => {
    const preferences = setSidebarFlag(
      createSidebarPreferences(),
      agentIds,
      'developer',
      'unreadIds',
      true,
    )
    persistSidebarPreferences(localStorage, preferences)
    expect(loadSidebarPreferences(localStorage)).toEqual(preferences)
    localStorage.setItem(SIDEBAR_STORAGE_KEY, '损坏内容')
    expect(() => loadSidebarPreferences(localStorage)).toThrow('格式异常')
    expect(localStorage.getItem(SIDEBAR_STORAGE_KEY)).toBe('损坏内容')
  })

  it('存储写入失败时抛错，不改写原内容', () => {
    const setItem = vi.fn(() => {
      throw new Error('磁盘已满')
    })
    const storage: WorkspaceStorage = {
      getItem: () => null,
      setItem,
    }
    expect(() => persistSidebarPreferences(storage, createSidebarPreferences())).toThrow(
      '侧栏偏好保存失败',
    )
    expect(storage.getItem(SIDEBAR_STORAGE_KEY)).toBeNull()
  })
})
