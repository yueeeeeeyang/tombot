import { describe, expect, it, vi } from 'vitest'
import { createWorkspace, saveMessage, updateDraft } from '../model'
import { loadWorkspace, persistWorkspace, STORAGE_KEY } from '../storage'
import { V2_STORAGE_KEY } from './v3-message-interactions'
import { migrateCatModel, V3_STORAGE_KEY } from './v4-cat-model'

function createVersion3() {
  const workspace = updateDraft(
    saveMessage(
      updateDraft(createWorkspace(), 'assistant', '保留这条消息'),
      'assistant',
      'message-one',
      '2026-11-09T07:00:00.000Z',
    ),
    'developer',
    '继续编辑',
  )
  return {
    version: 3 as const,
    activeAgentId: workspace.activeAgentId,
    agents: workspace.agents.map((agent) => ({
      id: agent.id,
      name: agent.name,
      description: agent.description,
      group: agent.group,
      avatar: agent.avatar,
      messages: agent.messages,
      draft: agent.draft,
    })),
  }
}

describe('猫咪模型前向迁移', () => {
  it('为每只猫咪补上默认模型，并保留 v3 原文', () => {
    const previous = createVersion3()
    const serialized = JSON.stringify(previous)
    localStorage.setItem(V3_STORAGE_KEY, serialized)
    localStorage.setItem(V2_STORAGE_KEY, '损坏的更旧备份')
    const migrated = loadWorkspace(localStorage)
    expect(migrated.version).toBe(5)
    expect(
      migrated.agents.every((agent) => agent.computerUrl === '' && agent.artifacts.length === 0),
    ).toBe(true)
    expect(migrated.agents.every((agent) => agent.modelId === 'gpt-4o')).toBe(true)
    expect(migrated.agents[0]?.messages[0]?.content).toBe('保留这条消息')
    expect(migrated.agents[1]?.draft).toBe('继续编辑')
    expect(localStorage.getItem(V3_STORAGE_KEY)).toBe(serialized)
    expect(JSON.parse(localStorage.getItem(STORAGE_KEY)!)).toEqual(migrated)
    const setItem = vi.spyOn(Storage.prototype, 'setItem')
    expect(loadWorkspace(localStorage)).toEqual(migrated)
    expect(setItem).not.toHaveBeenCalled()
  })

  it('已有 v4 时不读取 v3', () => {
    const workspace = updateDraft(createWorkspace(), 'assistant', '新版本草稿')
    workspace.agents[0]!.modelId = 'gemini-flash'
    persistWorkspace(localStorage, workspace)
    localStorage.setItem(V3_STORAGE_KEY, '损坏的旧版本')
    expect(loadWorkspace(localStorage)).toEqual(workspace)
  })

  it('v3 损坏时中止迁移且不覆盖原文', () => {
    const previous = createVersion3()
    previous.agents = []
    const serialized = JSON.stringify(previous)
    localStorage.setItem(V3_STORAGE_KEY, serialized)
    expect(() => loadWorkspace(localStorage)).toThrow('原始内容已保留')
    expect(localStorage.getItem(V3_STORAGE_KEY)).toBe(serialized)
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull()
  })

  it('直接迁移拒绝未知版本', () => {
    expect(() => migrateCatModel({ ...createVersion3(), version: 4 })).toThrow()
  })
})
