import { appWorkspaceSchema, liveWorkspaceSchema, type AppWorkspace } from '../model'

export const V4_STORAGE_KEY = 'tombot.workspace.v4'

/**
 * 给 v4 助手补上空的资料库和未连接的远端电脑。
 * 不编造文件或地址；一次线性遍历，失败不写 v5，v4 原文留作回滚。
 */
export function migrateCatSurfaces(previous: unknown): AppWorkspace {
  const workspace = liveWorkspaceSchema.parse(previous)
  const ids = workspace.agents.map((agent) => agent.id)
  return appWorkspaceSchema.parse({
    version: 5,
    activeAgentId: workspace.activeAgentId,
    leadAgentId: ids.includes('assistant') ? 'assistant' : (ids[0] ?? ''),
    agents: workspace.agents.map((agent) => ({
      ...agent,
      artifacts: [],
      computerUrl: '',
    })),
  })
}
