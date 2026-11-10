import { describe, expect, it } from 'vitest'
import { DEFAULT_CAT_MODEL_ID } from '../models/catalog'
import {
  addAgent,
  createWorkspace,
  MAX_AGENTS,
  MAX_MESSAGE_LENGTH,
  MAX_MESSAGES,
  replyExcerpt,
  REPLY_EXCERPT_LENGTH,
  saveMessage,
  searchAgents,
  setAgentComputerUrl,
  setAgentModel,
  toggleReaction,
  toModelTranscript,
  updateDraft,
  appWorkspaceSchema,
  type ChatMessage,
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
    expect(appWorkspaceSchema.safeParse(workspace).success).toBe(true)
    expect(workspace.version).toBe(5)
    expect(
      workspace.agents.every((agent) => agent.artifacts.length === 0 && agent.computerUrl === ''),
    ).toBe(true)
    expect(workspace.agents.every((agent) => agent.modelId === 'gpt-4o')).toBe(true)
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
    for (const avatar of ['', 'black', 'https://example.test/avatar.png', 'workspace-cat']) {
      const workspace = createWorkspace()
      expect(
        appWorkspaceSchema.safeParse({
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
      role: 'user',
      content: '内容',
      createdAt: timestamp,
      status: 'unsent',
      reactions: [],
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

  it('拒绝损坏的活动助手引用和重复标识', () => {
    expect(
      appWorkspaceSchema.safeParse({ ...createWorkspace(), activeAgentId: 'missing' }).success,
    ).toBe(false)
    const workspace = createWorkspace()
    workspace.agents[1]!.id = 'assistant'
    expect(appWorkspaceSchema.safeParse(workspace).success).toBe(false)
  })

  it('保存回复时记下原文摘要，目标丢失或空白输入不会改动草稿', () => {
    const workspace = saveMessage(
      updateDraft(createWorkspace(), 'assistant', '原来的想法\n还没说完'),
      'assistant',
      'message-one',
      timestamp,
    )
    const replied = saveMessage(
      updateDraft(workspace, 'assistant', '接着说'),
      'assistant',
      'message-two',
      timestamp,
      'message-one',
    )
    expect(replied.agents[0]?.messages[1]).toMatchObject({
      role: 'user',
      status: 'unsent',
      content: '接着说',
      reactions: [],
      replyTo: { messageId: 'message-one', excerpt: '原来的想法 还没说完' },
    })
    expect(replied.agents[0]?.draft).toBe('')
    expect(workspace.agents[0]?.messages).toHaveLength(1)
    const pending = updateDraft(workspace, 'assistant', '找不到对象')
    expect(() => saveMessage(pending, 'assistant', 'message-two', timestamp, 'missing')).toThrow(
      '不存在',
    )
    expect(pending.agents[0]?.draft).toBe('找不到对象')
  })

  it('同一表情再次切换会取消，未知表情和缺失消息不会改动原会话', () => {
    const workspace = saveMessage(
      updateDraft(createWorkspace(), 'assistant', '一个想法'),
      'assistant',
      'message-one',
      timestamp,
    )
    const reacted = toggleReaction(workspace, 'assistant', 'message-one', '👍')
    expect(reacted.agents[0]?.messages[0]?.reactions).toEqual([{ emoji: '👍', count: 1 }])
    expect(
      toggleReaction(reacted, 'assistant', 'message-one', '👍').agents[0]?.messages[0]?.reactions,
    ).toEqual([])
    expect(workspace.agents[0]?.messages[0]?.reactions).toEqual([])
    const counted = toggleReaction(workspace, 'assistant', 'message-one', '👍')
    counted.agents[0]!.messages[0]!.reactions = [{ emoji: '👍', count: 3 }]
    expect(
      toggleReaction(counted, 'assistant', 'message-one', '👍').agents[0]?.messages[0]?.reactions,
    ).toEqual([])
    expect(() => toggleReaction(workspace, 'assistant', 'message-one', '💩')).toThrow('不支持')
    expect(() => toggleReaction(workspace, 'assistant', 'missing', '👍')).toThrow('消息不存在')
    expect(() => toggleReaction(workspace, 'missing', 'message-one', '👍')).toThrow('助手不存在')
    expect(workspace.agents[0]?.messages[0]?.reactions).toEqual([])
  })

  it('拒绝重复表情和回复自己的消息', () => {
    const workspace = createWorkspace()
    const invalid = {
      ...workspace.agents[0]!,
      messages: [
        {
          id: 'message-one',
          role: 'user' as const,
          content: '内容',
          createdAt: timestamp,
          status: 'unsent' as const,
          reactions: [
            { emoji: '👍' as const, count: 1 },
            { emoji: '👍' as const, count: 2 },
          ],
          replyTo: { messageId: 'message-one', excerpt: '内容' },
        },
      ],
    }
    expect(
      appWorkspaceSchema.safeParse({
        ...workspace,
        agents: [invalid, ...workspace.agents.slice(1)],
      }).success,
    ).toBe(false)
  })

  it('回复摘要按码点截断，模型上下文使用英文说明和仍在的原文', () => {
    expect(replyExcerpt('第一行\n第二行')).toBe('第一行 第二行')
    expect(replyExcerpt('😀'.repeat(REPLY_EXCERPT_LENGTH + 1))).toBe(
      `${'😀'.repeat(REPLY_EXCERPT_LENGTH - 1)}…`,
    )
    const original: ChatMessage = {
      id: 'origin',
      role: 'assistant',
      content: '完整的原先回复',
      createdAt: timestamp,
      status: 'sent',
      reactions: [],
    }
    const reply: ChatMessage = {
      id: 'reply',
      role: 'user',
      content: '我接着说',
      createdAt: timestamp,
      status: 'unsent',
      reactions: [],
      replyTo: { messageId: 'origin', excerpt: '完整的原先回复' },
    }
    const transcript = toModelTranscript([original, reply])
    expect(transcript[0]).toEqual({ role: 'assistant', content: '完整的原先回复' })
    expect(transcript[1]?.content).toContain('The user is replying to an earlier message.')
    expect(transcript[1]?.content).toContain('Quoted message:\n完整的原先回复')
    expect(transcript[1]?.content).toContain('Reply:\n我接着说')
    const orphan = toModelTranscript([
      { ...reply, replyTo: { messageId: 'gone', excerpt: '只剩摘要' } },
    ])
    expect(orphan[0]?.content).toContain('Quoted message:\n只剩摘要')
    expect(orphan[0]?.content).not.toContain('完整的原先回复')
    const assistantReply = toModelTranscript([
      {
        ...original,
        replyTo: { messageId: 'reply', excerpt: '我接着说' },
      },
    ])
    expect(assistantReply[0]?.content).toContain(
      'This assistant message replies to an earlier message.',
    )
  })

  it('可以更换猫咪模型，空模型或猫咪不存在时不改原数据', () => {
    const workspace = createWorkspace()
    const changed = setAgentModel(workspace, 'assistant', 'claude-sonnet')
    expect(changed.agents[0]?.modelId).toBe('claude-sonnet')
    expect(workspace.agents[0]?.modelId).toBe(DEFAULT_CAT_MODEL_ID)
    expect(changed.agents[1]?.modelId).toBe(DEFAULT_CAT_MODEL_ID)
    const named = setAgentModel(workspace, 'assistant', '写作模型')
    expect(named.agents[0]?.modelId).toBe('写作模型')
    expect(() => setAgentModel(workspace, 'assistant', '   ')).toThrow('列表中的模型')
    expect(() => setAgentModel(workspace, 'missing', 'gpt-4o')).toThrow('助手不存在')
    expect(workspace.agents[0]?.modelId).toBe(DEFAULT_CAT_MODEL_ID)
  })

  it('保存远端电脑地址时规范化，拒绝凭据和未知猫咪', () => {
    const workspace = createWorkspace()
    const saved = setAgentComputerUrl(workspace, 'assistant', 'https://desktop.example.test/view')
    expect(saved.agents[0]?.computerUrl).toBe('https://desktop.example.test/view')
    expect(workspace.agents[0]?.computerUrl).toBe('')
    expect(setAgentComputerUrl(saved, 'assistant', '  ').agents[0]?.computerUrl).toBe('')
    expect(() =>
      setAgentComputerUrl(workspace, 'assistant', 'https://user:secret@desktop.example.test'),
    ).toThrow('登录凭据')
    expect(() => setAgentComputerUrl(workspace, 'missing', 'https://desktop.example.test')).toThrow(
      '助手不存在',
    )
    expect(workspace.agents[0]?.computerUrl).toBe('')
  })
})
