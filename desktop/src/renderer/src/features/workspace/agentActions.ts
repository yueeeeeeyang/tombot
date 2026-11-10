import { agentSchema, groupNameSchema, type CurrentWorkspace as Workspace } from './model'

/** 校验猫咪存在后重命名；禁止空白、超长和重复名称，消息与标识保持不变。 */
export function renameAgent(workspace: Workspace, id: string, name: string): Workspace {
  if (!workspace.agents.some((agent) => agent.id === id)) throw new Error('猫咪不存在')
  const result = agentSchema.shape.name.safeParse(name)
  if (!result.success) throw new Error(result.error.issues[0]!.message)
  if (
    workspace.agents.some(
      (agent) =>
        agent.id !== id && agent.name.toLocaleLowerCase() === result.data.toLocaleLowerCase(),
    )
  ) {
    throw new Error('这个猫咪名称已存在，请换一个名称')
  }
  return {
    ...workspace,
    agents: workspace.agents.map((agent) =>
      agent.id === id ? { ...agent, name: result.data } : agent,
    ),
  }
}

/** 将猫咪移至已有分组或未分组；默认猫咪保留独立入口，不接受无效目标。 */
export function moveAgent(workspace: Workspace, id: string, group: string): Workspace {
  if (!workspace.agents.some((agent) => agent.id === id)) throw new Error('猫咪不存在')
  if (id === 'assistant') throw new Error('默认猫咪不参与分组')
  if (!getAgentGroups(workspace).includes(group)) throw new Error('目标分组不存在')
  return {
    ...workspace,
    agents: workspace.agents.map((agent) => (agent.id === id ? { ...agent, group } : agent)),
  }
}

/** 返回可移动到的已有分组；未分组始终可用，默认猫咪的独立区域不作为分组。 */
export function getAgentGroups(workspace: Workspace): string[] {
  return [
    ...new Set([
      ...workspace.agents.filter((agent) => agent.id !== 'assistant').map((agent) => agent.group),
      '未分组',
    ]),
  ]
}

/** 生成当前工作空间可用的新分组名称，避开已有分组和未分组。 */
export function getNextGroupName(workspace: Workspace): string {
  const groups = new Set(getAgentGroups(workspace))
  if (!groups.has('新分组')) return '新分组'
  let index = 2
  while (groups.has(`新分组 ${index}`)) index += 1
  return `新分组 ${index}`
}

/** 校验分组名后为指定猫咪创建分组并移入；分组名不得与已有分组重复。 */
export function createGroupForAgent(workspace: Workspace, id: string, name: string): Workspace {
  if (!workspace.agents.some((agent) => agent.id === id)) throw new Error('猫咪不存在')
  if (id === 'assistant') throw new Error('默认猫咪不参与分组')
  const result = groupNameSchema.safeParse(name)
  if (!result.success) throw new Error(result.error.issues[0]!.message)
  if (
    getAgentGroups(workspace).some(
      (group) => group.toLocaleLowerCase() === result.data.toLocaleLowerCase(),
    )
  ) {
    throw new Error('这个分组名称已存在，请换一个名称')
  }
  return {
    ...workspace,
    agents: workspace.agents.map((agent) =>
      agent.id === id ? { ...agent, group: result.data } : agent,
    ),
  }
}

/** 重命名整个分组并同步组内助手；未分组、重复名称或非法名称不生效。 */
export function renameAgentGroup(
  workspace: Workspace,
  previousName: string,
  name: string,
): Workspace {
  const groups = getAgentGroups(workspace)
  if (!groups.includes(previousName)) throw new Error('分组不存在')
  if (previousName === '未分组') throw new Error('未分组不能重命名')
  const result = groupNameSchema.safeParse(name)
  if (!result.success) throw new Error(result.error.issues[0]!.message)
  const normalized = result.data.toLocaleLowerCase()
  if (groups.some((group) => group !== previousName && group.toLocaleLowerCase() === normalized)) {
    throw new Error('这个分组名称已存在，请换一个名称')
  }
  if (result.data === previousName) return workspace
  return {
    ...workspace,
    agents: workspace.agents.map((agent) =>
      agent.id !== 'assistant' && agent.group === previousName
        ? { ...agent, group: result.data }
        : agent,
    ),
  }
}

/** 确认后删除猫咪及其本地内容；活动会话回到默认猫咪，默认猫咪不可删除。 */
export function deleteAgent(workspace: Workspace, id: string): Workspace {
  if (id === 'assistant') throw new Error('默认猫咪不能删除')
  if (!workspace.agents.some((agent) => agent.id === id)) throw new Error('猫咪不存在')
  return {
    ...workspace,
    activeAgentId: workspace.activeAgentId === id ? 'assistant' : workspace.activeAgentId,
    agents: workspace.agents.filter((agent) => agent.id !== id),
  }
}
