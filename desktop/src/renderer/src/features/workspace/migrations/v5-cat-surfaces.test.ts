import { describe, expect, it, vi } from 'vitest'
import { createWorkspace } from '../model'
import { loadWorkspace, persistWorkspace, STORAGE_KEY } from '../storage'
import { V3_STORAGE_KEY } from './v4-cat-model'
import { migrateCatSurfaces, V4_STORAGE_KEY } from './v5-cat-surfaces'

function createVersion4() {
  const workspace = createWorkspace()
  return {
    version: 4 as const,
    activeAgentId: workspace.activeAgentId,
    agents: workspace.agents.map((agent) => ({
      id: agent.id,
      name: agent.name,
      description: agent.description,
      group: agent.group,
      avatar: agent.avatar,
      modelId: agent.modelId,
      messages: agent.messages,
      draft: agent.draft,
    })),
  }
}

describe('资料库与远端电脑前向迁移', () => {
  it('补上空产物和空地址，并保留 v4 原文', () => {
    const previous = createVersion4()
    previous.agents[0]!.modelId = 'claude-sonnet'
    const serialized = JSON.stringify(previous)
    localStorage.setItem(V4_STORAGE_KEY, serialized)
    localStorage.setItem(V3_STORAGE_KEY, '损坏的更旧备份')
    const migrated = loadWorkspace(localStorage)
    expect(migrated.version).toBe(5)
    expect(migrated.agents[0]?.modelId).toBe('claude-sonnet')
    expect(
      migrated.agents.every((agent) => agent.artifacts.length === 0 && agent.computerUrl === ''),
    ).toBe(true)
    expect(localStorage.getItem(V4_STORAGE_KEY)).toBe(serialized)
    expect(JSON.parse(localStorage.getItem(STORAGE_KEY)!)).toEqual(migrated)
    const setItem = vi.spyOn(Storage.prototype, 'setItem')
    expect(loadWorkspace(localStorage)).toEqual(migrated)
    expect(setItem).not.toHaveBeenCalled()
  })

  it('已有 v5 时不读取 v4', () => {
    const workspace = createWorkspace()
    workspace.agents[0]!.computerUrl = 'https://desktop.example.test/'
    persistWorkspace(localStorage, workspace)
    localStorage.setItem(V4_STORAGE_KEY, '损坏的旧版本')
    expect(loadWorkspace(localStorage)).toEqual(workspace)
  })

  it('v4 损坏时中止迁移且不覆盖原文', () => {
    const previous = createVersion4()
    previous.agents = []
    const serialized = JSON.stringify(previous)
    localStorage.setItem(V4_STORAGE_KEY, serialized)
    expect(() => loadWorkspace(localStorage)).toThrow('原始内容已保留')
    expect(localStorage.getItem(V4_STORAGE_KEY)).toBe(serialized)
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull()
  })

  it('直接迁移拒绝当前版本', () => {
    expect(() => migrateCatSurfaces(createWorkspace())).toThrow()
  })
})
