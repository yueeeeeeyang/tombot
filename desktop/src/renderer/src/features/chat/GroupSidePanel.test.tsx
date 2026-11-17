import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { useState } from 'react'
import { describe, expect, it, vi } from 'vitest'
import { getBackend, type GroupDetail } from '../desktop/backend'
import { createWorkspace } from '../workspace/model'
import { GroupSidePanel } from './GroupSidePanel'

const backend = vi.hoisted(() => ({
  groupDetail: vi.fn(),
  renameGroup: vi.fn(),
  addGroupMembers: vi.fn(),
  removeGroupMember: vi.fn(),
}))
vi.mock('../desktop/backend', () => ({ getBackend: () => backend }))

const agents = createWorkspace().agents.slice(0, 3)
const member = (index: number) => ({ bot_id: agents[index]!.id, name: agents[index]!.name })
const initial: GroupDetail = {
  avatar_presets: ['bombay', 'orange-tabby'],
  id: 'group',
  name: '创作组',
  revision: 1,
  conversation_id: 'conversation',
  members: [member(0), member(1)],
}

function Panel() {
  const [group, setGroup] = useState(initial)
  return <GroupSidePanel group={group} agents={agents} onClose={vi.fn()} onChange={setGroup} />
}

describe('群资料右栏', () => {
  it('群主使用当前用户头像，资料刷新后更新，未设置时显示默认头像', () => {
    const props = { group: initial, agents, onClose: vi.fn(), onChange: vi.fn() }
    const profile = {
      username: 'owner',
      displayName: '本人',
      avatar: 'data:image/png;base64,dGVzdA==',
    }
    const { rerender } = render(<GroupSidePanel {...props} userProfile={profile} />)
    expect(screen.getByRole('img', { name: '你的头像' })).toHaveAttribute('src', profile.avatar)
    expect(screen.getByText('群主')).toHaveClass('group-member-role')
    expect(screen.getByText('你')).toHaveClass('group-member-name')
    rerender(<GroupSidePanel {...props} userProfile={{ ...profile, avatar: '' }} />)
    expect(screen.getByRole('img', { name: '你的头像' }).getAttribute('src')).toContain(
      'workspace-cat.png',
    )
    rerender(<GroupSidePanel {...props} />)
    expect(screen.getByRole('img', { name: '你的头像' })).toBeInTheDocument()
  })
  it('按最新版本加入和移出，最后一位不可移出', async () => {
    let stored = initial
    const add = vi.fn(async () => {
      stored = { ...stored, revision: 3, members: [...stored.members, member(2)] }
    })
    const remove = vi.fn(async (_id, _revision, botId) => {
      stored = {
        ...stored,
        revision: stored.revision + 1,
        members: stored.members.filter((item) => item.bot_id !== botId),
      }
    })
    vi.spyOn(getBackend(), 'groupDetail').mockImplementation(async () => stored)
    vi.spyOn(getBackend(), 'addGroupMembers').mockImplementation(add)
    vi.spyOn(getBackend(), 'removeGroupMember').mockImplementation(remove)
    render(<Panel />)
    expect(screen.getAllByRole('tab').map((item) => item.textContent)).toEqual(['成员', '资料库'])
    expect(screen.queryByRole('tab', { name: '详情' })).toBeNull()
    expect(screen.getByText('你')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '添加成员' }))
    fireEvent.change(screen.getByRole('textbox', { name: '搜索猫咪名称' }), {
      target: { value: agents[2]!.name },
    })
    fireEvent.click(screen.getByRole('option', { name: agents[2]!.name }))
    await waitFor(() => expect(screen.getByRole('button', { name: '添加成员' })).toBeDisabled())
    expect(add).toHaveBeenCalledWith('group', 1, [agents[2]!.id])
    fireEvent.click(screen.getByRole('button', { name: `移出${agents[2]!.name}` }))
    await waitFor(() => expect(screen.getByRole('button', { name: '添加成员' })).toBeEnabled())
    expect(remove).toHaveBeenCalledWith('group', 3, agents[2]!.id)
    fireEvent.click(screen.getByRole('button', { name: `移出${agents[1]!.name}` }))
    await waitFor(() =>
      expect(screen.getByRole('button', { name: `移出${agents[0]!.name}` })).toBeDisabled(),
    )
  })

  it('失败显示错误并刷新版本，重复点击不会重复修改', async () => {
    let reject!: (error: Error) => void
    const remove = vi.spyOn(getBackend(), 'removeGroupMember').mockImplementation(
      () =>
        new Promise((_resolve, fail) => {
          reject = fail
        }),
    )
    vi.spyOn(getBackend(), 'groupDetail').mockResolvedValue({ ...initial, revision: 7 })
    render(<Panel />)
    const button = screen.getByRole('button', { name: `移出${agents[1]!.name}` })
    fireEvent.click(button)
    fireEvent.click(button)
    expect(remove).toHaveBeenCalledTimes(1)
    expect(button).toBeDisabled()
    await act(async () => reject(new Error('群资料已更新')))
    expect(screen.getByRole('alert')).toHaveTextContent('群资料已更新')
    fireEvent.click(button)
    expect(remove).toHaveBeenLastCalledWith('group', 7, agents[1]!.id)
    await act(async () => reject(new Error('仍然无法保存')))
  })
})
