import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { App } from './App'
import { AdminClient } from './api/client'
import { failure, json, makeClient, operation, provider, session, id } from '../tests/fixtures'

describe('管理界面', () => {
  it('失效会话显示登录，失败后清空口令和动态码', async () => {
    let attempts = 0
    const client = new AdminClient(async (path) => {
      if (String(path).endsWith('/login')) {
        attempts++
        return failure('AUTH_INVALID', '登录信息无效', 401)
      }
      return failure('SESSION_INVALID', '未登录', 401)
    })
    const user = userEvent.setup()
    render(<App client={client} />)
    await user.type(await screen.findByLabelText('管理员用户名'), 'administrator')
    await user.type(screen.getByLabelText('密码'), crypto.randomUUID())
    await user.type(
      screen.getByLabelText('动态验证码'),
      String(crypto.getRandomValues(new Uint32Array(1))[0]! % 1_000_000).padStart(6, '0'),
    )
    await user.click(screen.getByRole('button', { name: '登录管理平台' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('登录信息无效')
    expect(screen.getByLabelText('密码')).toHaveValue('')
    expect(screen.getByLabelText('动态验证码')).toHaveValue('')
    expect(attempts).toBe(1)
  })
  it('分页读取用户目录，切换页面不保留上页内容', async () => {
    const records = Array.from({ length: 50 }, (_, index) => ({
      id: index.toString().padStart(26, '0'),
      username: `user-${index}`,
      display_name: `用户 ${index}`,
      role: 'user',
      status: 'active',
      computer_status: 'ready',
      creation_operation_id: null,
      created_at: new Date().toISOString(),
    }))
    const fixture = makeClient((path) =>
      path.includes('before=') ? json({ users: [] }) : json({ users: records }),
    )
    render(<App client={fixture.client} />)
    expect(await screen.findByText('用户 49')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: '下一页' }))
    expect(await screen.findByText('暂无用户')).toBeInTheDocument()
    expect(screen.queryByText('用户 49')).not.toBeInTheDocument()
    expect(fixture.calls.at(-1)?.path).toContain('before=')
    await userEvent.click(screen.getByRole('button', { name: '返回首页' }))
    expect(await screen.findByText('用户 49')).toBeInTheDocument()
  })
  it('同步开户超时展示真实待完成状态，并保留原操作', async () => {
    let creations = 0
    let finishRead: ((response: Response) => void) | undefined
    const fixture = makeClient((path, options) => {
      if (path.startsWith('/computer-nodes'))
        return json({
          nodes: [{ id: id(7), name: '节点甲', status: 'ready', capacity: 20, reserved: 1 }],
        })
      if (path.startsWith('/computer-images'))
        return json({ images: [{ id: id(8), reference: '测试镜像', is_enabled: true }] })
      if (path === '/users' && options.method === 'POST') {
        creations++
        return json({ code: 'CREATE_PENDING', message: '仍在创建', operation: operation() }, 504)
      }
      if (path.startsWith('/user-creation-operations/'))
        return new Promise<Response>((resolve) => {
          finishRead = resolve
        })
      return json({ users: [] })
    })
    const user = userEvent.setup()
    render(<App client={fixture.client} />)
    await user.click(await screen.findByRole('button', { name: '创建用户' }))
    await user.type(screen.getByLabelText('用户名'), 'new-user')
    await user.type(screen.getByLabelText('显示名称'), '新用户')
    await user.selectOptions(screen.getByLabelText('电脑节点'), id(7))
    await user.selectOptions(screen.getByLabelText('电脑镜像'), id(8))
    await user.click(screen.getByRole('button', { name: '创建用户与电脑' }))
    expect(await screen.findByRole('dialog', { name: '用户创建进度' })).toHaveTextContent(
      '正在读取进度',
    )
    finishRead!(json({ operation: operation() }))
    expect(await screen.findByText('等待创建')).toBeInTheDocument()
    expect(screen.queryByText('电脑已就绪，账户等待激活')).not.toBeInTheDocument()
    expect(creations).toBe(1)
  })
  it('创建回包丢失只在显式核对时复用同一请求与幂等键', async () => {
    const bodies: unknown[] = []
    const fixture = makeClient((path, options) => {
      if (path.startsWith('/computer-nodes'))
        return json({
          nodes: [{ id: id(7), name: '节点', status: 'ready', capacity: 2, reserved: 0 }],
        })
      if (path.startsWith('/computer-images'))
        return json({ images: [{ id: id(8), reference: '镜像', is_enabled: true }] })
      if (path === '/users') {
        bodies.push(JSON.parse(String(options.body)))
        if (bodies.length === 1) throw new TypeError('连接断开')
        return json({ operation: operation('succeeded') }, 201)
      }
      if (path.startsWith('/user-creation-operations'))
        return json({ operation: operation('succeeded') })
      return json({ users: [] })
    })
    const user = userEvent.setup()
    render(<App client={fixture.client} />)
    await user.click(await screen.findByRole('button', { name: '创建用户' }))
    await user.type(screen.getByLabelText('用户名'), 'new-user')
    await user.type(screen.getByLabelText('显示名称'), '新用户')
    await user.selectOptions(screen.getByLabelText('电脑节点'), id(7))
    await user.selectOptions(screen.getByLabelText('电脑镜像'), id(8))
    await user.click(screen.getByRole('button', { name: '创建用户与电脑' }))
    expect(await screen.findByText(/创建结果尚未确认/)).toBeInTheDocument()
    expect(bodies).toHaveLength(1)
    expect(screen.getByLabelText('用户名')).toBeDisabled()
    await user.click(screen.getByRole('button', { name: '核对原创建请求' }))
    await screen.findByText('电脑已就绪，账户等待激活')
    expect(bodies).toHaveLength(2)
    expect(bodies[0]).toEqual(bodies[1])
  })
  it('模型修订冲突不会覆盖或重试，显示重新读取入口', async () => {
    let writes = 0
    const fixture = makeClient((path, options) => {
      if (options.method === 'POST') {
        writes++
        expect(JSON.parse(String(options.body))).toEqual({
          expected_revision: 7,
          is_enabled: false,
        })
        return failure('MODEL_CONFIGURATION_CONFLICT', '配置已被更新', 409)
      }
      if (path.startsWith('/model-providers')) return json({ providers: [provider] })
      return json({ users: [] })
    })
    const user = userEvent.setup()
    render(<App client={fixture.client} />)
    await user.click(await screen.findByRole('button', { name: '模型供应商' }))
    await user.click(await screen.findByRole('button', { name: '停用' }))
    await user.click(screen.getByRole('button', { name: '确认停用供应商' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('配置已被更新')
    expect(screen.getByRole('button', { name: '确认停用供应商' })).toBeDisabled()
    expect(writes).toBe(1)
    await user.click(screen.getByRole('button', { name: '关闭并重新读取' }))
    expect(await screen.findByText('测试供应商')).toBeInTheDocument()
  })
  it('账户失效卸载业务内容，外观是唯一允许持久化的偏好', async () => {
    let invalid = false
    const authentication = session()
    const client = new AdminClient(async (path) => {
      if (invalid) return failure('SESSION_INVALID', '已退出', 401)
      if (String(path).endsWith('/session')) return json(authentication)
      return json({ users: [] })
    })
    render(<App client={client} />)
    await screen.findByText('暂无用户')
    await userEvent.selectOptions(screen.getByLabelText('外观'), 'dark')
    expect(document.documentElement.dataset.theme).toBe('dark')
    expect(localStorage.length).toBe(1)
    invalid = true
    await userEvent.click(screen.getByRole('button', { name: '刷新用户' }))
    await waitFor(() =>
      expect(screen.getByRole('button', { name: '登录管理平台' })).toBeInTheDocument(),
    )
    expect(screen.queryByText('暂无用户')).not.toBeInTheDocument()
  })
})
