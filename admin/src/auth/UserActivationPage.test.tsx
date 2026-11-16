import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { AdminClient } from '../api/client'
import { activationInputs, empty, failure } from '../../tests/fixtures'
import { UserActivationPage } from './UserActivationPage'

function fill(input: { token: string; password: string } = activationInputs()) {
  fireEvent.change(screen.getByLabelText('激活码'), { target: { value: input.token } })
  fireEvent.change(screen.getByLabelText('设置密码'), { target: { value: input.password } })
  fireEvent.change(screen.getByLabelText('确认密码'), { target: { value: input.password } })
  return input
}

describe('普通用户公开激活页面', () => {
  it('无需管理会话，提交完成才显示成功，材料不写入地址或存储', async () => {
    window.history.replaceState(null, '', '/activate')
    const fetcher = vi.fn(async () => empty())
    render(<UserActivationPage client={new AdminClient(fetcher)} />)
    expect(fetcher).not.toHaveBeenCalled()
    expect(document.title).toBe('激活账户 · TomBot')
    expect(screen.getByRole('link', { name: '返回用户登录' })).toHaveAttribute('href', '/login')
    const input = fill()
    await userEvent.click(screen.getByRole('button', { name: '激活账户' }))
    const heading = await screen.findByRole('heading', { name: '账户已激活' })
    expect(heading).toHaveFocus()
    expect(screen.getByRole('status')).toHaveTextContent('登录用户平台')
    expect(screen.getByRole('link', { name: '登录用户平台' })).toHaveAttribute('href', '/login')
    expect(screen.queryByLabelText('激活码')).not.toBeInTheDocument()
    expect(fetcher).toHaveBeenCalledTimes(1)
    const [path, options] = fetcher.mock.calls[0] as unknown as [string, RequestInit]
    expect(path).toBe('/api/v2/auth/activate')
    expect(options.credentials).toBe('omit')
    expect(JSON.parse(String(options.body))).toEqual({
      activation_token: input.token,
      password: input.password,
    })
    expect(window.location.pathname + window.location.search + window.location.hash).toBe(
      '/activate',
    )
    expect(localStorage.length).toBe(0)
    expect(sessionStorage.length).toBe(0)
  })

  it.each(['short', 'mismatch', 'bytes', 'whitespace'])(
    '输入边界 %s 不发送请求并保留表单供修改',
    async (kind) => {
      const fetcher = vi.fn(async () => empty())
      render(<UserActivationPage client={new AdminClient(fetcher)} />)
      const input = fill()
      if (kind === 'whitespace')
        fireEvent.change(screen.getByLabelText('激活码'), { target: { value: '   ' } })
      else if (kind === 'mismatch')
        fireEvent.change(screen.getByLabelText('确认密码'), {
          target: { value: crypto.randomUUID() },
        })
      else
        fireEvent.change(screen.getByLabelText('设置密码'), {
          target: { value: kind === 'short' ? '短密码' : '字'.repeat(342) },
        })
      await userEvent.click(screen.getByRole('button', { name: '激活账户' }))
      expect(await screen.findByRole('alert')).toBeVisible()
      expect(fetcher).not.toHaveBeenCalled()
      expect(screen.getByLabelText('确认密码')).not.toHaveValue('')
      if (kind !== 'whitespace') expect(screen.getByLabelText('激活码')).toHaveValue(input.token)
    },
  )

  it('支持十二个中文字符，粘贴激活码时移除首尾空白', async () => {
    const fetcher = vi.fn(async () => empty())
    render(<UserActivationPage client={new AdminClient(fetcher)} />)
    const input = activationInputs()
    fill({ ...input, token: ` ${input.token} `, password: '密'.repeat(12) })
    await userEvent.click(screen.getByRole('button', { name: '激活账户' }))
    await screen.findByRole('heading', { name: '账户已激活' })
    const [, options] = fetcher.mock.calls[0] as unknown as [string, RequestInit]
    expect(JSON.parse(String(options.body)).activation_token).toBe(input.token)
  })

  it('无效、过期或已使用的激活码给出重新签发指引，不报告成功', async () => {
    const fetcher = vi.fn(async () => failure('ACTIVATION_INVALID', '激活信息无效或已过期'))
    render(<UserActivationPage client={new AdminClient(fetcher)} />)
    fill()
    await userEvent.click(screen.getByRole('button', { name: '激活账户' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('激活码无效、已过期或已使用')
    expect(screen.getByRole('alert')).toHaveTextContent('联系管理员重新签发')
    expect(screen.queryByRole('heading', { name: '账户已激活' })).not.toBeInTheDocument()
    expect(screen.getByLabelText('激活码')).toHaveValue('')
    expect(screen.getByLabelText('设置密码')).toHaveValue('')
    expect(screen.getByRole('button', { name: '激活账户' })).toBeEnabled()
    expect(fetcher).toHaveBeenCalledTimes(1)
  })

  it('网络结果不明不自动重发，提示先尝试用户平台登录', async () => {
    const fetcher = vi.fn(async () => {
      throw new TypeError('网络不可达')
    })
    render(<UserActivationPage client={new AdminClient(fetcher)} />)
    fill()
    await userEvent.click(screen.getByRole('button', { name: '激活账户' }))
    await screen.findByRole('heading', { name: '激活结果待确认' })
    expect(screen.getByRole('status')).toHaveTextContent('使用用户名和刚设置的密码尝试登录')
    expect(screen.queryByRole('button', { name: '激活账户' })).not.toBeInTheDocument()
    expect(fetcher).toHaveBeenCalledTimes(1)
  })

  it('请求在途阻止重复提交，离开页面清空表单并取消等待', async () => {
    let signal: AbortSignal | null | undefined
    const fetcher = vi.fn(async (_path: RequestInfo | URL, options?: RequestInit) => {
      signal = options?.signal
      return new Promise<Response>((_resolve, reject) =>
        signal?.addEventListener('abort', () => reject(new DOMException('已取消', 'AbortError'))),
      )
    })
    render(<UserActivationPage client={new AdminClient(fetcher)} />)
    fill()
    const form = screen.getByRole('button', { name: '激活账户' }).closest('form')!
    fireEvent.submit(form)
    fireEvent.submit(form)
    expect(fetcher).toHaveBeenCalledTimes(1)
    expect(screen.getByRole('button', { name: '正在激活…' })).toBeDisabled()
    expect(screen.getByLabelText('设置密码')).toHaveValue('')
    act(() => window.dispatchEvent(new Event('pagehide')))
    await screen.findByRole('heading', { name: '激活结果待确认' })
    expect(signal?.aborted).toBe(true)
    expect(screen.queryByLabelText('激活码')).not.toBeInTheDocument()
  })

  it('关闭页面取消在途请求，地址中的参数不会自动填入或提交', async () => {
    window.history.replaceState(null, '', '/activate?activation_token=ignored#password=ignored')
    let signal: AbortSignal | null | undefined
    const fetcher = vi.fn(async (_path: RequestInfo | URL, options?: RequestInit) => {
      signal = options?.signal
      return new Promise<Response>((_resolve, reject) =>
        signal?.addEventListener('abort', () => reject(new DOMException('已取消', 'AbortError'))),
      )
    })
    const { unmount } = render(<UserActivationPage client={new AdminClient(fetcher)} />)
    expect(screen.getByLabelText('激活码')).toHaveValue('')
    expect(fetcher).not.toHaveBeenCalled()
    fill()
    fireEvent.submit(screen.getByRole('button', { name: '激活账户' }).closest('form')!)
    unmount()
    await waitFor(() => expect(signal?.aborted).toBe(true))
    expect(fetcher).toHaveBeenCalledTimes(1)
  })
})
