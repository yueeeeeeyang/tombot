import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { ComputerPane } from './ComputerPane'

describe('详情中的电脑卡片', () => {
  it('默认收起，键盘展开后可准备和连接当前猫咪的电脑', async () => {
    const user = userEvent.setup()
    const onPrepare = vi.fn()
    const onConnect = vi.fn()
    render(
      <ComputerPane
        agentName="小黑"
        computerUrl=""
        onSave={vi.fn()}
        onPrepare={onPrepare}
        onConnect={onConnect}
      />,
    )
    const card = screen.getByRole('button', { name: '小黑的电脑' })
    expect(card).toHaveAttribute('aria-expanded', 'false')
    expect(screen.getByRole('status')).toHaveTextContent('未连接')
    expect(screen.queryByRole('textbox', { name: '远端电脑地址' })).not.toBeInTheDocument()
    card.focus()
    await user.keyboard('{Enter}')
    expect(card).toHaveAttribute('aria-expanded', 'true')
    await user.click(screen.getByRole('button', { name: '开始准备' }))
    await user.click(screen.getByRole('button', { name: '连接电脑' }))
    expect(onPrepare).toHaveBeenCalledOnce()
    expect(onConnect).toHaveBeenCalledOnce()
  })

  it('拒绝不安全地址，合法地址才交给保存回调', async () => {
    const user = userEvent.setup()
    const save = vi.fn()
    render(<ComputerPane agentName="小黑" computerUrl="" onSave={save} />)
    await user.click(screen.getByRole('button', { name: '小黑的电脑' }))
    const input = screen.getByRole('textbox', { name: '远端电脑地址' })
    await user.type(input, 'javascript:alert(1)')
    await user.click(screen.getByRole('button', { name: '保存地址' }))
    expect(screen.getByRole('alert')).toHaveTextContent('HTTP 或 HTTPS')
    expect(save).not.toHaveBeenCalled()
    await user.clear(input)
    await user.type(input, 'https://desktop.example.test/')
    await user.click(screen.getByRole('button', { name: '保存地址' }))
    expect(save).toHaveBeenCalledWith('https://desktop.example.test/')
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('连接画面保持沙箱约束，可展开、收起和断开', async () => {
    const user = userEvent.setup()
    const save = vi.fn()
    render(
      <ComputerPane agentName="小黄" computerUrl="https://desktop.example.test/" onSave={save} />,
    )
    expect(screen.getByRole('status')).toHaveTextContent('已连接')
    const frame = screen.getByTitle('小黄的远端电脑')
    expect(frame).toHaveAttribute('sandbox', 'allow-scripts allow-forms')
    expect(frame).toHaveAttribute('referrerpolicy', 'no-referrer')
    await user.click(screen.getByRole('button', { name: '全屏查看' }))
    expect(frame.parentElement?.parentElement).toHaveClass('is-expanded')
    await user.click(screen.getByRole('button', { name: '小黄的电脑' }))
    expect(screen.queryByTitle('小黄的远端电脑')).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '小黄的电脑' }))
    await user.click(screen.getByRole('button', { name: '断开' }))
    expect(save).toHaveBeenCalledWith('')
  })
})
