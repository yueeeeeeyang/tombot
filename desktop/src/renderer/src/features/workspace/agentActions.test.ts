import { describe, expect, it } from 'vitest'
import { createWorkspace } from './model'
import {
  createGroupForAgent,
  getAgentGroups,
  getNextGroupName,
  moveAgent,
  renameAgentGroup,
} from './agentActions'

describe('分组领域规则', () => {
  it('为新分组生成不冲突的默认名称', () => {
    const original = createWorkspace()
    expect(getNextGroupName(original)).toBe('新分组')
    const first = createGroupForAgent(original, 'developer', '新分组')
    expect(getNextGroupName(first)).toBe('新分组 2')
    const second = createGroupForAgent(first, 'artist', '新分组 2')
    expect(getNextGroupName(second)).toBe('新分组 3')
  })

  it('创建分组时移入指定猫咪，不修改其他会话', () => {
    const original = createWorkspace()
    const created = createGroupForAgent(original, 'developer', '  程序组  ')
    expect(created.agents.find((agent) => agent.id === 'developer')?.group).toBe('程序组')
    expect(created.agents.find((agent) => agent.id === 'artist')?.group).toBe('游戏开发')
    expect(original.agents.find((agent) => agent.id === 'developer')?.group).toBe('游戏开发')
    expect(getAgentGroups(created)).toContain('程序组')
  })

  it('重命名分组同步组内所有猫咪，并保留其他分组', () => {
    const first = createGroupForAgent(createWorkspace(), 'developer', '新分组')
    const grouped = moveAgent(first, 'artist', '新分组')
    const renamed = renameAgentGroup(grouped, '新分组', '程序组')
    expect(
      renamed.agents
        .filter((agent) => ['developer', 'artist'].includes(agent.id))
        .every((agent) => agent.group === '程序组'),
    ).toBe(true)
    expect(renamed.agents.find((agent) => agent.id === 'audio')?.group).toBe('游戏开发')
    expect(getAgentGroups(renamed)).toContain('程序组')
    expect(getAgentGroups(renamed)).not.toContain('新分组')
  })

  it('拒绝默认猫咪、未知猫咪、重复或非法名称', () => {
    const workspace = createWorkspace()
    expect(() => createGroupForAgent(workspace, 'assistant', '新分组')).toThrow('默认猫咪')
    expect(() => createGroupForAgent(workspace, 'missing', '新分组')).toThrow('猫咪不存在')
    expect(() => createGroupForAgent(workspace, 'developer', '游戏开发')).toThrow('已存在')
    expect(() => createGroupForAgent(workspace, 'developer', '   ')).toThrow('请输入分组名称')
    expect(() => createGroupForAgent(workspace, 'developer', '组'.repeat(25))).toThrow('最多')
  })

  it('重命名时拒绝重复名称、未分组和非法名称', () => {
    const workspace = createGroupForAgent(createWorkspace(), 'developer', '新分组')
    expect(() => renameAgentGroup(workspace, '缺失分组', '其他')).toThrow('分组不存在')
    expect(() => renameAgentGroup(workspace, '未分组', '其他')).toThrow('不能重命名')
    expect(() => renameAgentGroup(workspace, '新分组', '游戏开发')).toThrow('已存在')
    expect(() => renameAgentGroup(workspace, '新分组', '未分组')).toThrow('已存在')
    expect(() => renameAgentGroup(workspace, '新分组', '   ')).toThrow('请输入分组名称')
    expect(() => renameAgentGroup(workspace, '新分组', '组'.repeat(25))).toThrow('最多')
  })
})
