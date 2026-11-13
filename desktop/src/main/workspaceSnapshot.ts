import type { Agent, AppWorkspace } from '../renderer/src/features/workspace/model'
import { CAT_AVATAR_IDS } from '../renderer/src/features/avatars/presets'
import { DEFAULT_CAT_MODEL_ID } from '../renderer/src/features/models/catalog'

/** 服务端机器人目录项；字段与 internal/bots/types.go 的 Bot 保持一致。 */
export interface ServerBot {
  id: string
  name: string
  description: string
  avatar_ref: string
  category: string
  system_prompt: string
  tool_permissions: string[]
  approval_mode: string
  accent_hue: number
  desktop_id: string
  conversation_id: string
  has_custom_avatar: boolean
  is_archived: boolean
  revision: number
}

export interface ServerGroup {
  id: string
  name: string
  purpose: string
  conversation_id: string
  member_ids: string[]
  is_archived: boolean
  revision: number
}

export interface ServerWorkspace {
  active_bot_id: string | null
  primary_bot_id: string | null
  revision: number
}

/** GET /api/v2/workspace 的完整快照；事件水位用于客户端增量同步。 */
export interface ServerSnapshot {
  workspace: ServerWorkspace
  bots: ServerBot[]
  groups: ServerGroup[]
  has_more_bots: boolean
  has_more_groups: boolean
  event_sequence: number
}

/** 本地草稿与界面状态；服务端没有对应字段，按用户分区后单独保存。 */
export interface LocalDrafts {
  drafts: Record<string, string>
  artifacts: Record<string, Agent['artifacts']>
}

const avatarIds = new Set<string>(CAT_AVATAR_IDS)

/** 头像只接受客户端已知的预设标识，未知值回退到默认头像而不是原样透传。 */
function avatarFor(bot: ServerBot): Agent['avatar'] {
  return avatarIds.has(bot.avatar_ref) ? (bot.avatar_ref as Agent['avatar']) : 'bombay'
}

/**
 * 把服务端目录映射成本地工作台。
 * 模型由服务端策略决定，客户端不再持有选择，因此这里只保留稳定的占位标识。
 */
export function workspaceFromSnapshot(
  snapshot: ServerSnapshot,
  local: LocalDrafts = { drafts: {}, artifacts: {} },
): { workspace: AppWorkspace; conversations: Map<string, string> } {
  const conversations = new Map<string, string>()
  const agents: Agent[] = snapshot.bots
    .filter((bot) => !bot.is_archived)
    .map((bot) => {
      // 单聊会话由服务端随机器人一起创建；客户端只按机器人身份记住它。
      if (bot.conversation_id) conversations.set(bot.id, bot.conversation_id)
      return {
        id: bot.id,
        name: bot.name,
        description: bot.description,
        group: bot.category || '未分组',
        avatar: avatarFor(bot),
        modelId: DEFAULT_CAT_MODEL_ID,
        messages: [],
        draft: local.drafts[bot.id] ?? '',
        artifacts: local.artifacts[bot.id] ?? [],
        computerUrl: '',
      }
    })
  const first = agents[0]?.id ?? ''
  const active = agents.some((agent) => agent.id === snapshot.workspace.active_bot_id)
    ? (snapshot.workspace.active_bot_id as string)
    : first
  const lead = agents.some((agent) => agent.id === snapshot.workspace.primary_bot_id)
    ? (snapshot.workspace.primary_bot_id as string)
    : active
  return {
    workspace: { version: 5, activeAgentId: active, leadAgentId: lead, agents },
    conversations,
  }
}

/** 保存时只提交服务端理解的字段；草稿和产物留在本机，不能借工作台写入服务端。 */
export function workspaceSelection(
  workspace: AppWorkspace,
  expectedRevision: number,
): {
  expected_revision: number
  active_bot_id: string | null
  primary_bot_id: string | null
} {
  const hasAgent = workspace.agents.some((agent) => agent.id === workspace.activeAgentId)
  const hasLead = workspace.agents.some((agent) => agent.id === workspace.leadAgentId)
  return {
    expected_revision: expectedRevision,
    active_bot_id: hasAgent ? workspace.activeAgentId : null,
    primary_bot_id: hasLead ? workspace.leadAgentId : null,
  }
}

/** 收集当前草稿与产物，按机器人身份分区保存。 */
export function draftsFromWorkspace(workspace: AppWorkspace): LocalDrafts {
  const drafts: LocalDrafts = { drafts: {}, artifacts: {} }
  for (const agent of workspace.agents) {
    if (agent.draft) drafts.drafts[agent.id] = agent.draft
    if (agent.artifacts.length > 0) drafts.artifacts[agent.id] = agent.artifacts
  }
  return drafts
}
