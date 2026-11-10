import { z } from 'zod'
import { CAT_AVATAR_IDS, DEFAULT_AGENT_AVATARS } from '../avatars/presets'
import { CAT_MODEL_IDS, DEFAULT_CAT_MODEL_ID } from '../models/catalog'
import { isReactionEmoji, REACTION_EMOJIS, type ReactionEmoji } from './reactions'

export const MAX_MESSAGE_LENGTH = 10_000
export const MAX_MESSAGES = 500
export const MAX_AGENTS = 64
export const REPLY_EXCERPT_LENGTH = 80

export const groupNameSchema = z
  .string()
  .trim()
  .min(1, '请输入分组名称')
  .max(24, '分组名称最多 24 个字')

const legacyMessageSchema = z.object({
  id: z.string().min(1),
  content: z.string().trim().min(1).max(MAX_MESSAGE_LENGTH),
  createdAt: z.string().datetime(),
})

const legacyAgentSchema = z.object({
  id: z.string().min(1),
  name: z.string().trim().min(1, '请输入助手名称').max(24, '助手名称最多 24 个字'),
  description: z.string().trim().max(120, '助手介绍最多 120 个字'),
  group: groupNameSchema,
  avatar: z.enum(CAT_AVATAR_IDS, { error: '请选择预设猫咪头像' }),
  messages: z.array(legacyMessageSchema).max(MAX_MESSAGES),
  draft: z.string().max(MAX_MESSAGE_LENGTH),
})

function refineWorkspace(
  workspace: { activeAgentId: string; agents: { id: string; messages: readonly unknown[] }[] },
  context: { addIssue: (issue: { code: 'custom'; message: string }) => void },
): void {
  const ids = workspace.agents.map((agent) => agent.id)
  if (
    new Set(ids).size !== ids.length ||
    !ids.includes(workspace.activeAgentId) ||
    !ids.includes('assistant')
  ) {
    context.addIssue({ code: 'custom', message: '工作空间的助手标识无效' })
  }
  if (workspace.agents.reduce((count, agent) => count + agent.messages.length, 0) > MAX_MESSAGES) {
    context.addIssue({ code: 'custom', message: '本地消息数量已达上限' })
  }
}

/**
 * 冻结的 v2 工作空间。历史迁移脚本直接用它校验 v1 升级结果，字段不能再改。
 */
export const workspaceSchema = z
  .object({
    version: z.literal(2),
    activeAgentId: z.string().min(1),
    agents: z.array(legacyAgentSchema).min(1).max(MAX_AGENTS),
  })
  .superRefine(refineWorkspace)

export type Workspace = z.infer<typeof workspaceSchema>

const replySchema = z.object({
  messageId: z.string().min(1),
  excerpt: z.string().trim().min(1).max(REPLY_EXCERPT_LENGTH),
})

const reactionSchema = z.object({
  emoji: z.enum(REACTION_EMOJIS),
  count: z.number().int().min(1).max(999),
})

const attachmentSchema = z.object({
  name: z.string().trim().min(1).max(200),
  path: z.string().trim().min(1).max(500),
  sha256: z.string().trim().length(64),
  mediaType: z.string().trim().max(120),
  kind: z.enum(['image', 'file']),
})

const dmNoticeSchema = z.object({
  direction: z.enum(['sent', 'received']),
  peerId: z.string().trim().min(1).max(80),
  peerName: z.string().trim().min(1).max(80),
  conversationId: z.string().trim().min(1).max(80),
})

const messageSchema = z
  .object({
    id: z.string().min(1),
    role: z.enum(['user', 'assistant']),
    content: z.string().trim().min(1).max(MAX_MESSAGE_LENGTH),
    createdAt: z.string().datetime(),
    status: z.enum(['unsent', 'failed', 'sent']),
    requestId: z.string().trim().min(1).max(128).optional(),
    replyTo: replySchema.optional(),
    reactions: z.array(reactionSchema).max(REACTION_EMOJIS.length),
    attachments: z.array(attachmentSchema).max(4).optional(),
    dmNotice: dmNoticeSchema.optional(),
  })
  .superRefine((message, context) => {
    const emojis = message.reactions.map((reaction) => reaction.emoji)
    if (new Set(emojis).size !== emojis.length) {
      context.addIssue({ code: 'custom', message: '同一条消息不能重复记录同一个表情' })
    }
    if (message.replyTo?.messageId === message.id) {
      context.addIssue({ code: 'custom', message: '消息不能回复自己' })
    }
  })

/** 冻结的 v3 助手。v3 迁移脚本校验的就是这份结构，不能再加字段。 */
const v3AgentSchema = z.object({
  id: z.string().min(1),
  name: z.string().trim().min(1, '请输入助手名称').max(24, '助手名称最多 24 个字'),
  description: z.string().trim().max(120, '助手介绍最多 120 个字'),
  group: groupNameSchema,
  avatar: z.enum(CAT_AVATAR_IDS, { error: '请选择预设猫咪头像' }),
  messages: z.array(messageSchema).max(MAX_MESSAGES),
  draft: z.string().max(MAX_MESSAGE_LENGTH),
})

/** 冻结的 v3 工作空间。v3 迁移脚本直接解析它，版本号和字段保持不动。 */
export const currentWorkspaceSchema = z
  .object({
    version: z.literal(3),
    activeAgentId: z.string().min(1),
    agents: z.array(v3AgentSchema).min(1).max(MAX_AGENTS),
  })
  .superRefine(refineWorkspace)

export type CurrentWorkspace = z.infer<typeof currentWorkspaceSchema>

/** 冻结的 v4 助手。v4 迁移脚本校验的就是这份结构，不能再加字段。 */
const v4AgentSchema = z.object({
  id: z.string().min(1),
  name: z.string().trim().min(1, '请输入助手名称').max(24, '助手名称最多 24 个字'),
  description: z.string().trim().max(120, '助手介绍最多 120 个字'),
  group: groupNameSchema,
  avatar: z.enum(CAT_AVATAR_IDS, { error: '请选择预设猫咪头像' }),
  modelId: z.enum(CAT_MODEL_IDS, { error: '请选择列表中的模型' }),
  messages: z.array(messageSchema).max(MAX_MESSAGES),
  draft: z.string().max(MAX_MESSAGE_LENGTH),
})

/** 冻结的 v4 工作空间。v4 迁移直接解析它，版本号保持不动。 */
export const liveWorkspaceSchema = z
  .object({
    version: z.literal(4),
    activeAgentId: z.string().min(1),
    agents: z.array(v4AgentSchema).min(1).max(MAX_AGENTS),
  })
  .superRefine(refineWorkspace)

export type LiveWorkspace = z.infer<typeof liveWorkspaceSchema>

export const MAX_ARTIFACTS = 100
export const MAX_ARTIFACT_TITLE = 80
export const MAX_ARTIFACT_PREVIEW = 16_000
export const MAX_COMPUTER_URL = 2_048

const artifactPreviewPattern = /^data:image\/(?:png|jpeg|webp);base64,[a-zA-Z0-9+/=]+$/

export const artifactSchema = z.object({
  id: z.string().min(1).max(80),
  title: z.string().trim().min(1).max(MAX_ARTIFACT_TITLE),
  kind: z.enum(['image', 'video', 'file']),
  createdAt: z.string().datetime(),
  preview: z.string().max(MAX_ARTIFACT_PREVIEW).regex(artifactPreviewPattern).optional(),
})

/** 空字符串表示未连接；非空必须是不含账号的 HTTP(S) 地址。 */
export function isComputerUrl(value: string): boolean {
  if (!value) return true
  try {
    const url = new URL(value)
    return ['http:', 'https:'].includes(url.protocol) && !url.username && !url.password
  } catch {
    return false
  }
}

export const agentSchema = z
  .object({
    id: z.string().min(1),
    name: z.string().trim().min(1, '请输入助手名称').max(24, '助手名称最多 24 个字'),
    description: z.string().trim().max(120, '助手介绍最多 120 个字'),
    group: groupNameSchema,
    avatar: z.enum(CAT_AVATAR_IDS, { error: '请选择预设猫咪头像' }),
    modelId: z.string().trim().min(1, '请选择列表中的模型').max(120, '模型名称过长'),
    messages: z.array(messageSchema).max(MAX_MESSAGES),
    draft: z.string().max(MAX_MESSAGE_LENGTH),
    artifacts: z.array(artifactSchema).max(MAX_ARTIFACTS),
    computerUrl: z.string().max(MAX_COMPUTER_URL),
  })
  .superRefine((agent, context) => {
    const ids = agent.artifacts.map((artifact) => artifact.id)
    if (new Set(ids).size !== ids.length) {
      context.addIssue({ code: 'custom', message: '产物标识不能重复' })
    }
    if (!isComputerUrl(agent.computerUrl)) {
      context.addIssue({ code: 'custom', message: '请输入不含登录凭据的 HTTP 或 HTTPS 地址。' })
    }
  })

export const appWorkspaceSchema = z
  .object({
    version: z.literal(5),
    activeAgentId: z.string().min(1),
    agents: z.array(agentSchema).min(1).max(MAX_AGENTS),
  })
  .superRefine(refineWorkspace)

export type Agent = z.infer<typeof agentSchema>
export type Artifact = z.infer<typeof artifactSchema>
export type ChatMessage = z.infer<typeof messageSchema>
export type MessageAttachment = z.infer<typeof attachmentSchema>
export type DmNotice = z.infer<typeof dmNoticeSchema>
export type AppWorkspace = z.infer<typeof appWorkspaceSchema>
export type NewAgent = Pick<Agent, 'name' | 'description' | 'group' | 'avatar'>
export type ModelTurn = { role: 'user' | 'assistant'; content: string }

/** 创建首次使用的工作空间；预设助手没有伪造会话或模型回复。 */
export function createWorkspace(): AppWorkspace {
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
    version: 5,
    activeAgentId: 'assistant',
    agents: profiles.map(([id, name, description, group]) => ({
      id,
      name,
      description,
      group,
      avatar: DEFAULT_AGENT_AVATARS[id],
      modelId: DEFAULT_CAT_MODEL_ID,
      messages: [],
      draft: '',
      artifacts: [],
      computerUrl: '',
    })),
  }
}

/** 校验名称及容量后添加助手；冲突或输入无效时抛出中文错误。 */
export function addAgent(
  workspace: AppWorkspace,
  profile: NewAgent,
  id: string,
  modelId: string = DEFAULT_CAT_MODEL_ID,
): AppWorkspace {
  if (workspace.agents.length >= MAX_AGENTS) throw new Error(`最多创建 ${MAX_AGENTS} 个助手`)
  const result = agentSchema.safeParse({
    ...profile,
    id,
    modelId,
    messages: [],
    draft: '',
    artifacts: [],
    computerUrl: '',
  })
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

/** 更换指定猫咪的模型；未知模型或猫咪不存在时抛错，不改原工作空间。 */
export function setAgentModel(
  workspace: AppWorkspace,
  agentId: string,
  modelId: string,
): AppWorkspace {
  const trimmed = modelId.trim()
  if (!trimmed || trimmed.length > 120) throw new Error('请选择列表中的模型')
  if (!workspace.agents.some((agent) => agent.id === agentId)) throw new Error('助手不存在')
  return {
    ...workspace,
    agents: workspace.agents.map((agent) =>
      agent.id === agentId ? { ...agent, modelId: trimmed } : agent,
    ),
  }
}

/** 保存或清空某只猫咪的远端电脑地址；非法地址或猫咪不存在时抛错，不改原工作空间。 */
export function setAgentComputerUrl(
  workspace: AppWorkspace,
  agentId: string,
  computerUrl: string,
): AppWorkspace {
  const trimmed = computerUrl.trim()
  if (trimmed.length > MAX_COMPUTER_URL) throw new Error('远端电脑地址过长。')
  if (!isComputerUrl(trimmed)) throw new Error('请输入不含登录凭据的 HTTP 或 HTTPS 地址。')
  if (!workspace.agents.some((agent) => agent.id === agentId)) throw new Error('助手不存在')
  const canonical = trimmed ? new URL(trimmed).toString() : ''
  return {
    ...workspace,
    agents: workspace.agents.map((agent) =>
      agent.id === agentId ? { ...agent, computerUrl: canonical } : agent,
    ),
  }
}

/** 把服务端已经写下的私信提示插进当前会话，同一条标识不重复。 */
export function insertDmNotice(
  workspace: AppWorkspace,
  agentId: string,
  notice: DmNotice & { id: string; createdAt: string },
): AppWorkspace {
  const agent = workspace.agents.find((item) => item.id === agentId)
  if (!agent || agent.messages.some((message) => message.id === notice.id)) return workspace
  const content =
    notice.direction === 'received'
      ? `收到 ${notice.peerName} 的消息`
      : `已发消息给 ${notice.peerName}`
  const message = messageSchema.parse({
    id: notice.id,
    role: 'assistant',
    content,
    createdAt: notice.createdAt,
    status: 'sent',
    reactions: [],
    dmNotice: {
      direction: notice.direction,
      peerId: notice.peerId,
      peerName: notice.peerName,
      conversationId: notice.conversationId,
    },
  })
  return {
    ...workspace,
    agents: workspace.agents.map((item) =>
      item.id === agentId ? { ...item, messages: [...item.messages, message] } : item,
    ),
  }
}

/** 更新指定助手的输入草稿，不改变其他会话；超长输入或助手不存在时抛出错误。 */
export function updateDraft(workspace: AppWorkspace, agentId: string, draft: string): AppWorkspace {
  if (draft.length > MAX_MESSAGE_LENGTH) throw new Error(`消息最多 ${MAX_MESSAGE_LENGTH} 个字`)
  if (!workspace.agents.some((agent) => agent.id === agentId)) throw new Error('助手不存在')
  return {
    ...workspace,
    agents: workspace.agents.map((agent) => (agent.id === agentId ? { ...agent, draft } : agent)),
  }
}

/** 把被回复原文收成单行摘要；按码点截断，避免把表情切成半个。 */
export function replyExcerpt(content: string): string {
  const singleLine = content.replace(/\s+/g, ' ').trim()
  const characters = [...singleLine]
  if (characters.length <= REPLY_EXCERPT_LENGTH) return singleLine
  return `${characters.slice(0, REPLY_EXCERPT_LENGTH - 1).join('')}…`
}

/** 将输入存为明确的未发送消息；空白、容量、重复标识或失效回复都不会改变原工作空间。 */
export function saveMessage(
  workspace: AppWorkspace,
  agentId: string,
  id: string,
  createdAt: string,
  replyToId?: string,
  attachments: MessageAttachment[] = [],
): AppWorkspace {
  const agent = workspace.agents.find((candidate) => candidate.id === agentId)
  if (!agent) throw new Error('助手不存在')
  const replyTarget = replyToId
    ? agent.messages.find((message) => message.id === replyToId)
    : undefined
  if (replyToId && !replyTarget) throw new Error('回复的消息不存在')
  const content = agent.draft.trim() || attachments.map((item) => item.name).join('、')
  const result = messageSchema.safeParse({
    id,
    role: 'user',
    content,
    createdAt,
    status: 'unsent',
    reactions: [],
    ...(attachments.length > 0 ? { attachments } : {}),
    ...(replyTarget
      ? { replyTo: { messageId: replyTarget.id, excerpt: replyExcerpt(replyTarget.content) } }
      : {}),
  })
  if (!result.success) throw new Error('请输入有效内容，消息最多 10000 个字')
  if (workspace.agents.reduce((count, item) => count + item.messages.length, 0) >= MAX_MESSAGES) {
    throw new Error('本地消息已达 500 条上限，请先删除不需要的草稿')
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

/**
 * 切换当前用户对某条消息的表情。
 * 再次点同一个表情会整颗取消，而不是累加；数量大于 1 只用于展示已经存在的计数。
 */
export function toggleReaction(
  workspace: AppWorkspace,
  agentId: string,
  messageId: string,
  emoji: string,
): AppWorkspace {
  if (!isReactionEmoji(emoji)) throw new Error('不支持这个表情')
  const agent = workspace.agents.find((item) => item.id === agentId)
  if (!agent) throw new Error('助手不存在')
  if (!agent.messages.some((message) => message.id === messageId)) throw new Error('消息不存在')
  return {
    ...workspace,
    agents: workspace.agents.map((item) =>
      item.id === agentId
        ? {
            ...item,
            messages: item.messages.map((message) =>
              message.id === messageId
                ? { ...message, reactions: nextReactions(message, emoji) }
                : message,
            ),
          }
        : item,
    ),
  }
}

function nextReactions(message: ChatMessage, emoji: ReactionEmoji): ChatMessage['reactions'] {
  if (message.reactions.some((reaction) => reaction.emoji === emoji)) {
    return message.reactions.filter((reaction) => reaction.emoji !== emoji)
  }
  return [...message.reactions, { emoji, count: 1 }]
}

/**
 * 组装交给模型的对话。回复会附上被引用的原文，避免模型只看到孤立的新句子。
 * 原文还在时用全文，已被删除时退回保存下来的摘要。说明文字用英文，因为这是模型上下文。
 */
export function toModelTranscript(messages: ChatMessage[]): ModelTurn[] {
  const byId = new Map(messages.map((message) => [message.id, message]))
  return messages.map((message) => ({
    role: message.role,
    content: contentForModel(
      message,
      message.replyTo ? byId.get(message.replyTo.messageId) : undefined,
    ),
  }))
}

function contentForModel(message: ChatMessage, quoted: ChatMessage | undefined): string {
  if (!message.replyTo) return message.content
  const lead =
    message.role === 'user'
      ? 'The user is replying to an earlier message.'
      : 'This assistant message replies to an earlier message.'
  return [
    lead,
    'Quoted message:',
    quoted?.content ?? message.replyTo.excerpt,
    'Reply:',
    message.content,
  ].join('\n')
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
