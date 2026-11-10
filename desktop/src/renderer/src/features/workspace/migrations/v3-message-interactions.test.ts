import { describe, expect, it, vi } from 'vitest'
import { createWorkspace, saveMessage, updateDraft } from '../model'
import { loadWorkspace, persistWorkspace, STORAGE_KEY } from '../storage'
import { PREVIOUS_STORAGE_KEY } from './v2-cat-avatars'
import { migrateMessageInteractions, V2_STORAGE_KEY } from './v3-message-interactions'

function createVersion2() {
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
    version: 2,
    activeAgentId: workspace.activeAgentId,
    agents: workspace.agents.map((agent) => ({
      id: agent.id,
      name: agent.name,
      description: agent.description,
      group: agent.group,
      avatar: agent.avatar,
      draft: agent.draft,
      messages: agent.messages.map((message) => ({
        id: message.id,
        content: message.content,
        createdAt: message.createdAt,
      })),
    })),
  }
}

describe('消息交互前向迁移', () => {
  it('为历史草稿补上用户角色、未发送状态和空表情，并保留 v2 原文', () => {
    const previous = createVersion2()
    const serialized = JSON.stringify(previous)
    localStorage.setItem(V2_STORAGE_KEY, serialized)
    localStorage.setItem(PREVIOUS_STORAGE_KEY, '损坏的更旧备份')
    const migrated = loadWorkspace(localStorage)
    expect(migrated.version).toBe(3)
    expect(migrated.activeAgentId).toBe('assistant')
    expect(migrated.agents).toHaveLength(previous.agents.length)
    expect(migrated.agents[1]?.draft).toBe('继续编辑')
    expect(migrated.agents[0]?.messages[0]).toEqual({
      id: 'message-one',
      role: 'user',
      content: '保留这条消息',
      createdAt: '2026-11-09T07:00:00.000Z',
      status: 'unsent',
      reactions: [],
    })
    expect(localStorage.getItem(V2_STORAGE_KEY)).toBe(serialized)
    expect(localStorage.getItem(PREVIOUS_STORAGE_KEY)).toBe('损坏的更旧备份')
    expect(JSON.parse(localStorage.getItem(STORAGE_KEY)!)).toEqual(migrated)
    const setItem = vi.spyOn(Storage.prototype, 'setItem')
    expect(loadWorkspace(localStorage)).toEqual(migrated)
    expect(setItem).not.toHaveBeenCalled()
  })

  it('已有 v3 时不读取 v2，也不会把新消息退回旧结构', () => {
    const workspace = updateDraft(createWorkspace(), 'assistant', '新版本草稿')
    persistWorkspace(localStorage, workspace)
    localStorage.setItem(V2_STORAGE_KEY, '损坏的旧版本')
    expect(loadWorkspace(localStorage)).toEqual(workspace)
  })

  it('v2 损坏时中止迁移且不覆盖原文', () => {
    const previous = createVersion2()
    previous.agents[0]!.messages[0]!.createdAt = '无效时间'
    const serialized = JSON.stringify(previous)
    localStorage.setItem(V2_STORAGE_KEY, serialized)
    expect(() => loadWorkspace(localStorage)).toThrow('原始内容已保留')
    expect(localStorage.getItem(V2_STORAGE_KEY)).toBe(serialized)
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull()
  })

  it('迁移写入失败时保留 v2，下次仍可重试', () => {
    const serialized = JSON.stringify(createVersion2())
    localStorage.setItem(V2_STORAGE_KEY, serialized)
    const storage = {
      getItem: localStorage.getItem.bind(localStorage),
      setItem: () => {
        throw new DOMException('空间不足', 'QuotaExceededError')
      },
    }
    expect(() => loadWorkspace(storage)).toThrow('升级保存失败，原始草稿已保留')
    expect(localStorage.getItem(V2_STORAGE_KEY)).toBe(serialized)
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull()
    expect(loadWorkspace(localStorage).agents[0]?.messages[0]?.content).toBe(
      createVersion2().agents[0]!.messages[0]!.content,
    )
  })

  it('直接迁移拒绝当前版本和空助手列表', () => {
    expect(() => migrateMessageInteractions({ ...createVersion2(), version: 3 })).toThrow()
    expect(() => migrateMessageInteractions({ ...createVersion2(), agents: [] })).toThrow()
  })
})
