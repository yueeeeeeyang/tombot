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
  it('按最新版本重命名、加入和移出，最后一位不可移出', async () => {
    let stored = initial
    const rename = vi.fn(async (_id, _revision, name) => {
      stored = { ...stored, name, revision: 2 }
      return stored
    })
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
    vi.spyOn(getBackend(), 'renameGroup').mockImplementation(rename)
    vi.spyOn(getBackend(), 'addGroupMembers').mockImplementation(add)
    vi.spyOn(getBackend(), 'removeGroupMember').mockImplementation(remove)
    render(<Panel />)
    expect(screen.queryByRole('textbox')).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: '修改群名称' }))
    expect(screen.getByRole('button', { name: '保存名称' })).toBeDisabled()
    fireEvent.change(screen.getByRole('textbox', { name: '群名称' }), {
      target: { value: '  新名字  ' },
    })
    fireEvent.click(screen.getByRole('button', { name: '保存名称' }))
    expect(await screen.findByRole('heading', { name: '新名字' })).toBeInTheDocument()
    expect(rename).toHaveBeenCalledWith('group', 1, '新名字')
    fireEvent.click(screen.getByRole('tab', { name: '成员' }))
    fireEvent.click(screen.getByRole('button', { name: '添加成员' }))
    fireEvent.change(screen.getByRole('textbox', { name: '搜索猫咪名称' }), {
      target: { value: agents[2]!.name },
    })
    fireEvent.click(screen.getByRole('option', { name: agents[2]!.name }))
    await waitFor(() => expect(screen.getByRole('button', { name: '添加成员' })).toBeDisabled())
    expect(add).toHaveBeenCalledWith('group', 2, [agents[2]!.id])
    fireEvent.click(screen.getByRole('button', { name: `移出${agents[2]!.name}` }))
    await waitFor(() => expect(screen.getByRole('button', { name: '添加成员' })).toBeEnabled())
    expect(remove).toHaveBeenCalledWith('group', 3, agents[2]!.id)
    fireEvent.click(screen.getByRole('button', { name: `移出${agents[1]!.name}` }))
    await waitFor(() =>
      expect(screen.getByRole('button', { name: `移出${agents[0]!.name}` })).toBeDisabled(),
    )
  })

  it('失败保留名称，刷新版本后才允许手动重试，阻止重复提交', async () => {
    let reject!: (failure: Error) => void
    const rename = vi.spyOn(getBackend(), 'renameGroup').mockImplementation(
      () =>
        new Promise((_resolve, fail) => {
          reject = fail
        }),
    )
    vi.spyOn(getBackend(), 'groupDetail').mockResolvedValue({ ...initial, revision: 7 })
    const view = render(<Panel />)
    fireEvent.click(screen.getByRole('button', { name: '修改群名称' }))
    fireEvent.change(screen.getByRole('textbox'), { target: { value: '待保存' } })
    fireEvent.submit(view.container.querySelector('form')!)
    fireEvent.submit(view.container.querySelector('form')!)
    expect(rename).toHaveBeenCalledTimes(1)
    expect(screen.getByRole('textbox')).toBeDisabled()
    await act(async () => reject(new Error('群资料已被更新')))
    expect(screen.getByRole('alert')).toHaveTextContent('群资料已被更新')
    expect(screen.getByRole('textbox')).toHaveValue('待保存')
    fireEvent.submit(view.container.querySelector('form')!)
    expect(rename).toHaveBeenLastCalledWith('group', 7, '待保存')
    await act(async () => reject(new Error('仍然无法保存')))
  })
})
