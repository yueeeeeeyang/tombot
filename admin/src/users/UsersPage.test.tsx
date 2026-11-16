import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { UsersPage } from './UsersPage'
import { failure, id, json, makeClient, operation } from '../../tests/fixtures'
import { chooseOption } from '../../tests/select'

function node(digit = 4) {
  return {
    id: id(digit),
    name: `节点${digit}`,
    status: 'ready',
    capacity: 4,
    reserved: 0,
    image: { id: id(digit + 2), reference: `已验收镜像${digit}`, is_enabled: true },
  }
}

describe('创建电脑的节点镜像关联', () => {
  it('切换节点自动切换已验收镜像，只提交所选节点的镜像', async () => {
    const fixture = makeClient((path, options) => {
      if (path.startsWith('/computer-nodes')) return json({ nodes: [node(), node(5)] })
      if (
        (path === '/users' && options.method === 'POST') ||
        path.startsWith('/user-creation-operations/')
      )
        return json({ operation: operation('succeeded') })
      return json({ users: [] })
    })
    render(<UsersPage client={fixture.client} />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: '创建用户' }))
    const dialog = screen.getByRole('dialog', { name: '创建用户与电脑' })
    expect(within(dialog).queryByRole('combobox', { name: '电脑镜像' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '创建用户与电脑' })).toBeDisabled()
    await chooseOption(user, '电脑节点', /^节点4/)
    expect(screen.getByLabelText('电脑镜像')).toHaveTextContent('已验收镜像4')
    await chooseOption(user, '电脑节点', /^节点5/)
    expect(screen.getByLabelText('电脑镜像')).toHaveTextContent('已验收镜像5')
    expect(within(dialog).queryByText(/本页.*条/)).not.toBeInTheDocument()
    await user.type(screen.getByLabelText('用户名'), 'new-user')
    await user.type(screen.getByLabelText('显示名称'), '新用户')
    await user.click(screen.getByRole('button', { name: '创建用户与电脑' }))
    await screen.findByText('电脑已就绪，账户等待激活')
    const request = fixture.calls.find(
      ({ path, options }) => path === '/users' && options.method === 'POST',
    )
    expect(JSON.parse(String(request?.options.body))).toMatchObject({
      node_id: id(5),
      image_id: id(7),
    })
    expect(fixture.calls.some(({ path }) => path.startsWith('/computer-images'))).toBe(false)
  })

  it.each([
    { status: 'bootstrapping' },
    { status: 'offline' },
    { reserved: 4 },
    { image: null },
    { image: { ...node().image, is_enabled: false } },
  ])('不可用节点或镜像禁止创建：%j', async (change) => {
    const fixture = makeClient((path) =>
      path.startsWith('/computer-nodes')
        ? json({ nodes: [{ ...node(), ...change }, node(5)] })
        : json({ users: [] }),
    )
    render(<UsersPage client={fixture.client} />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: '创建用户' }))
    await user.click(screen.getByRole('combobox', { name: '电脑节点' }))
    expect(await screen.findByRole('option', { name: /^节点4/ })).toHaveAttribute(
      'aria-disabled',
      'true',
    )
    await user.click(screen.getByRole('option', { name: /^节点4/ }))
    expect(screen.getByRole('button', { name: '创建用户与电脑' })).toBeDisabled()
    expect(screen.getByLabelText('电脑镜像')).toHaveTextContent('选择可用节点后自动确定')
  })

  it('目录读取失败时禁止创建，重新读取后恢复选择', async () => {
    let reads = 0
    const fixture = makeClient((path) => {
      if (path.startsWith('/computer-nodes')) {
        reads++
        return reads === 1
          ? failure('TEMPORARILY_UNAVAILABLE', '节点目录暂不可用', 503)
          : json({ nodes: [node()] })
      }
      return json({ users: [] })
    })
    render(<UsersPage client={fixture.client} />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: '创建用户' }))
    await screen.findByText('节点目录暂不可用')
    expect(screen.getByRole('button', { name: '创建用户与电脑' })).toBeDisabled()
    await user.click(screen.getByRole('button', { name: '重新读取电脑资源' }))
    await waitFor(() => expect(screen.getByRole('combobox', { name: '电脑节点' })).toBeEnabled())
    await chooseOption(user, '电脑节点', /^节点4/)
    expect(screen.getByLabelText('电脑镜像')).toHaveTextContent('已验收镜像4')
  })

  it('提交时节点升级则刷新目录并要求重新选择，不沿用旧镜像', async () => {
    let upgraded = false
    const writes: { image_id: string; idempotency_key: string }[] = []
    const fixture = makeClient((path, options) => {
      if (path.startsWith('/computer-nodes'))
        return json({ nodes: [{ ...node(), image: upgraded ? node(5).image : node().image }] })
      if (path === '/users' && options.method === 'POST') {
        writes.push(JSON.parse(String(options.body)))
        if (!upgraded) {
          upgraded = true
          return failure('CAPACITY_UNAVAILABLE', '节点已变化，请重新选择', 409)
        }
        return json({ operation: operation('succeeded') })
      }
      if (path.startsWith('/user-creation-operations/'))
        return json({ operation: operation('succeeded') })
      return json({ users: [] })
    })
    render(<UsersPage client={fixture.client} />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: '创建用户' }))
    await user.type(screen.getByLabelText('用户名'), 'new-user')
    await user.type(screen.getByLabelText('显示名称'), '新用户')
    await chooseOption(user, '电脑节点', /^节点4/)
    await user.click(screen.getByRole('button', { name: '创建用户与电脑' }))
    await screen.findByText('节点已变化，请重新选择')
    await waitFor(() =>
      expect(screen.getByLabelText('电脑镜像')).toHaveTextContent('选择可用节点后自动确定'),
    )
    expect(screen.getByRole('button', { name: '创建用户与电脑' })).toBeDisabled()
    await chooseOption(user, '电脑节点', /^节点4/)
    expect(screen.getByLabelText('电脑镜像')).toHaveTextContent('已验收镜像5')
    await user.click(screen.getByRole('button', { name: '创建用户与电脑' }))
    await screen.findByText('电脑已就绪，账户等待激活')
    expect(writes.map((request) => request.image_id)).toEqual([id(6), id(7)])
    expect(writes[0]?.idempotency_key).not.toBe(writes[1]?.idempotency_key)
  })
})
