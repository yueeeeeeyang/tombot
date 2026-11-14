import { fireEvent, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { Field } from '../components'
import { Select } from './Select'
import { chooseOption } from '../../tests/select'

const options = [
  { value: 'one', label: '选项甲' },
  { value: 'disabled', label: '已停用', disabled: true },
  { value: 'two', label: '选项乙' },
]

describe('管理表单自定义下拉', () => {
  it('必填项阻止空表单提交，选择后按名称提交真实值', async () => {
    const submit = vi.fn()
    const user = userEvent.setup()
    render(
      <form
        onSubmit={(event) => {
          event.preventDefault()
          submit(Object.fromEntries(new FormData(event.currentTarget)))
        }}
      >
        <fieldset>
          <Field label="节点" hint="请选择可用节点">
            <Select name="node_id" label="节点" required options={options} />
          </Field>
        </fieldset>
        <button type="submit">保存</button>
      </form>,
    )
    const control = screen.getByRole('combobox', { name: '节点' })
    await user.click(screen.getByRole('button', { name: '保存' }))
    expect(submit).not.toHaveBeenCalled()
    expect(control).toHaveFocus()
    expect(control).toHaveAttribute('aria-invalid', 'true')
    expect(control).toHaveAccessibleDescription('请选择可用节点 请选择节点')
    await chooseOption(user, '节点', '选项乙')
    expect(control).not.toHaveAttribute('aria-invalid')
    await user.click(screen.getByRole('button', { name: '保存' }))
    expect(submit).toHaveBeenCalledExactlyOnceWith({ node_id: 'two' })
  })

  it('默认值、禁用选项及动态换页后的无效值不会静默选中其他项', async () => {
    const user = userEvent.setup()
    const view = render(
      <form>
        <Select name="model" label="模型" required defaultValue="one" options={options} />
      </form>,
    )
    const form = view.container.querySelector('form')!
    expect(new FormData(form).get('model')).toBe('one')
    await user.click(screen.getByRole('combobox'))
    await user.click(screen.getByRole('option', { name: '已停用' }))
    expect(new FormData(form).get('model')).toBe('one')
    view.rerender(
      <form>
        <Select name="model" label="模型" required defaultValue="one" options={[options[2]!]} />
      </form>,
    )
    expect(form.checkValidity()).toBe(false)
    expect(new FormData(form).get('model')).toBe('')
    expect(screen.getByRole('combobox')).toHaveTextContent('请选择')
  })

  it('禁用字段不进入 FormData，空值作为可选默认项有效', () => {
    const view = render(
      <form>
        <fieldset disabled>
          <Select name="locked" label="锁定" defaultValue="one" options={options} required />
        </fieldset>
        <Select name="effort" label="思考级别" options={[{ value: '', label: '供应商默认' }]} />
      </form>,
    )
    const form = view.container.querySelector('form')!
    expect(form.checkValidity()).toBe(true)
    expect(Object.fromEntries(new FormData(form))).toEqual({ effort: '' })
    fireEvent.click(screen.getByRole('combobox', { name: '锁定' }))
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
  })
})
