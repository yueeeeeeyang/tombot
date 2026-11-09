import { z } from 'zod'
import { DEFAULT_AGENT_AVATARS, type CatAvatarId } from '../../avatars/presets'
import { MAX_AGENTS, workspaceSchema, type Workspace } from '../model'

export const PREVIOUS_STORAGE_KEY = 'tombot.workspace.v1'

const previousWorkspaceSchema = z.object({
  version: z.literal(1),
  activeAgentId: z.string(),
  agents: z
    .array(
      z
        .object({
          id: z.string(),
          color: z.enum(['black', 'orange', 'amber', 'blue', 'green', 'purple']),
        })
        .passthrough(),
    )
    .min(1)
    .max(MAX_AGENTS),
})

const previousColorAvatars = {
  black: 'bombay',
  orange: 'orange-tabby',
  amber: 'calico',
  blue: 'british-shorthair',
  green: 'american-shorthair',
  purple: 'ragdoll',
} as const satisfies Record<string, CatAvatarId>

/** 将 v1 颜色迁移为猫咪预设并完整校验；无效旧数据抛错，由存储层保留原文。 */
export function migrateCatAvatars(previous: unknown): Workspace {
  const workspace = previousWorkspaceSchema.parse(previous)
  return workspaceSchema.parse({
    ...workspace,
    version: 2,
    agents: workspace.agents.map(({ color, ...agent }) => ({
      ...agent,
      avatar: Object.hasOwn(DEFAULT_AGENT_AVATARS, agent.id)
        ? DEFAULT_AGENT_AVATARS[agent.id as keyof typeof DEFAULT_AGENT_AVATARS]
        : previousColorAvatars[color],
    })),
  })
}
