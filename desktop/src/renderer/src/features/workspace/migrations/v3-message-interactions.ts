import { currentWorkspaceSchema, workspaceSchema, type CurrentWorkspace } from '../model'

export const V2_STORAGE_KEY = 'tombot.workspace.v2'

/**
 * 把 v2 草稿升级为带角色、发送状态和表情的 v3。
 * 历史消息都是用户尚未送出的草稿，所以角色固定为 user、状态为 unsent，表情为空。
 * 只做一次线性遍历，不锁存储；失败时不写 v3，v2 原文留作回滚。
 */
export function migrateMessageInteractions(previous: unknown): CurrentWorkspace {
  const workspace = workspaceSchema.parse(previous)
  return currentWorkspaceSchema.parse({
    version: 3,
    activeAgentId: workspace.activeAgentId,
    agents: workspace.agents.map((agent) => ({
      ...agent,
      messages: agent.messages.map((message) => ({
        id: message.id,
        role: 'user',
        content: message.content,
        createdAt: message.createdAt,
        status: 'unsent',
        reactions: [],
      })),
    })),
  })
}
