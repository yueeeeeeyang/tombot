import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { App } from '../App'
import { id, json, makeClient, provider } from '../../tests/fixtures'

describe('模型目录空状态', () => {
  it.each([
    { navigation: '模型供应商', title: '暂无模型供应商', detail: '' },
    { navigation: '模型策略', title: '暂无模型策略', detail: '' },
    { navigation: '模型供应商', title: '暂无模型', detail: '模型管理' },
    { navigation: '模型策略', title: '暂无策略版本', detail: '版本管理' },
  ])('$title 使用独立空面板，不显示空表格或零条分页', async ({ navigation, title, detail }) => {
    const policy = {
      id: id(6),
      name: '测试策略',
      is_default: false,
      is_enabled: true,
      revision: 1,
      current_version: null,
    }
    const fixture = makeClient((path) => {
      if (path.endsWith('/models?limit=50')) return json({ models: [] })
      if (path.endsWith('/versions?limit=50')) return json({ versions: [] })
      if (path === `/model-providers/${provider.id}`) return json({ provider })
      if (path === `/model-policies/${policy.id}`) return json({ policy })
      if (path.startsWith('/model-providers')) return json({ providers: detail ? [provider] : [] })
      if (path.startsWith('/model-policies'))
        return json({
          policies: detail ? [policy] : [],
        })
      return json({ users: [] })
    })
    const user = userEvent.setup()
    const { container } = render(<App client={fixture.client} />)
    await user.click(await screen.findByRole('link', { name: navigation }))
    if (detail) await user.click(await screen.findByRole('link', { name: detail }))
    expect(await screen.findByRole('heading', { name: title })).toBeInTheDocument()
    expect(screen.queryByRole('table')).not.toBeInTheDocument()
    expect(screen.queryByText(/本页/)).not.toBeInTheDocument()
    expect(container.querySelector('.pagination')).toBeNull()
    expect(container.querySelector('.empty-state svg')).toBeInTheDocument()
  })
})
