import { act, fireEvent, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { createWorkspace } from '../workspace/model'
import { GroupCreateDialog } from './ConversationPanel'

describe('创建群聊', () => {
  it('校验空白名称和成员，显示头像及已选数量', async () => {
    const onCreate = vi.fn()
    const user = userEvent.setup()
    const { container } = render(
      <GroupCreateDialog
        agents={createWorkspace().agents.slice(0, 1)}
        onClose={vi.fn()}
        onCreate={onCreate}
      />,
    )
    expect(container.querySelector('.avatar')).toBeInTheDocument()
    await user.type(screen.getByRole('textbox'), '   ')
    await user.click(screen.getByRole('button', { name: '创建群聊' }))
    expect(screen.getByRole('alert')).toHaveTextContent('请输入群名称')
    await user.clear(screen.getByRole('textbox'))
    await user.type(screen.getByRole('textbox'), '  创作小队  ')
    await user.click(screen.getByRole('button', { name: '创建群聊' }))
    expect(screen.getByRole('alert')).toHaveTextContent('至少选择一只')
    expect(onCreate).not.toHaveBeenCalled()
    await user.click(screen.getByRole('checkbox', { name: '小黑' }))
    expect(screen.getByText('已选 1 位')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '创建群聊' }))
    expect(onCreate).toHaveBeenCalledWith('创作小队', ['assistant'])
  })
  it('请求期间禁止重复提交，失败保留表单并允许重试', async () => {
    let reject!: (error: Error) => void
    const onCreate = vi.fn(
      () =>
        new Promise<void>((_resolve, fail) => {
          reject = fail
        }),
    )
    const { container } = render(
      <GroupCreateDialog
        agents={createWorkspace().agents.slice(0, 1)}
        onClose={vi.fn()}
        onCreate={onCreate}
      />,
    )
    fireEvent.change(screen.getByRole('textbox'), { target: { value: '讨论组' } })
    fireEvent.click(screen.getByRole('checkbox'))
    fireEvent.submit(container.querySelector('form')!)
    fireEvent.submit(container.querySelector('form')!)
    expect(onCreate).toHaveBeenCalledTimes(1)
    expect(screen.getByRole('button', { name: '正在创建…' })).toBeDisabled()
    await act(async () => reject(new Error('无法连接服务')))
    expect(screen.getByRole('alert')).toHaveTextContent('无法连接服务')
    expect(screen.getByRole('textbox')).toHaveValue('讨论组')
    expect(screen.getByRole('checkbox')).toBeChecked()
    expect(screen.getByRole('button', { name: '创建群聊' })).toBeEnabled()
  })
  it('没有猫咪时提供说明并禁止创建', () => {
    render(<GroupCreateDialog agents={[]} onClose={vi.fn()} onCreate={vi.fn()} />)
    expect(screen.getByText(/还没有猫咪/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '创建群聊' })).toBeDisabled()
  })
})
