import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { it, expect, vi } from 'vitest'
import { createHash, webcrypto } from 'node:crypto'
import { NodeUpgradeDialog } from './NodeUpgradeDialog'
import { failure, json, makeClient, id } from '../../tests/fixtures'
import { chooseOption } from '../../tests/select'

const release = { id: 'r', label: 'v1', platform: 'linux/amd64' }
const nodes = [
  { id: id(4), name: '节点甲' },
  { id: id(5), name: '节点乙' },
]

it('同一版本的节点程序和电脑镜像能单独选择下发', async () => {
  const releases = [
    { ...release, kind: 'programs' },
    { ...release, id: 'image-r', kind: 'image' },
  ]
  const dispatch = vi.fn()
  const fixture = makeClient((path, options) => {
    if (path === '/node-updates/dispatch') {
      dispatch(JSON.parse(String(options.body)))
      return json({})
    }
    return json({ releases, jobs: [] })
  })
  render(<NodeUpgradeDialog client={fixture.client} nodes={nodes} onClose={() => {}} />)
  const user = userEvent.setup()
  await screen.findByRole('combobox', { name: '目标版本' })
  await chooseOption(user, '目标版本', 'v1 · linux/amd64 · 节点程序')
  await chooseOption(user, '目标版本', 'v1 · linux/amd64 · 电脑镜像')
  await user.click(screen.getByLabelText('节点甲'))
  await user.click(screen.getByRole('button', { name: '升级所选节点（1）' }))
  await waitFor(() =>
    expect(dispatch).toHaveBeenCalledWith({ release: 'image-r', nodes: [id(4)], all: false }),
  )
})

it('仅向所选范围下发，未通过验收的节点不显示就绪', async () => {
  const written: unknown[] = []
  const fixture = makeClient((path, options) => {
    if (path === '/node-updates/dispatch') {
      written.push(JSON.parse(String(options.body)))
      return json({})
    }
    return json({
      releases: [release],
      jobs: [{ id: 'j', node: id(4), state: 'started', label: 'v1' }],
    })
  })
  render(<NodeUpgradeDialog client={fixture.client} nodes={nodes} onClose={() => {}} />)
  const user = userEvent.setup()
  expect(await screen.findByText(/等待验收/)).toBeInTheDocument()
  expect(screen.queryByText('验收就绪')).not.toBeInTheDocument()
  await chooseOption(user, '目标版本', 'v1 · linux/amd64')
  const selectPage = screen.getByRole('checkbox', { name: '选择本页全部节点' })
  await user.click(selectPage)
  expect(screen.getByLabelText('节点乙')).toBeChecked()
  await user.click(screen.getByLabelText('节点乙'))
  expect(selectPage).toBePartiallyChecked()
  await user.click(screen.getByRole('button', { name: '升级所选节点（1）' }))
  await waitFor(() => expect(written).toEqual([{ release: 'r', nodes: [id(4)], all: false }]))
  await user.click(screen.getByRole('radio', { name: '所有同架构节点' }))
  expect(screen.getByText(/包含其他分页/)).toBeVisible()
  expect(screen.queryByRole('checkbox')).not.toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: '升级所有同架构节点' }))
  await waitFor(() => expect(written[1]).toEqual({ release: 'r', nodes: [], all: true }))
})

it('只选择一个文件，完整性校验失败时保留文件并禁止下发', async () => {
  vi.stubGlobal('crypto', webcrypto)
  const fixture = makeClient((path) => {
    if (path.endsWith('/uploads')) return json({ next: 0 })
    if (path.includes('/parts/')) return json({})
    if (path.endsWith('/releases')) return failure('PACKAGE_INVALID', '离线包校验失败')
    return json({ releases: [], jobs: [] })
  })
  render(<NodeUpgradeDialog client={fixture.client} nodes={nodes} onClose={() => {}} />)
  const user = userEvent.setup()
  const input = await screen.findByLabelText('程序包或电脑镜像包')
  const submit = screen.getByRole('button', { name: '上传发布包' })
  expect(screen.queryByLabelText('发布签名清单')).not.toBeInTheDocument()
  expect(submit).toBeDisabled()
  await user.upload(input, new File([], 'node.tar.gz'))
  await user.click(submit)
  expect(await screen.findByRole('alert')).toHaveTextContent('非空发布包')
  await user.upload(input, new File(['abcd'], 'node.tar.gz'))
  await user.click(submit)
  expect(await screen.findByRole('alert')).toHaveTextContent('离线包校验失败')
  const sha = (value: string) => createHash('sha256').update(value).digest('hex')
  expect(fixture.calls.map((call) => call.path)).toEqual([
    '/node-updates/status',
    '/node-updates/uploads',
    '/node-updates/parts/' + sha(sha('abcd')) + '/0/' + sha('abcd'),
    '/node-updates/releases',
  ])
  expect(JSON.parse(String(fixture.calls.at(-1)?.options.body))).toEqual({
    artifact: sha(sha('abcd')),
  })
})

it.each([true, false])('等待所有分片确认后选中新版本，最后一片失败=%s', async (fails) => {
  vi.stubGlobal('crypto', webcrypto)
  const archive = new File([new Uint8Array(4 * 1024 * 1024 + 8)], 'tomnode.tar.gz')
  const received: number[] = []
  let finishUpload!: (response: Response) => void
  const lastPart = new Promise<Response>((resolve) => {
    finishUpload = resolve
  })
  const fixture = makeClient((path, options) => {
    if (path === '/node-updates/uploads') return json({ next: 0 })
    if (path === '/node-updates/releases') return json(release)
    if (path.startsWith('/node-updates/parts/')) {
      received.push((options.body as Blob).size)
      return path.includes('/1/') ? lastPart : json({})
    }
    return json({ releases: received.length && !fails ? [release] : [], jobs: [] })
  })
  render(<NodeUpgradeDialog client={fixture.client} nodes={nodes} onClose={() => {}} />)
  const user = userEvent.setup()
  await user.upload(await screen.findByLabelText('程序包或电脑镜像包'), archive)
  await user.click(screen.getByRole('button', { name: '上传发布包' }))
  await waitFor(() => expect(received).toEqual([4 * 1024 * 1024, 8]))
  expect(screen.getByRole('progressbar')).toHaveAttribute('value', '99')
  expect(screen.getByRole('button', { name: '返回' })).toBeDisabled()
  finishUpload(fails ? failure('PART_INVALID', '分片校验失败') : json({}))
  if (fails) {
    expect(await screen.findByRole('alert')).toHaveTextContent('分片校验失败')
    expect(screen.getByText(archive.name)).toBeVisible()
    expect(screen.getByRole('button', { name: '上传发布包' })).toBeEnabled()
  } else {
    await waitFor(() =>
      expect(screen.getByRole('combobox', { name: '目标版本' })).toHaveTextContent(
        'v1 · linux/amd64',
      ),
    )
    expect(screen.getByRole('button', { name: '升级所选节点（0）' })).toBeDisabled()
  }
  expect(fixture.calls.some((call) => call.path.endsWith('/dispatch'))).toBe(false)
})

it('刷新期间保留版本和节点选择，刷新失败后提示重新读取', async () => {
  let finishRead!: (response: Response) => void
  const refresh = new Promise<Response>((resolve) => {
    finishRead = resolve
  })
  let reads = 0
  const fixture = makeClient(() =>
    ++reads === 2 ? refresh : json({ releases: [release], jobs: [] }),
  )
  render(<NodeUpgradeDialog client={fixture.client} nodes={nodes} onClose={() => {}} />)
  const user = userEvent.setup()
  await screen.findByRole('combobox', { name: '目标版本' })
  await chooseOption(user, '目标版本', 'v1 · linux/amd64')
  await user.click(screen.getByLabelText('节点乙'))
  await user.click(screen.getByRole('button', { name: '刷新状态' }))
  expect(screen.getByLabelText('节点乙')).toBeChecked()
  expect(screen.getByRole('combobox')).toHaveTextContent('v1 · linux/amd64')
  finishRead(failure('STATUS_FAILED', '读取升级状态失败'))
  expect(await screen.findByRole('alert')).toHaveTextContent('读取升级状态失败')
  expect(screen.getByRole('button', { name: '升级所选节点（1）' })).toBeDisabled()
  await user.click(screen.getByRole('button', { name: '重新读取' }))
  expect(await screen.findByLabelText('节点乙')).toBeChecked()
  expect(screen.getByRole('button', { name: '升级所选节点（1）' })).toBeEnabled()
})
