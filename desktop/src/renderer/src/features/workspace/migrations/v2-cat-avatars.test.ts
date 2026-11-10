import { describe, expect, it, vi } from 'vitest'
import { DEFAULT_AGENT_AVATARS } from '../../avatars/presets'
import { createWorkspace, saveMessage, updateDraft } from '../model'
import { loadWorkspace, persistWorkspace, STORAGE_KEY } from '../storage'
import { migrateCatAvatars, PREVIOUS_STORAGE_KEY } from './v2-cat-avatars'

/** 构造包含未发送消息、编辑草稿和自建助手的历史格式，不依赖迁移映射生成期望值。 */
function createPreviousWorkspace() {
  const workspace = updateDraft(
    saveMessage(
      updateDraft(createWorkspace(), 'assistant', '保留这条消息'),
      'assistant',
      'message-one',
      '2026-11-09T07:00:00.000Z',
    ),
    'developer',
    '继续编辑的草稿',
  )
  const colors = ['black', 'orange', 'black', 'amber', 'blue', 'blue', 'amber', 'green']
  return {
    version: 1,
    activeAgentId: 'writer',
    agents: [
      ...workspace.agents.map((agent, index) => ({
        id: agent.id,
        name: agent.name,
        description: agent.description,
        group: agent.group,
        messages: agent.messages,
        draft: agent.draft,
        color: colors[index]!,
      })),
      {
        id: 'writer',
        name: '写作助手',
        description: '自建助手',
        group: '写作',
        color: 'purple',
        messages: [],
        draft: '写作草稿',
      },
    ],
  }
}

describe('猫咪头像前向迁移', () => {
  it('只替换头像格式，保留所有助手、消息、草稿和当前会话；原文保留作备份', () => {
    const previous = createPreviousWorkspace()
    expect(migrateCatAvatars(previous).version).toBe(2)
    expect(migrateCatAvatars(previous).agents[0]?.messages[0]).toEqual({
      id: 'message-one',
      content: '保留这条消息',
      createdAt: '2026-11-09T07:00:00.000Z',
    })
    const serialized = JSON.stringify(previous)
    localStorage.setItem(PREVIOUS_STORAGE_KEY, serialized)
    const migrated = loadWorkspace(localStorage)
    expect(migrated.version).toBe(3)
    expect(migrated.agents[0]?.messages[0]).toMatchObject({
      role: 'user',
      status: 'unsent',
      reactions: [],
      content: '保留这条消息',
    })
    expect(migrated.activeAgentId).toBe('writer')
    expect(migrated.agents).toHaveLength(9)
    previous.agents.forEach((agent, index) => {
      expect({ ...migrated.agents[index], color: agent.color }).toMatchObject(agent)
      expect(migrated.agents[index]).not.toHaveProperty('color')
    })
    expect(migrated.agents.map((agent) => agent.avatar)).toEqual([
      ...Object.values(DEFAULT_AGENT_AVATARS),
      'ragdoll',
    ])
    expect(localStorage.getItem(PREVIOUS_STORAGE_KEY)).toBe(serialized)
    expect(JSON.parse(localStorage.getItem(STORAGE_KEY)!)).toEqual(migrated)
    const setItem = vi.spyOn(Storage.prototype, 'setItem')
    expect(loadWorkspace(localStorage)).toEqual(migrated)
    expect(setItem).not.toHaveBeenCalled()
  })

  it('已有新版本时不读旧备份，不回退新头像或新草稿', () => {
    const workspace = updateDraft(createWorkspace(), 'assistant', '新版本草稿')
    workspace.agents[0]!.avatar = 'persian'
    persistWorkspace(localStorage, workspace)
    localStorage.setItem(PREVIOUS_STORAGE_KEY, '损坏的旧备份')
    expect(loadWorkspace(localStorage)).toEqual(workspace)
  })

  it.each(['black', 'orange', 'amber', 'blue', 'green', 'purple'])(
    '自建助手的旧颜色 %s 能迁移为有效头像',
    (color) => {
      const previous = createPreviousWorkspace()
      previous.agents.at(-1)!.color = color
      localStorage.setItem(PREVIOUS_STORAGE_KEY, JSON.stringify(previous))
      expect(loadWorkspace(localStorage).agents.at(-1)?.avatar).toBe(
        {
          black: 'bombay',
          orange: 'orange-tabby',
          amber: 'calico',
          blue: 'british-shorthair',
          green: 'american-shorthair',
          purple: 'ragdoll',
        }[color],
      )
    },
  )

  it.each(['版本', '颜色', '助手引用', '消息', '空列表'])(
    '旧数据的%s损坏时中止迁移且不覆盖原文',
    (invalid) => {
      const previous = createPreviousWorkspace()
      if (invalid === '版本') previous.version = 99
      if (invalid === '颜色') previous.agents[0]!.color = 'unknown'
      if (invalid === '助手引用') previous.activeAgentId = 'missing'
      if (invalid === '消息') previous.agents[0]!.messages[0]!.createdAt = '无效时间'
      if (invalid === '空列表') previous.agents = []
      const serialized = JSON.stringify(previous)
      localStorage.setItem(PREVIOUS_STORAGE_KEY, serialized)
      expect(() => loadWorkspace(localStorage)).toThrow('原始内容已保留')
      expect(localStorage.getItem(PREVIOUS_STORAGE_KEY)).toBe(serialized)
      expect(localStorage.getItem(STORAGE_KEY)).toBeNull()
    },
  )

  it('迁移写入失败明确上报，原文保持不变并允许下次重新迁移', () => {
    const serialized = JSON.stringify(createPreviousWorkspace())
    localStorage.setItem(PREVIOUS_STORAGE_KEY, serialized)
    const storage = {
      getItem: localStorage.getItem.bind(localStorage),
      setItem: () => {
        throw new DOMException('空间不足', 'QuotaExceededError')
      },
    }
    expect(() => loadWorkspace(storage)).toThrow('升级保存失败，原始草稿已保留')
    expect(localStorage.getItem(PREVIOUS_STORAGE_KEY)).toBe(serialized)
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull()
    expect(loadWorkspace(localStorage).agents.at(-1)?.draft).toBe('写作草稿')
  })
})
