import { act, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { activationInputs, failure, id, json, makeClient } from '../../tests/fixtures'
import { UsersPage } from './UsersPage'

function fixture(reject = false) {
  const input = activationInputs()
  const client = makeClient((path) => {
    if (path.endsWith('/activation'))
      return reject
        ? failure('ACTIVATION_UNAVAILABLE', '当前用户不能签发激活材料')
        : json({
            user_id: id(4),
            activation_token: input.token,
            expires_at: new Date(Date.now() + 3600_000).toISOString(),
          })
    return json({
      users: [
        {
          id: id(4),
          username: 'new-user',
          display_name: '新用户',
          role: 'user',
          status: 'pending_activation',
          computer_status: 'ready',
          revision: 1,
          creation_operation_id: id(5),
          created_at: new Date().toISOString(),
        },
      ],
    })
  })
  return { ...client, input }
}

describe('管理员签发用户激活材料', () => {
  it('同时展示激活码和独立链接，复制仅包含公开地址，关闭后清除材料', async () => {
    const setup = fixture()
    const user = userEvent.setup()
    const copy = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue()
    render(<UsersPage client={setup.client} />)
    await user.click(await screen.findByRole('button', { name: '签发激活材料' }))
    expect(screen.queryByLabelText('激活页面链接')).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '签发新的激活材料' }))
    const url = window.location.origin + '/activate'
    expect(await screen.findByLabelText('一次性激活码')).toHaveValue(setup.input.token)
    expect(screen.getByLabelText('激活页面链接')).toHaveValue(url)
    expect(screen.getByRole('link', { name: '打开激活页面' })).toHaveAttribute('href', url)
    expect(screen.getByRole('link', { name: '打开激活页面' })).toHaveAttribute(
      'rel',
      'noopener noreferrer',
    )
    await user.click(screen.getByRole('button', { name: '复制链接' }))
    expect(copy).toHaveBeenCalledWith(url)
    expect(await screen.findByText('激活页面链接已复制')).toBeVisible()
    expect(localStorage.length).toBe(0)
    expect(sessionStorage.length).toBe(0)
    await user.click(screen.getByRole('button', { name: '已保存，关闭展示' }))
    await user.click(screen.getByRole('button', { name: '签发激活材料' }))
    expect(screen.queryByLabelText('一次性激活码')).not.toBeInTheDocument()
    expect(setup.calls.filter(({ path }) => path.endsWith('/activation'))).toHaveLength(1)
  })

  it('复制失败提供手动复制入口，离开页面清除激活材料', async () => {
    const setup = fixture()
    const user = userEvent.setup()
    vi.spyOn(navigator.clipboard, 'writeText').mockRejectedValue(
      new DOMException('剪贴板不可用', 'NotAllowedError'),
    )
    render(<UsersPage client={setup.client} />)
    await user.click(await screen.findByRole('button', { name: '签发激活材料' }))
    await user.click(screen.getByRole('button', { name: '签发新的激活材料' }))
    await user.click(await screen.findByRole('button', { name: '复制链接' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(
      '复制失败，请选中激活页面链接手动复制',
    )
    expect(screen.getByLabelText('激活页面链接')).toHaveValue(window.location.origin + '/activate')
    act(() => window.dispatchEvent(new Event('pagehide')))
    expect(screen.queryByLabelText('一次性激活码')).not.toBeInTheDocument()
    expect(screen.queryByRole('link', { name: '打开激活页面' })).not.toBeInTheDocument()
  })

  it('签发失败不展示激活材料或成功入口', async () => {
    const setup = fixture(true)
    render(<UsersPage client={setup.client} />)
    await userEvent.click(await screen.findByRole('button', { name: '签发激活材料' }))
    await userEvent.click(screen.getByRole('button', { name: '签发新的激活材料' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('当前用户不能签发激活材料')
    expect(screen.queryByLabelText('一次性激活码')).not.toBeInTheDocument()
    expect(screen.queryByLabelText('激活页面链接')).not.toBeInTheDocument()
  })
})
