import { DEFAULT_CAT_MODEL_ID } from '../../models/catalog'
import { currentWorkspaceSchema, liveWorkspaceSchema, type LiveWorkspace } from '../model'

export const V3_STORAGE_KEY = 'tombot.workspace.v3'

/**
 * 给 v3 助手补上模型标识。历史数据没有逐只选择，因此都落到目录中的默认模型。
 * 一次线性遍历，不锁存储；失败不写 v4，v3 原文留作回滚。
 */
export function migrateCatModel(previous: unknown): LiveWorkspace {
  const workspace = currentWorkspaceSchema.parse(previous)
  return liveWorkspaceSchema.parse({
    version: 4,
    activeAgentId: workspace.activeAgentId,
    agents: workspace.agents.map((agent) => ({
      ...agent,
      modelId: DEFAULT_CAT_MODEL_ID,
    })),
  })
}
