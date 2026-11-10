import { fireEvent, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { describe, expect, it, vi } from 'vitest'
import { createWorkspace } from '../workspace/model'
import { GroupComposer } from './GroupComposer'

const members = createWorkspace().agents.slice(0, 2)
function Composer({
  onChange,
  onSend,
}: {
  onChange: (value: string, ids: string[]) => void
  onSend: () => void
}) {
  const [draft, setDraft] = useState('')
  return (
    <GroupComposer
      title="讨论组"
      draft={draft}
      members={members}
      onChange={(value, ids) => {
        setDraft(value)
        onChange(value, ids)
      }}
      onSend={onSend}
    />
  )
}
describe('群消息提及', () => {
  it('搜索并用键盘选择，携带标识，删除提及后清理收件人', async () => {
    const change = vi.fn(),
      send = vi.fn(),
      user = userEvent.setup()
    render(<Composer onChange={change} onSend={send} />)
    await user.type(screen.getByRole('textbox'), '@小')
    expect(screen.getByRole('listbox', { name: '提及成员' })).toBeInTheDocument()
    await user.keyboard('{Enter}')
    expect(change).toHaveBeenLastCalledWith('@小黑 ', ['assistant'])
    expect(send).not.toHaveBeenCalled()
    await user.type(screen.getByRole('textbox'), '看看这个')
    await user.keyboard('{Enter}')
    expect(send).toHaveBeenCalledTimes(1)
    fireEvent.change(screen.getByRole('textbox'), { target: { value: '公开消息' } })
    expect(change).toHaveBeenLastCalledWith('公开消息', [])
  })
  it('不存在的名称显示空结果，Esc 收起，中文输入确认不发送', () => {
    const send = vi.fn()
    render(<Composer onChange={vi.fn()} onSend={send} />)
    fireEvent.change(screen.getByRole('textbox'), { target: { value: '@不存在' } })
    expect(screen.getByText('没有找到群成员')).toBeInTheDocument()
    fireEvent.keyDown(screen.getByRole('textbox'), { key: 'Escape' })
    expect(screen.queryByRole('listbox')).toBeNull()
    fireEvent.keyDown(screen.getByRole('textbox'), { key: 'Enter', isComposing: true })
    expect(send).not.toHaveBeenCalled()
  })
  it('中文正文内可以提及成员，邮箱地址不打开菜单', async () => {
    const change = vi.fn()
    const user = userEvent.setup()
    render(<Composer onChange={change} onSend={vi.fn()} />)
    await user.type(screen.getByRole('textbox'), '请@小黑')
    await user.keyboard('{Enter}')
    expect(change).toHaveBeenLastCalledWith('请@小黑 ', ['assistant'])
    await user.clear(screen.getByRole('textbox'))
    await user.type(screen.getByRole('textbox'), 'mail@example.test')
    expect(screen.queryByRole('listbox')).toBeNull()
    expect(change).toHaveBeenLastCalledWith('mail@example.test', [])
  })
})
