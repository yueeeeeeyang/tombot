import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { App } from '../App'
import { AdminClient } from '../api/client'
import {
  activationInputs,
  empty,
  enrollment,
  failure,
  json,
  type Route,
} from '../../tests/fixtures'

function activationFixture(route?: Route) {
  const input = activationInputs()
  const setup = enrollment()
  const calls: { path: string; options: RequestInit }[] = []
  const client = new AdminClient(async (url, options = {}) => {
    const path = String(url)
    calls.push({ path, options })
    if (path.endsWith('/session')) return failure('SESSION_INVALID', '尚未登录', 401)
    if (route) return route(path, options)
    if (path.endsWith('/admin-enrollment')) return json(setup)
    return empty()
  })
  return { client, input, setup, calls }
}

async function enterSetup(fixture: ReturnType<typeof activationFixture>) {
  const user = userEvent.setup()
  render(<App client={fixture.client} />)
  await user.click(await screen.findByRole('button', { name: '激活管理员账户' }))
  await user.type(screen.getByLabelText('一次性激活材料'), fixture.input.token)
  await user.click(screen.getByRole('button', { name: '继续设置' }))
  await screen.findByLabelText('设置密码')
  return user
}

async function fillAccount(
  user: ReturnType<typeof userEvent.setup>,
  input: ReturnType<typeof activationInputs>,
) {
  await user.type(screen.getByLabelText('设置密码'), input.password)
  await user.type(screen.getByLabelText('确认密码'), input.password)
  await user.type(screen.getByLabelText('验证器动态码'), input.code)
}

describe('管理员激活', () => {
  it('完整登记并激活后清除材料，返回登录并提示使用新动态码', async () => {
    const fixture = activationFixture()
    const user = await enterSetup(fixture)
    expect(screen.getByLabelText('验证器设置密钥')).toHaveAttribute('type', 'password')
    await user.click(screen.getByRole('button', { name: '显示设置密钥' }))
    expect(screen.getByLabelText('验证器设置密钥')).toHaveAttribute('type', 'text')
    await user.click(screen.getByRole('button', { name: '隐藏设置密钥' }))
    await fillAccount(user, fixture.input)
    await user.click(screen.getByRole('button', { name: '激活账户' }))
    expect(await screen.findByRole('status')).toHaveTextContent('账户已激活')
    expect(screen.getByRole('status')).toHaveTextContent('新的动态验证码')
    expect(screen.queryByLabelText('验证器设置密钥')).not.toBeInTheDocument()
    expect(screen.getByLabelText('密码')).toHaveValue('')
    expect(screen.getByLabelText('动态验证码')).toHaveValue('')
    expect(fixture.calls.filter((call) => call.path.endsWith('/activate'))).toHaveLength(1)
    const body = JSON.parse(String(fixture.calls.at(-1)!.options.body))
    expect(
      body.activation_token === fixture.input.token &&
        body.password === fixture.input.password &&
        body.code === fixture.input.code,
    ).toBe(true)
    expect(fixture.client.snapshot().phase).toBe('anonymous')
    expect(localStorage.length).toBe(0)
    expect(sessionStorage.length).toBe(0)
    await user.click(screen.getByRole('button', { name: '激活管理员账户' }))
    expect(screen.getByLabelText('一次性激活材料')).toHaveValue('')
  })
  it('密码不一致时不调用接口且清空密码和动态码', async () => {
    const fixture = activationFixture()
    const user = await enterSetup(fixture)
    await fillAccount(user, fixture.input)
    await user.type(screen.getByLabelText('确认密码'), 'x')
    await user.click(screen.getByRole('button', { name: '激活账户' }))
    expect(screen.getByRole('alert')).toHaveTextContent('两次密码需一致')
    expect(screen.getByLabelText('设置密码')).toHaveValue('')
    expect(screen.getByLabelText('确认密码')).toHaveValue('')
    expect(screen.getByLabelText('验证器动态码')).toHaveValue('')
    expect(fixture.calls.filter((call) => call.path.endsWith('/activate'))).toHaveLength(0)
  })
  it('验证码拒绝保留登记步骤，显式重新输入才可提交', async () => {
    const setup = enrollment()
    const fixture = activationFixture((path) =>
      path.endsWith('/admin-enrollment')
        ? json(setup)
        : failure('AUTH_INVALID', '动态验证未通过', 401),
    )
    const user = await enterSetup(fixture)
    await fillAccount(user, fixture.input)
    await user.click(screen.getByRole('button', { name: '激活账户' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('动态验证未通过')
    expect(screen.getByLabelText('设置密码')).toHaveValue('')
    expect(screen.getByLabelText('验证器动态码')).toHaveValue('')
    expect(screen.getByLabelText('验证器设置密钥')).toHaveAttribute('type', 'password')
    expect(fixture.calls.filter((call) => call.path.endsWith('/activate'))).toHaveLength(1)
  })
  it('激活回包未知不重放并销毁材料，转到登录核实', async () => {
    const fixture = activationFixture((path) => {
      if (path.endsWith('/admin-enrollment')) return json(enrollment())
      throw new TypeError('测试断连')
    })
    const user = await enterSetup(fixture)
    await fillAccount(user, fixture.input)
    await user.click(screen.getByRole('button', { name: '激活账户' }))
    expect(await screen.findByRole('status')).toHaveTextContent('激活结果尚未确认')
    expect(screen.queryByRole('button', { name: '激活账户' })).not.toBeInTheDocument()
    expect(screen.queryByLabelText('验证器设置密钥')).not.toBeInTheDocument()
    expect(fixture.calls.filter((call) => call.path.endsWith('/activate'))).toHaveLength(1)
    await user.click(screen.getByRole('button', { name: '返回登录' }))
    expect(screen.getByRole('status')).toHaveTextContent('激活结果尚未确认')
  })
  it('过期激活材料回到材料输入，不继续提交旧材料', async () => {
    const fixture = activationFixture((path) =>
      path.endsWith('/admin-enrollment')
        ? json(enrollment())
        : failure('ACTIVATION_INVALID', '激活材料已经失效', 400),
    )
    const user = await enterSetup(fixture)
    await fillAccount(user, fixture.input)
    await user.click(screen.getByRole('button', { name: '激活账户' }))
    expect(await screen.findByLabelText('一次性激活材料')).toHaveValue('')
    expect(screen.queryByLabelText('验证器设置密钥')).not.toBeInTheDocument()
    expect(screen.getByRole('alert')).toHaveTextContent('激活材料已经失效')
  })
  it('取消在途登记会中止连接并丢弃迟到内容', async () => {
    let finish: ((response: Response) => void) | undefined
    const fixture = activationFixture(
      () =>
        new Promise<Response>((resolve) => {
          finish = resolve
        }),
    )
    const user = userEvent.setup()
    render(<App client={fixture.client} />)
    await user.click(await screen.findByRole('button', { name: '激活管理员账户' }))
    await user.type(screen.getByLabelText('一次性激活材料'), fixture.input.token)
    await user.click(screen.getByRole('button', { name: '继续设置' }))
    const signal = fixture.calls.at(-1)!.options.signal!
    await user.click(screen.getByRole('button', { name: '返回登录' }))
    expect(signal.aborted).toBe(true)
    await act(async () => {
      finish!(json(enrollment()))
    })
    expect(screen.queryByLabelText('验证器设置密钥')).not.toBeInTheDocument()
    expect(screen.getByLabelText('管理员用户名')).toBeInTheDocument()
  })
  it('离开历史页面清除材料和输入，恢复后重新开始', async () => {
    const fixture = activationFixture()
    const user = await enterSetup(fixture)
    await fillAccount(user, fixture.input)
    fireEvent(window, new Event('pagehide'))
    expect(screen.getByLabelText('一次性激活材料')).toHaveValue('')
    expect(screen.queryByLabelText('验证器设置密钥')).not.toBeInTheDocument()
    expect(screen.queryByLabelText('设置密码')).not.toBeInTheDocument()
  })
  it('快速重复提交只发出一次请求，取消激活显示未知结果', async () => {
    let finish: ((response: Response) => void) | undefined
    const fixture = activationFixture((path) =>
      path.endsWith('/admin-enrollment')
        ? json(enrollment())
        : new Promise<Response>((resolve) => {
            finish = resolve
          }),
    )
    const user = await enterSetup(fixture)
    await fillAccount(user, fixture.input)
    const form = screen.getByLabelText('设置密码').closest('form')!
    fireEvent.submit(form)
    fireEvent.submit(form)
    expect(fixture.calls.filter((call) => call.path.endsWith('/activate'))).toHaveLength(1)
    await user.click(screen.getByRole('button', { name: '返回登录' }))
    expect(screen.getByRole('status')).toHaveTextContent('激活结果尚未确认')
    await act(async () => {
      finish!(empty())
    })
    await waitFor(() =>
      expect(screen.queryByText('账户已激活', { exact: false })).not.toBeInTheDocument(),
    )
  })
})
