import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { expect, it } from 'vitest'
import { id, json, makeClient } from '../../tests/fixtures'
import { chooseOption } from '../../tests/select'
import { UsersPage } from './UsersPage'

it('只有禁用用户可编辑，更换节点和额度时用户名不进入请求', async () => {
  const row = {
    id: id(1),
    username: 'game-dev',
    display_name: '游戏开发',
    status: 'disabled',
    revision: 3,
    computer_status: 'ready',
    creation_operation_id: null,
    created_at: '2026-11-19',
  }
  const settings = {
    ...row,
    has_avatar: false,
    node_name: '原节点',
    image_reference: '原镜像',
    generation: 2,
    limits: {
      concurrent_model_calls: 2,
      concurrent_desktops: 2,
      queued_runs: 10,
      monthly_token_limit: '9007199254740993',
    },
    progress: null,
    computer: {
      node_id: id(3),
      image_id: id(4),
      resources: {
        cpu_millis: 1000,
        memory_bytes: 1024 ** 3,
        disk_bytes: 2 * 1024 ** 3,
        process_limit: 64,
      },
    },
  }
  const client = makeClient((path) => {
    if (path.startsWith('/computer-nodes'))
      return json({
        nodes: [
          {
            id: id(5),
            name: '目标节点',
            capacity: 1,
            reserved: 0,
            status: 'ready',
            image: { id: id(6), reference: '目标镜像', is_enabled: true },
          },
        ],
      })
    if (path.endsWith('/settings')) return json(settings)
    return json({ users: [row, { ...row, id: id(2), username: 'second', status: 'active' }] })
  })
  render(<UsersPage client={client.client} />)
  const user = userEvent.setup()
  await user.click(await screen.findByRole('button', { name: '编辑' }))
  const dialog = within(await screen.findByRole('dialog', { name: '编辑禁用用户' }))
  expect(await dialog.findByLabelText('用户名')).toHaveAttribute('readonly')
  await chooseOption(user, '电脑节点', /目标节点/)
  await user.clear(dialog.getByLabelText('显示名称'))
  await user.type(dialog.getByLabelText('显示名称'), '新名称')
  await user.clear(dialog.getByLabelText('桌面并发上限'))
  await user.type(dialog.getByLabelText('桌面并发上限'), '3')
  await user.click(dialog.getByRole('button', { name: '保存修改' }))
  const call = client.calls.find((c) => c.path.endsWith('/settings') && c.options.method === 'PUT')
  expect(JSON.parse(String(call?.options.body))).toMatchObject({
    revision: 3,
    generation: 2,
    display_name: '新名称',
    computer: { node_id: id(5), image_id: id(6) },
    limits: { concurrent_desktops: 3, monthly_token_limit: '9007199254740993' },
  })
  expect(JSON.parse(String(call?.options.body))).not.toHaveProperty('username')
})
