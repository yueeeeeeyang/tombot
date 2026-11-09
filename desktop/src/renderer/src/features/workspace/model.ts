import { z } from 'zod'
import { CAT_AVATAR_IDS, DEFAULT_AGENT_AVATARS } from '../avatars/presets'

export const MAX_MESSAGE_LENGTH = 10_000
export const MAX_MESSAGES = 500
export const MAX_AGENTS = 64

const messageSchema = z.object({
  id: z.string().min(1),
  content: z.string().trim().min(1).max(MAX_MESSAGE_LENGTH),
  createdAt: z.string().datetime(),
})

export const groupNameSchema = z
  .string()
  .trim()
  .min(1, '请输入分组名称')
  .max(24, '分组名称最多 24 个字')

export const agentSchema = z.object({
  id: z.string().min(1),
  name: z.string().trim().min(1, '请输入助手名称').max(24, '助手名称最多 24 个字'),
  description: z.string().trim().max(120, '助手介绍最多 120 个字'),
  group: groupNameSchema,
  avatar: z.enum(CAT_AVATAR_IDS, { error: '请选择预设猫咪头像' }),
  messages: z.array(messageSchema).max(MAX_MESSAGES),
  draft: z.string().max(MAX_MESSAGE_LENGTH),
})

export const workspaceSchema = z
  .object({
    version: z.literal(2),
    activeAgentId: z.string().min(1),
    agents: z.array(agentSchema).min(1).max(MAX_AGENTS),
  })
  .superRefine((workspace, context) => {
    const ids = workspace.agents.map((agent) => agent.id)
    if (
      new Set(ids).size !== ids.length ||
      !ids.includes(workspace.activeAgentId) ||
      !ids.includes('assistant')
    ) {
      context.addIssue({ code: 'custom', message: '工作空间的助手标识无效' })
    }
    if (
      workspace.agents.reduce((count, agent) => count + agent.messages.length, 0) > MAX_MESSAGES
    ) {
      context.addIssue({ code: 'custom', message: '本地消息数量已达上限' })
    }
  })

export type Agent = z.infer<typeof agentSchema>
export type Workspace = z.infer<typeof workspaceSchema>
export type NewAgent = Pick<Agent, 'name' | 'description' | 'group' | 'avatar'>

/** 创建首次使用的工作空间；预设助手没有伪造会话或模型回复。 */
export function createWorkspace(): Workspace {
  const profiles: Array<[keyof typeof DEFAULT_AGENT_AVATARS, string, string, string]> = [
    ['assistant', '小黑', '你的日常搭档，陪你把想法变成现实。', '我的助手'],
    ['developer', '游戏程序', '代码、逻辑和实现，交给我一起想。', '游戏开发'],
    ['producer', '游戏负责人 + 策划', '从一个好点子，到一份清晰的计划。', '游戏开发'],
    ['artist', '游戏美术', '一起寻找属于这个世界的视觉语言。', '游戏开发'],
    ['audio', '游戏音效', '让每一个场景，都有自己的声音。', '游戏开发'],
    ['coding', '编程搭档', '聊聊代码，也聊聊下一步怎么做。', '未分组'],
    ['news', '资讯助手', '为你整理值得关注的新鲜事。', '未分组'],
    ['organizer', '整理助手', '少一点杂乱，多一点井井有条。', '未分组'],
  ]
  return {
    version: 2,
    activeAgentId: 'assistant',
    agents: profiles.map(([id, name, description, group]) => ({
      id,
      name,
      description,
      group,
      avatar: DEFAULT_AGENT_AVATARS[id],
      messages: [],
      draft: '',
    })),
  }
}

/** 校验名称及容量后添加助手；冲突或输入无效时抛出中文错误。 */
export function addAgent(workspace: Workspace, profile: NewAgent, id: string): Workspace {
  if (workspace.agents.length >= MAX_AGENTS) throw new Error(`最多创建 ${MAX_AGENTS} 个助手`)
  const result = agentSchema.safeParse({ ...profile, id, messages: [], draft: '' })
  if (!result.success) throw new Error(result.error.issues[0]?.message ?? '请检查助手资料')
  const agent = result.data
  if (
    workspace.agents.some(
      (existing) =>
        existing.name.toLocaleLowerCase() === agent.name.toLocaleLowerCase() || existing.id === id,
    )
  ) {
    throw new Error('这个助手名称已存在，请换一个名称')
  }
  return { ...workspace, activeAgentId: id, agents: [...workspace.agents, agent] }
}

/** 更新指定助手的输入草稿，不改变其他会话；超长输入或助手不存在时抛出错误。 */
export function updateDraft(workspace: Workspace, agentId: string, draft: string): Workspace {
  if (draft.length > MAX_MESSAGE_LENGTH) throw new Error(`消息最多 ${MAX_MESSAGE_LENGTH} 个字`)
  if (!workspace.agents.some((agent) => agent.id === agentId)) throw new Error('助手不存在')
  return {
    ...workspace,
    agents: workspace.agents.map((agent) => (agent.id === agentId ? { ...agent, draft } : agent)),
  }
}

/** 将输入存为明确的未发送消息；空白、容量及重复标识错误不会改变原工作空间。 */
export function saveMessage(
  workspace: Workspace,
  agentId: string,
  id: string,
  createdAt: string,
): Workspace {
  const agent = workspace.agents.find((candidate) => candidate.id === agentId)
  if (!agent) throw new Error('助手不存在')
  const result = messageSchema.safeParse({ id, content: agent.draft, createdAt })
  if (!result.success) throw new Error('请输入有效内容，消息最多 10000 个字')
  if (workspace.agents.reduce((count, item) => count + item.messages.length, 0) >= MAX_MESSAGES) {
    throw new Error('本地消息已达 500 条上限，请先导出并删除不需要的草稿')
  }
  if (workspace.agents.some((item) => item.messages.some((message) => message.id === id))) {
    throw new Error('这条消息已经保存，请勿重复提交')
  }
  return {
    ...workspace,
    agents: workspace.agents.map((item) =>
      item.id === agentId
        ? { ...item, draft: '', messages: [...item.messages, result.data] }
        : item,
    ),
  }
}

/** 按助手资料和已保存消息进行本地检索；空查询返回全部助手。 */
export function searchAgents(agents: Agent[], query: string): Agent[] {
  const normalized = query.trim().toLocaleLowerCase()
  return agents.filter((agent) =>
    [
      agent.name,
      agent.description,
      agent.group,
      ...agent.messages.map((message) => message.content),
    ].some((text) => text.toLocaleLowerCase().includes(normalized)),
  )
}

/** 导出用户创建的本地内容，明确标注尚未发送；不包含界面的使用指南。 */
export function exportConversation(agent: Agent): string {
  const messages = agent.messages.map(
    (message) =>
      `### ${new Date(message.createdAt).toLocaleString('zh-CN')} · 未发送\n\n${message.content}`,
  )
  return [
    `# ${agent.name}\n\nTomBot 本地草稿 · 尚未发送至服务端`,
    ...messages,
    ...(agent.draft.trim() ? [`### 正在编辑\n\n${agent.draft}`] : []),
  ].join('\n\n')
}
