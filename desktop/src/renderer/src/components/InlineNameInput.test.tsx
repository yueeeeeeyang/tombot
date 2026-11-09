import { fireEvent, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { InlineNameInput } from './InlineNameInput'

describe('原位名称输入', () => {
  it('自动聚焦并全选原文，Enter 提交当前输入', async () => {
    const user = userEvent.setup()
    const onCommit = vi.fn(() => true)
    const onCancel = vi.fn()
    render(
      <InlineNameInput
        initialName="旧名称"
        label="猫咪名称"
        onCommit={onCommit}
        onCancel={onCancel}
      />,
    )
    const input = screen.getByRole('textbox', { name: '猫咪名称' }) as HTMLInputElement
    expect(input).toHaveFocus()
    expect(input).toHaveValue('旧名称')
    expect(input.selectionStart).toBe(0)
    expect(input.selectionEnd).toBe(3)
    await user.clear(input)
    await user.type(input, '新名称{Enter}')
    expect(onCommit).toHaveBeenCalledWith('新名称')
    expect(onCancel).not.toHaveBeenCalled()
  })

  it('提交失败保留输入并恢复焦点，输入法确认不提交，Escape 取消', async () => {
    const user = userEvent.setup()
    const onCommit = vi.fn(() => false)
    const onCancel = vi.fn()
    render(
      <InlineNameInput
        initialName="旧名称"
        label="分组名称"
        onCommit={onCommit}
        onCancel={onCancel}
      />,
    )
    const input = screen.getByRole('textbox', { name: '分组名称' })
    await user.type(input, '{Enter}')
    expect(onCommit).toHaveBeenCalledWith('旧名称')
    expect(input).toHaveFocus()
    fireEvent.keyDown(input, { key: 'Enter', isComposing: true })
    expect(onCommit).toHaveBeenCalledTimes(1)
    fireEvent.change(input, { target: { value: '新名称' } })
    fireEvent.blur(input)
    expect(onCommit).toHaveBeenLastCalledWith('新名称')
    expect(onCommit).toHaveBeenCalledTimes(2)
    fireEvent.keyDown(input, { key: 'Escape' })
    expect(onCancel).toHaveBeenCalledTimes(1)
  })
})
