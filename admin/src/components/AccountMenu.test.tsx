import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { AccountMenu } from './AccountMenu'
import { session, sessionSchema } from '../../tests/fixtures'

describe('侧栏账户菜单', () => {
  it('显示真实资料，支持键盘选择主题、关闭与退出', async () => {
    const logout = vi.fn()
    const user = userEvent.setup()
    render(
      <AccountMenu
        principal={sessionSchema.parse(session()).principal}
        busy={false}
        onLogout={logout}
      />,
    )
    const trigger = screen.getByRole('button', { name: '账户菜单：测试管理员（test-admin）' })
    expect(screen.getByText('@test-admin')).toBeVisible()
    expect(screen.getByRole('img', { name: '当前用户头像' })).toBeVisible()
    expect(screen.queryByRole('menuitem')).not.toBeInTheDocument()
    trigger.focus()
    await user.keyboard('{ArrowDown}')
    expect(screen.getByRole('menuitemradio', { name: '跟随系统' })).toHaveFocus()
    await user.keyboard('{ArrowDown}{ArrowDown}{Enter}')
    expect(localStorage.getItem('tombot-admin-theme')).toBe('dark')
    expect(document.documentElement.dataset.theme).toBe('dark')
    expect(trigger).toHaveFocus()
    expect(screen.queryByRole('menu')).not.toBeInTheDocument()
    await user.click(trigger)
    expect(screen.getByRole('menuitemradio', { name: '深色' })).toHaveAttribute(
      'aria-checked',
      'true',
    )
    await user.keyboard('{Escape}')
    expect(trigger).toHaveFocus()
    await user.click(trigger)
    await user.click(document.body)
    expect(screen.queryByRole('menu')).not.toBeInTheDocument()
    trigger.focus()
    await user.keyboard('{ArrowUp}')
    expect(screen.getByRole('menuitem', { name: '退出登录' })).toHaveFocus()
    await user.keyboard('{Enter}')
    expect(logout).toHaveBeenCalledTimes(1)
  })
  it('主题保存失败明确提示，操作中禁止重复打开账户菜单', async () => {
    const save = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('测试存储不可用')
    })
    try {
      const user = userEvent.setup()
      const rendered = render(
        <AccountMenu
          principal={sessionSchema.parse(session()).principal}
          busy={false}
          onLogout={vi.fn()}
        />,
      )
      await user.click(screen.getByRole('button', { name: /^账户菜单/ }))
      await user.click(screen.getByRole('menuitemradio', { name: '浅色' }))
      expect(screen.getByRole('status')).toHaveTextContent('外观仅在本次页面生效')
      rendered.rerender(
        <AccountMenu
          principal={sessionSchema.parse(session()).principal}
          busy={true}
          onLogout={vi.fn()}
        />,
      )
      expect(screen.getByRole('button', { name: /^账户菜单/ })).toBeDisabled()
    } finally {
      save.mockRestore()
    }
  })
})
