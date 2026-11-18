import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { it, expect } from 'vitest'
import { NodeUpgradeDialog } from './NodeUpgradeDialog'
import { json, makeClient, id } from '../../tests/fixtures'
it('仅向所选范围下发，未通过验收的节点不显示就绪', async () => {
  const written: unknown[] = []
  const fixture = makeClient((path, options) => {
    if (path === '/node-updates/dispatch') {
      written.push(JSON.parse(String(options.body)))
      return json({})
    }
    return json({
      releases: [{ id: 'r', label: 'v1', platform: 'linux/amd64' }],
      jobs: [{ id: 'j', node: id(4), state: 'started', label: 'v1' }],
    })
  })
  render(
    <NodeUpgradeDialog
      client={fixture.client}
      nodes={[{ id: id(4), name: '节点甲' }]}
      onClose={() => {}}
    />,
  )
  const user = userEvent.setup()
  expect(await screen.findByText(/等待验收/)).toBeInTheDocument()
  expect(screen.queryByText('验收就绪')).not.toBeInTheDocument()
  await user.selectOptions(screen.getByLabelText('目标版本'), 'r')
  await user.click(screen.getByLabelText('节点甲'))
  await user.click(screen.getByRole('button', { name: /升级所选/ }))
  await waitFor(() => expect(written).toEqual([{ release: 'r', nodes: [id(4)], all: false }]))
  await user.click(await screen.findByLabelText(/所有同架构更新节点/))
  await user.click(screen.getByRole('button', { name: '升级所有同架构节点' }))
  await waitFor(() => expect(written[1]).toEqual({ release: 'r', nodes: [], all: true }))
})
