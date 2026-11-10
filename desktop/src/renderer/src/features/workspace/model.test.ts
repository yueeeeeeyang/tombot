import { describe, expect, it } from 'vitest'
import { WORKSPACE_AVATAR } from '../avatars/presets'
import {
  addAgent,
  createWorkspace,
  exportConversation,
  MAX_AGENTS,
  MAX_MESSAGE_LENGTH,
  MAX_MESSAGES,
  saveMessage,
  searchAgents,
  updateDraft,
  workspaceSchema,
  type NewAgent,
} from './model'

const profile: NewAgent = {
  name: '写作助手',
  description: '一起写作',
  avatar: 'ragdoll',
  group: '未分组',
}
const timestamp = '2026-11-09T07:00:00.000Z'

describe('工作空间领域规则', () => {
  it('初始工作空间包含独立助手，没有伪造消息', () => {
    const workspace = createWorkspace()
    expect(workspaceSchema.safeParse(workspace).success).toBe(true)
    expect(workspace.agents).toHaveLength(8)
    expect(new Set(workspace.agents.map((agent) => agent.avatar)).size).toBe(8)
    expect(workspace.agents.every((agent) => agent.messages.length === 0)).toBe(true)
  })

  it('创建助手时规范化名称，并切换到新助手', () => {
    const original = createWorkspace()
    const workspace = addAgent(original, { ...profile, name: '  写作助手  ' }, 'writer')
    expect(workspace.activeAgentId).toBe('writer')
    expect(workspace.agents.at(-1)?.name).toBe('写作助手')
    expect(workspace.agents.at(-1)?.avatar).toBe('ragdoll')
    expect(original.agents).toHaveLength(8)
  })

  it('拒绝未知头像和远程头像地址', () => {
    for (const avatar of ['', 'black', 'https://example.test/avatar.png', WORKSPACE_AVATAR.id]) {
      const workspace = createWorkspace()
      expect(
        workspaceSchema.safeParse({
          ...workspace,
          agents: [{ ...workspace.agents[0], avatar }],
        }).success,
      ).toBe(false)
    }
  })

  it.each(['', '   ', '字'.repeat(25)])('拒绝无效助手名称：%s', (name) => {
    expect(() => addAgent(createWorkspace(), { ...profile, name }, 'writer')).toThrow()
  })

  it('拒绝重复名称、重复标识和超限助手数量', () => {
    const workspace = addAgent(createWorkspace(), profile, 'writer')
    expect(() => addAgent(workspace, profile, 'writer-two')).toThrow('已存在')
    expect(() => addAgent(workspace, { ...profile, name: '另一位助手' }, 'writer')).toThrow(
      '已存在',
    )
    let full = createWorkspace()
    for (let index = full.agents.length; index < MAX_AGENTS; index++) {
      full = addAgent(full, { ...profile, name: `助手 ${index}` }, `agent-${index}`)
    }
    expect(() => addAgent(full, profile, 'writer')).toThrow('最多创建')
  })

  it('草稿按助手隔离，保存时保留换行并清空输入', () => {
    const workspace = updateDraft(createWorkspace(), 'assistant', '  第一行\n第二行  ')
    const saved = saveMessage(workspace, 'assistant', 'message-one', timestamp)
    expect(saved.agents[0]?.messages[0]?.content).toBe('第一行\n第二行')
    expect(saved.agents[0]?.draft).toBe('')
    expect(saved.agents[1]?.messages).toHaveLength(0)
    expect(workspace.agents[0]?.messages).toHaveLength(0)
  })

  it('允许长度边界，拒绝超长草稿和未知助手', () => {
    expect(
      updateDraft(createWorkspace(), 'assistant', '字'.repeat(MAX_MESSAGE_LENGTH)).agents[0]?.draft
        .length,
    ).toBe(MAX_MESSAGE_LENGTH)
    expect(() =>
      updateDraft(createWorkspace(), 'assistant', '字'.repeat(MAX_MESSAGE_LENGTH + 1)),
    ).toThrow('最多')
    expect(() => updateDraft(createWorkspace(), 'missing', '内容')).toThrow('不存在')
    expect(() => saveMessage(createWorkspace(), 'missing', 'message-one', timestamp)).toThrow(
      '不存在',
    )
  })

  it('拒绝空白消息、非法时间和重复提交', () => {
    expect(() =>
      saveMessage(
        updateDraft(createWorkspace(), 'assistant', '  \n'),
        'assistant',
        'message-one',
        timestamp,
      ),
    ).toThrow('有效内容')
    const workspace = updateDraft(createWorkspace(), 'assistant', '一个想法')
    expect(() => saveMessage(workspace, 'assistant', 'message-one', '无效日期')).toThrow()
    const saved = updateDraft(
      saveMessage(workspace, 'assistant', 'message-one', timestamp),
      'assistant',
      '再次提交',
    )
    expect(() => saveMessage(saved, 'assistant', 'message-one', timestamp)).toThrow('重复提交')
  })

  it('全局消息容量有界且失败保留草稿', () => {
    const workspace = updateDraft(createWorkspace(), 'assistant', '尚未保存')
    workspace.agents[0]!.messages = Array.from({ length: MAX_MESSAGES }, (_, index) => ({
      id: `message-${index}`,
      content: '内容',
      createdAt: timestamp,
    }))
    expect(() => saveMessage(workspace, 'assistant', 'overflow', timestamp)).toThrow('上限')
    expect(workspace.agents[0]?.draft).toBe('尚未保存')
  })

  it('支持助手、分组和消息搜索，空白查询返回全部助手', () => {
    const workspace = saveMessage(
      updateDraft(createWorkspace(), 'assistant', '为 TOMBOT 编写界面'),
      'assistant',
      'message-one',
      timestamp,
    )
    expect(searchAgents(workspace.agents, ' tombot ').map((agent) => agent.id)).toEqual([
      'assistant',
    ])
    expect(searchAgents(workspace.agents, '游戏开发')).toHaveLength(4)
    expect(searchAgents(workspace.agents, '无匹配内容')).toHaveLength(0)
    expect(searchAgents(workspace.agents, '  ')).toHaveLength(8)
  })

  it('导出仅包含用户草稿，并明确标注未发送', () => {
    const workspace = updateDraft(
      saveMessage(
        updateDraft(createWorkspace(), 'assistant', '已存内容'),
        'assistant',
        'message-one',
        timestamp,
      ),
      'assistant',
      '未完成输入',
    )
    const markdown = exportConversation(workspace.agents[0]!)
    expect(markdown).toContain('未发送')
    expect(markdown).toContain('已存内容')
    expect(markdown).toContain('未完成输入')
    expect(markdown).not.toContain('你的想法，有了一个新家')
  })

  it('拒绝损坏的活动助手引用和重复标识', () => {
    expect(
      workspaceSchema.safeParse({ ...createWorkspace(), activeAgentId: 'missing' }).success,
    ).toBe(false)
    const workspace = createWorkspace()
    workspace.agents[1]!.id = 'assistant'
    expect(workspaceSchema.safeParse(workspace).success).toBe(false)
  })
})
