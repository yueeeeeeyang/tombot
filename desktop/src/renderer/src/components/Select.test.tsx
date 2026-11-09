import { useState } from 'react'
import { fireEvent, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { Select } from './Select'

const options = [
  { value: 'system', label: '跟随系统' },
  { value: 'unavailable', label: '不可用', disabled: true },
  { value: 'light', label: '浅色' },
  { value: 'dark', label: '深色' },
]

function Example({ onSubmit = vi.fn() }) {
  const [value, setValue] = useState('system')
  return (
    <form
      onSubmit={(event) => {
        event.preventDefault()
        onSubmit()
      }}
    >
      <Select label="主题" value={value} onChange={setValue} options={options} />
      <button type="button">下一个控件</button>
    </form>
  )
}

describe('自定义选择器', () => {
  it('点击选项提交一次，更新显示并恢复焦点；禁用项不能选择', async () => {
    const user = userEvent.setup()
    render(<Example />)
    const trigger = screen.getByRole('combobox', { name: '主题' })
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
    await user.click(trigger)
    expect(screen.getByRole('option', { name: '跟随系统' })).toHaveAttribute(
      'aria-selected',
      'true',
    )
    await user.click(screen.getByRole('option', { name: '不可用' }))
    expect(trigger).toHaveAttribute('aria-expanded', 'true')
    expect(trigger).toHaveTextContent('跟随系统')
    await user.click(screen.getByRole('option', { name: '深色' }))
    expect(trigger).toHaveTextContent('深色')
    expect(trigger).toHaveFocus()
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
  })

  it('键盘跳过禁用项并循环导航，首尾定位和确认不会提交外层表单', async () => {
    const user = userEvent.setup()
    const submit = vi.fn()
    render(<Example onSubmit={submit} />)
    const trigger = screen.getByRole('combobox', { name: '主题' })
    trigger.focus()
    await user.keyboard('{Enter}{ArrowDown}')
    expect(trigger).toHaveAttribute(
      'aria-activedescendant',
      screen.getByRole('option', { name: '浅色' }).id,
    )
    await user.keyboard('{End}{ArrowDown}')
    expect(trigger).toHaveAttribute(
      'aria-activedescendant',
      screen.getByRole('option', { name: '跟随系统' }).id,
    )
    await user.keyboard('{Home}{ArrowUp}{Enter}')
    expect(trigger).toHaveTextContent('深色')
    expect(submit).not.toHaveBeenCalled()
    await user.keyboard(' ')
    expect(screen.getByRole('listbox')).toBeInTheDocument()
  })

  it('Escape 取消待选值，Tab 和外部点击关闭菜单且不改值', async () => {
    const user = userEvent.setup()
    render(<Example />)
    const trigger = screen.getByRole('combobox', { name: '主题' })
    await user.click(trigger)
    await user.keyboard('{End}{Escape}')
    expect(trigger).toHaveTextContent('跟随系统')
    expect(trigger).toHaveFocus()
    await user.click(trigger)
    await user.tab()
    expect(screen.getByRole('button', { name: '下一个控件' })).toHaveFocus()
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
    await user.click(trigger)
    await user.click(document.body)
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
    expect(trigger).toHaveTextContent('跟随系统')
  })

  it('按名称查找并忽略输入法合成键', async () => {
    const user = userEvent.setup()
    const onChange = vi.fn()
    render(
      <Select
        label="模型"
        value="one"
        onChange={onChange}
        options={[
          { value: 'one', label: 'Claude' },
          { value: 'two', label: 'DeepSeek' },
        ]}
      />,
    )
    const trigger = screen.getByRole('combobox', { name: '模型' })
    await user.click(trigger)
    fireEvent.keyDown(trigger, { key: 'Enter', isComposing: true })
    expect(onChange).not.toHaveBeenCalled()
    await user.keyboard('dee{Enter}')
    expect(onChange).toHaveBeenCalledExactlyOnceWith('two')
  })

  it('空列表和整个控件禁用时不可打开；未知值显示占位而不自动修改', async () => {
    const user = userEvent.setup()
    const onChange = vi.fn()
    const view = render(<Select label="模型" value="unknown" onChange={onChange} options={[]} />)
    const trigger = screen.getByRole('combobox', { name: '模型' })
    expect(trigger).toBeDisabled()
    expect(trigger).toHaveTextContent('请选择')
    view.rerender(
      <Select label="模型" value="unknown" onChange={onChange} options={options} disabled />,
    )
    await user.click(trigger)
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
    expect(onChange).not.toHaveBeenCalled()
  })
})
