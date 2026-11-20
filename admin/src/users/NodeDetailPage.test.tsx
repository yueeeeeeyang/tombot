import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router'
import { describe, expect, it } from 'vitest'
import { NodeDetailPage } from './NodeDetailPage'
import { NodesPage } from './NodesPage'
import { failure, json, makeClient } from '../../tests/fixtures'
import { nodeDetail } from '../../tests/node-fixture'

function mount(fixture: ReturnType<typeof makeClient>, path: string) {
  render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path="/computer-nodes" element={<NodesPage client={fixture.client} />} />
        <Route
          path="/computer-nodes/:nodeID"
          element={<NodeDetailPage client={fixture.client} />}
        />
      </Routes>
    </MemoryRouter>,
  )
}

describe('节点详情与编辑', () => {
  const basicCapabilities = {
    terminal: true,
    files: true,
    process_reaping: true,
    disk_quota: true,
    network_isolation: true,
  }
  it('就绪节点省略浏览器探针时，从列表进入详情仍显示五项基础验收', async () => {
    const node = nodeDetail()
    node.status = 'ready'
    node.diagnosis = { code: 'ready', message: '心跳与电脑能力验收均有效，可以接收新电脑。' }
    node.can_retry_verification = false
    Object.assign(node.verification!, {
      state: 'succeeded',
      error_code: null,
      node_error_code: null,
      capabilities: basicCapabilities,
    })
    const fixture = makeClient((path) =>
      json(path.split('?')[0] === '/computer-nodes' ? { nodes: [node] } : node),
    )
    mount(fixture, '/computer-nodes')
    expect(await screen.findByText('就绪')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('link', { name: node.name }))
    expect(await screen.findByText(node.diagnosis.message)).toBeInTheDocument()
    expect(screen.getAllByText('已通过')).toHaveLength(5)
    expect(screen.queryByText('未通过')).not.toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })
  it('显示阻止电脑升级的节点验收错误', async () => {
    const node = nodeDetail()
    mount(
      makeClient(() =>
        json({ ...node, upgrade_step: 'verification_quarantined/agent_retirement_entry_unknown' }),
      ),
      `/computer-nodes/${node.id}`,
    )
    expect(
      await screen.findByText(/升级阶段 · 节点验收已隔离 · agent_retirement_entry_unknown/),
    ).toBeInTheDocument()
  })
  it('保留历史浏览器探针结果', async () => {
    const node = nodeDetail()
    node.verification!.capabilities = { ...basicCapabilities, browser: true }
    mount(
      makeClient(() => json(node)),
      `/computer-nodes/${node.id}`,
    )
    expect(await screen.findByText('历史浏览器探针')).toBeInTheDocument()
    expect(screen.getAllByText('已通过')).toHaveLength(6)
  })
  it('缺少必需的基础能力时仍拒绝详情响应', async () => {
    const node = nodeDetail()
    const verification = {
      ...node.verification,
      capabilities: { ...basicCapabilities, files: undefined },
    }
    mount(
      makeClient(() => json({ ...node, verification })),
      `/computer-nodes/${node.id}`,
    )
    expect(await screen.findByRole('alert')).toHaveTextContent('服务返回的数据不完整')
  })
  it('列表展示验收隔离，详情显示原因和未确认能力，按原版本重新核对', async () => {
    const node = nodeDetail()
    let retries = 0
    const fixture = makeClient((path, options) => {
      if (path.split('?')[0] === '/computer-nodes')
        return json({ nodes: [{ ...node, verification_status: 'quarantined' }] })
      if (path.endsWith('/verification-retry')) {
        retries++
        expect(JSON.parse(String(options.body))).toEqual({ fencing_token: 7 })
        node.verification!.state = 'checking'
        node.can_retry_verification = false
        node.diagnosis = { code: 'checking', message: '正在核对原验收资源' }
      }
      return json(node)
    })
    const user = userEvent.setup()
    mount(fixture, '/computer-nodes')
    expect(await screen.findByText('验收已隔离')).toBeInTheDocument()
    expect(screen.queryByText('引导中')).not.toBeInTheDocument()
    await user.click(screen.getByRole('link', { name: node.name }))
    expect(await screen.findByText('container_identity_mismatch')).toBeInTheDocument()
    expect(screen.getAllByText('未确认')).toHaveLength(5)
    expect(screen.queryByText('历史浏览器探针')).not.toBeInTheDocument()
    expect(screen.getByText(node.url!)).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '重新核对验收' }))
    await user.click(screen.getByRole('button', { name: '开始核对' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(retries).toBe(1)
    expect(await screen.findByText('正在核对原验收资源')).toBeInTheDocument()
  })
  it('详情通过弹窗编辑，只提交可编辑字段及配置版本，保存后刷新详情', async () => {
    let node = nodeDetail()
    let writes = 0
    const fixture = makeClient((_path, options) => {
      if (options.method === 'PUT') {
        writes++
        const body = JSON.parse(String(options.body))
        expect(body).toEqual({
          name: '机房节点',
          capacity: 8,
          is_draining: true,
          configuration_version: 2,
        })
        node = { ...node, ...body, configuration_version: 3 }
      }
      return json(node)
    })
    const user = userEvent.setup()
    mount(fixture, `/computer-nodes/${node.id}`)
    await user.click(await screen.findByRole('button', { name: '编辑节点' }))
    const name = await screen.findByLabelText('节点名称')
    await user.clear(name)
    await user.type(name, '机房节点')
    const capacity = screen.getByLabelText('准入容量')
    await user.clear(capacity)
    await user.type(capacity, '8')
    await user.click(screen.getByRole('combobox', { name: '接收新电脑' }))
    await user.click(screen.getByRole('option', { name: '暂停接收' }))
    expect(screen.queryByRole('textbox', { name: '节点地址' })).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '保存修改' }))
    expect(await screen.findByRole('heading', { name: '机房节点' })).toBeInTheDocument()
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(writes).toBe(1)
  })
  it('并发冲突后要求重读，不盲目重发；读取失败不伪造节点状态', async () => {
    const node = nodeDetail()
    let writes = 0
    const fixture = makeClient((_path, options) => {
      if (options.method === 'PUT') {
        writes++
        return failure('NODE_CONFLICT', '节点配置已变化', 409)
      }
      return json(node)
    })
    const user = userEvent.setup()
    mount(fixture, `/computer-nodes/${node.id}`)
    await user.click(await screen.findByRole('button', { name: '编辑节点' }))
    await screen.findByLabelText('节点名称')
    await user.click(screen.getByRole('button', { name: '保存修改' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('节点配置已变化')
    expect(screen.queryByRole('button', { name: '保存修改' })).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '重新读取当前配置' }))
    expect(await screen.findByRole('button', { name: '保存修改' })).toBeEnabled()
    expect(writes).toBe(1)
  })
  it.each([
    ['browser_start_membership_failed', '浏览器进程加入受管资源组失败'],
    ['browser_start_deadline_failed', '浏览器启动期限或宿主与容器的共享时钟校验失败'],
    ['browser_start_budget_failed', '浏览器运行预算超过电脑代理的部署限制'],
    ['browser_start_state_failed', '浏览器启动授权已消费，或代理已停止接单'],
  ])('浏览器启动分类 %s 显示具体阶段，不误报其他能力已通过', async (code, message) => {
    const node = nodeDetail()
    node.verification!.state = 'failed'
    node.verification!.error_code = 'capability_check_failed'
    node.verification!.node_error_code = code
    const fixture = makeClient(() => json(node))
    mount(fixture, `/computer-nodes/${node.id}`)
    expect(await screen.findByText(new RegExp(message))).toBeInTheDocument()
    expect(screen.getAllByText('未确认')).toHaveLength(6)
    expect(screen.queryByText('已通过')).not.toBeInTheDocument()
  })
  it('详情读取失败和缺失明确显示错误', async () => {
    const fixture = makeClient(() => failure('NODE_NOT_FOUND', '节点不存在', 404))
    mount(fixture, `/computer-nodes/${nodeDetail().id}`)
    expect(await screen.findByRole('alert')).toHaveTextContent('节点不存在')
    expect(screen.queryByRole('button', { name: '重新核对验收' })).not.toBeInTheDocument()
  })
  it('列表弹窗读取最新配置，取消不保存，再次打开重新读取并刷新列表', async () => {
    let node = nodeDetail()
    let reads = 0
    let writes = 0
    const fixture = makeClient((path, options) => {
      if (path.split('?')[0] === '/computer-nodes') return json({ nodes: [node] })
      if (options.method === 'PUT') {
        writes++
        const body = JSON.parse(String(options.body))
        expect(body.configuration_version).toBe(3)
        node = { ...node, ...body, configuration_version: 4 }
      } else reads++
      return json(node)
    })
    const user = userEvent.setup()
    mount(fixture, '/computer-nodes')
    const edit = await screen.findByRole('button', { name: '编辑' })
    await user.click(edit)
    await user.clear(await screen.findByLabelText('节点名称'))
    await user.type(screen.getByLabelText('节点名称'), '未保存名称')
    await user.click(screen.getByRole('button', { name: '取消' }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(edit).toHaveFocus()
    expect(writes).toBe(0)
    node = { ...node, name: '已更新名称', configuration_version: 3 }
    await user.click(edit)
    expect(await screen.findByLabelText('节点名称')).toHaveValue('已更新名称')
    await user.clear(screen.getByLabelText('节点名称'))
    await user.type(screen.getByLabelText('节点名称'), '最终名称')
    await user.click(screen.getByRole('button', { name: '保存修改' }))
    expect(await screen.findByRole('link', { name: '最终名称' })).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: '电脑节点' })).toBeInTheDocument()
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(reads).toBe(2)
    expect(writes).toBe(1)
  })
  it('弹窗读取失败时显示错误并允许重试，不显示可提交的旧表单', async () => {
    const node = nodeDetail()
    let failed = true
    const fixture = makeClient((path) => {
      if (path.split('?')[0] === '/computer-nodes') return json({ nodes: [node] })
      return failed ? failure('NODE_NOT_FOUND', '节点不存在', 404) : json(node)
    })
    const user = userEvent.setup()
    mount(fixture, '/computer-nodes')
    await user.click(await screen.findByRole('button', { name: '编辑' }))
    const dialog = within(screen.getByRole('dialog', { name: '编辑节点' }))
    expect(await dialog.findByRole('alert')).toHaveTextContent('节点不存在')
    expect(dialog.queryByRole('button', { name: '保存修改' })).not.toBeInTheDocument()
    failed = false
    await user.click(dialog.getByRole('button', { name: '重新读取' }))
    expect(await dialog.findByLabelText('节点名称')).toHaveValue(node.name)
  })
  it('保存期间阻止关闭和重复提交，成功后关闭弹窗', async () => {
    const node = nodeDetail()
    let complete!: (response: Response) => void
    let writes = 0
    const fixture = makeClient((_path, options) => {
      if (options.method === 'PUT') {
        writes++
        return new Promise<Response>((resolve) => {
          complete = resolve
        })
      }
      return json(node)
    })
    const user = userEvent.setup()
    mount(fixture, `/computer-nodes/${node.id}`)
    await user.click(await screen.findByRole('button', { name: '编辑节点' }))
    await user.click(await screen.findByRole('button', { name: '保存修改' }))
    await waitFor(() => expect(writes).toBe(1))
    expect(screen.getByRole('button', { name: '正在保存…' })).toBeDisabled()
    expect(screen.getByRole('button', { name: '关闭弹窗' })).toBeDisabled()
    expect(screen.getByRole('button', { name: '取消' })).toBeDisabled()
    expect(screen.getByLabelText('节点名称')).toBeDisabled()
    fireEvent(screen.getByRole('dialog'), new Event('cancel', { cancelable: true }))
    expect(screen.getByRole('dialog')).toBeInTheDocument()
    complete(json(node))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(writes).toBe(1)
  })
})
