import { act, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { NodesPage } from './NodesPage'
import { failure, id, json, makeClient } from '../../tests/fixtures'

const expiry = '2026-11-14T00:00:00Z'

describe('节点自动接入', () => {
  it('显示一次性凭据码，复制精确内容，关闭后不能再次查看', async () => {
    const token = 'tomnode1.' + Array.from({ length: 40 }, () => crypto.randomUUID()).join('')
    let writes = 0
    const fixture = makeClient((path) => {
      if (path.endsWith('/enrollment-tokens')) {
        writes++
        return json({ id: id(5), token, expires_at: expiry }, 201)
      }
      return json({ nodes: [] })
    })
    const user = userEvent.setup()
    const copy = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue()
    render(<NodesPage client={fixture.client} />)
    await user.click(screen.getByRole('button', { name: '生成接入凭据' }))
    expect(screen.queryByLabelText('镜像标识')).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '生成凭据' }))
    const code = await screen.findByRole('textbox', { name: '接入凭据码' })
    expect(code).toHaveValue(token)
    expect(code).toHaveAttribute('readonly')
    expect(screen.queryByRole('link', { name: /下载/ })).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '复制凭据码' }))
    expect(copy).toHaveBeenCalledExactlyOnceWith(token)
    expect(await screen.findByRole('button', { name: '已复制' })).toBeEnabled()
    expect(writes).toBe(1)
    expect(localStorage.length).toBe(0)
    expect(sessionStorage.length).toBe(0)
    await user.click(screen.getByRole('button', { name: /^关闭$/ }))
    await user.click(screen.getByRole('button', { name: '生成接入凭据' }))
    expect(screen.queryByRole('textbox', { name: '接入凭据码' })).not.toBeInTheDocument()
    expect(writes).toBe(1)
  })

  it('复制失败保留凭据供手动复制，并允许再次复制', async () => {
    const token = crypto.randomUUID()
    const fixture = makeClient((path) =>
      path.endsWith('/enrollment-tokens')
        ? json({ id: id(5), token, expires_at: expiry })
        : json({ nodes: [] }),
    )
    const user = userEvent.setup()
    const copy = vi
      .spyOn(navigator.clipboard, 'writeText')
      .mockRejectedValueOnce(new DOMException('禁止复制', 'NotAllowedError'))
      .mockResolvedValue()
    render(<NodesPage client={fixture.client} />)
    await user.click(screen.getByRole('button', { name: '生成接入凭据' }))
    await user.click(screen.getByRole('button', { name: '生成凭据' }))
    await user.click(await screen.findByRole('button', { name: '复制凭据码' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('复制失败，请选中凭据码手动复制。')
    expect(screen.getByRole('textbox', { name: '接入凭据码' })).toHaveValue(token)
    await user.click(screen.getByRole('button', { name: '复制凭据码' }))
    expect(copy).toHaveBeenCalledTimes(2)
    expect(await screen.findByRole('button', { name: '已复制' })).toBeEnabled()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    act(() => window.dispatchEvent(new Event('pagehide')))
    expect(screen.queryByRole('textbox', { name: '接入凭据码' })).not.toBeInTheDocument()
  })

  it('签发失败显示错误并可重试，不显示凭据', async () => {
    const fixture = makeClient((path) =>
      path.endsWith('/enrollment-tokens')
        ? failure('NODE_CATALOG_UNAVAILABLE', '接入服务暂不可用', 503)
        : json({ nodes: [] }),
    )
    const user = userEvent.setup()
    render(<NodesPage client={fixture.client} />)
    await user.click(screen.getByRole('button', { name: '生成接入凭据' }))
    await user.click(screen.getByRole('button', { name: '生成凭据' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('接入服务暂不可用')
    expect(screen.queryByRole('textbox', { name: '接入凭据码' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '生成凭据' })).toBeEnabled()
  })

  it('离开页面取消在途签发，不恢复迟到响应中的凭据', async () => {
    let finish!: (value: Response) => void
    const pending = new Promise<Response>((resolve) => {
      finish = resolve
    })
    const fixture = makeClient((path) =>
      path.endsWith('/enrollment-tokens') ? pending : json({ nodes: [] }),
    )
    const user = userEvent.setup()
    const view = render(<NodesPage client={fixture.client} />)
    await user.click(screen.getByRole('button', { name: '生成接入凭据' }))
    await user.click(screen.getByRole('button', { name: '生成凭据' }))
    const request = fixture.calls.find((call) => call.path.endsWith('/enrollment-tokens'))
    view.unmount()
    expect(request?.options.signal?.aborted).toBe(true)
    await act(async () =>
      finish(json({ id: id(4), token: crypto.randomUUID(), expires_at: expiry })),
    )
    render(<NodesPage client={fixture.client} />)
    await user.click(screen.getByRole('button', { name: '生成接入凭据' }))
    expect(screen.queryByRole('textbox', { name: '接入凭据码' })).not.toBeInTheDocument()
  })
})
