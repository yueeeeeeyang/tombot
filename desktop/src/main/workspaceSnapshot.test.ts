import { describe, expect, it } from 'vitest'
import {
  draftsFromWorkspace,
  workspaceFromSnapshot,
  workspaceSelection,
  type ServerSnapshot,
} from './workspaceSnapshot'

function snapshot(): ServerSnapshot {
  return {
    workspace: { active_bot_id: 'bot-b', primary_bot_id: 'bot-a', revision: 7 },
    bots: [
      {
        id: 'bot-a',
        name: '研究员',
        description: '查资料',
        avatar_ref: 'orange-tabby',
        category: '工作',
        system_prompt: 'You are a researcher.',
        tool_permissions: [],
        approval_mode: 'standard',
        accent_hue: 15,
        desktop_id: 'desktop-a',
        conversation_id: 'conv-a',
        has_custom_avatar: false,
        is_archived: false,
        revision: 1,
      },
      {
        id: 'bot-b',
        name: '整理员',
        description: '',
        avatar_ref: '不存在的头像',
        category: '',
        system_prompt: 'You are an organizer.',
        tool_permissions: [],
        approval_mode: 'standard',
        accent_hue: 200,
        desktop_id: 'desktop-b',
        conversation_id: 'conv-b',
        has_custom_avatar: false,
        is_archived: false,
        revision: 2,
      },
      {
        id: 'bot-c',
        name: '已归档',
        description: '',
        avatar_ref: '',
        category: '',
        system_prompt: 'Archived.',
        tool_permissions: [],
        approval_mode: 'standard',
        accent_hue: 0,
        desktop_id: 'desktop-c',
        conversation_id: 'conv-c',
        has_custom_avatar: false,
        is_archived: true,
        revision: 1,
      },
    ],
    groups: [],
    has_more_bots: false,
    has_more_groups: false,
    event_sequence: 12,
  }
}

describe('工作台快照映射', () => {
  it('只保留未归档机器人，并记录单聊会话', () => {
    const mapped = workspaceFromSnapshot(snapshot())
    expect(mapped.workspace.agents.map((agent) => agent.id)).toEqual(['bot-a', 'bot-b'])
    expect(mapped.conversations.get('bot-a')).toBe('conv-a')
    expect(mapped.conversations.has('bot-c')).toBe(false)
  })

  it('未知头像回退到默认，缺分类归入未分组，模型不来自客户端', () => {
    const mapped = workspaceFromSnapshot(snapshot())
    const [first, second] = mapped.workspace.agents
    expect(first?.avatar).toBe('orange-tabby')
    expect(second?.avatar).toBe('bombay')
    expect(second?.group).toBe('未分组')
    expect(first?.computerUrl).toBe('')
    expect(first?.messages).toEqual([])
  })

  it('服务端选择不存在时回退到首个机器人，不留下悬空标识', () => {
    const current = snapshot()
    current.workspace.active_bot_id = 'bot-missing'
    current.workspace.primary_bot_id = null
    const mapped = workspaceFromSnapshot(current)
    expect(mapped.workspace.activeAgentId).toBe('bot-a')
    expect(mapped.workspace.leadAgentId).toBe('bot-a')
  })

  it('空目录不会产生悬空选择', () => {
    const current = snapshot()
    current.bots = []
    const mapped = workspaceFromSnapshot(current)
    expect(mapped.workspace.agents).toEqual([])
    expect(mapped.workspace.activeAgentId).toBe('')
    expect(mapped.workspace.leadAgentId).toBe('')
  })

  it('草稿与产物按机器人身份从本机补回', () => {
    const mapped = workspaceFromSnapshot(snapshot(), {
      drafts: { 'bot-a': '未发送的草稿' },
      artifacts: {},
    })
    expect(mapped.workspace.agents[0]?.draft).toBe('未发送的草稿')
    expect(mapped.workspace.agents[1]?.draft).toBe('')
  })

  it('保存时只提交选择，草稿不进入服务端请求', () => {
    const mapped = workspaceFromSnapshot(snapshot(), {
      drafts: { 'bot-a': '草稿正文' },
      artifacts: {},
    })
    const selection = workspaceSelection(mapped.workspace, 7)
    expect(selection).toEqual({
      expected_revision: 7,
      active_bot_id: 'bot-b',
      primary_bot_id: 'bot-a',
    })
    expect(JSON.stringify(selection)).not.toContain('草稿正文')
  })

  it('选择指向已删除机器人时显式提交空值', () => {
    const mapped = workspaceFromSnapshot(snapshot())
    const workspace = { ...mapped.workspace, activeAgentId: 'bot-gone' }
    expect(workspaceSelection(workspace, 3).active_bot_id).toBeNull()
  })

  it('草稿收集忽略空草稿与空产物', () => {
    const mapped = workspaceFromSnapshot(snapshot())
    const workspace = {
      ...mapped.workspace,
      agents: mapped.workspace.agents.map((agent) =>
        agent.id === 'bot-a' ? { ...agent, draft: '待发送' } : agent,
      ),
    }
    expect(draftsFromWorkspace(workspace)).toEqual({ drafts: { 'bot-a': '待发送' }, artifacts: {} })
  })
})
