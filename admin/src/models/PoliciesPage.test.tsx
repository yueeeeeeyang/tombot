import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { App } from '../App'
import { empty, failure, id, json, makeClient } from '../../tests/fixtures'

describe('停用策略的版本操作', () => {
  it.each(['success', 'conflict', 'unavailable'])(
    '显示停用原因并从详情启用策略：%s',
    async (outcome) => {
      const policy = {
        id: id(6),
        name: '默认策略',
        is_default: true,
        is_enabled: false,
        revision: 7,
        current_version: null as number | null,
      }
      const version = {
        policy_id: policy.id,
        version: 1,
        routing_mode: 'priority',
        is_enabled: false,
        verified_at: null,
        candidates: [
          {
            position: 1,
            provider_id: id(4),
            model_id: id(5),
            credential_id: null,
            priority: 1,
            weight: 1,
            configuration: {
              protocol: 'responses',
              base_url: 'https://models.example.test',
              model_key: 'fast',
              context_limit: 8192,
              output_limit: 1024,
              supports_tools: true,
              supports_vision: false,
              prices: {},
            },
          },
        ],
      }
      let stateWrites = 0,
        versionWrites = 0
      const fixture = makeClient((path, options) => {
        if (path.endsWith('/state')) {
          stateWrites++
          expect(JSON.parse(String(options.body))).toEqual({
            expected_revision: 7,
            is_enabled: true,
          })
          if (outcome === 'conflict')
            return failure('MODEL_CONFIGURATION_CONFLICT', '策略已被其他管理员修改', 409)
          if (outcome === 'unavailable')
            return failure('SERVICE_UNAVAILABLE', '暂时无法启用策略', 503)
          policy.is_enabled = true
          policy.revision++
          return empty()
        }
        if (path.endsWith('/enable')) {
          expect(JSON.parse(String(options.body))).toEqual({ expected_revision: 8 })
          versionWrites++
          version.is_enabled = true
          policy.current_version = 1
          policy.revision++
          return json({ version })
        }
        if (path.startsWith(`/model-policies/${policy.id}/versions?`))
          return json({ versions: [version] })
        if (path === `/model-policies/${policy.id}`) return json({ policy })
        if (path.startsWith('/model-policies?')) return json({ policies: [policy] })
        throw new Error(`未配置的测试接口：${path}`)
      })
      await fixture.client.restore()
      const user = userEvent.setup()
      window.history.replaceState(null, '', '/admin/model-policies')
      render(<App client={fixture.client} />)
      await user.click(await screen.findByRole('link', { name: '版本管理' }))
      const enableVersion = await screen.findByRole('button', { name: '启用' })
      expect(enableVersion).toBeDisabled()
      expect(enableVersion).toHaveAccessibleDescription('请先启用策略，再启用所需版本。')
      await user.click(screen.getByRole('button', { name: '启用策略' }))
      expect(stateWrites).toBe(0)
      await user.click(screen.getByRole('button', { name: '取消' }))
      expect(stateWrites).toBe(0)
      await user.click(await screen.findByRole('button', { name: '启用策略' }))
      await user.click(screen.getByRole('button', { name: '确认启用策略' }))
      if (outcome === 'success') {
        const enable = await screen.findByRole('button', { name: '启用' })
        expect(enable).toBeEnabled()
        expect(screen.queryByText('策略已停用')).not.toBeInTheDocument()
        await user.click(enable)
        await user.click(screen.getByRole('button', { name: '确认启用版本 v1' }))
        expect(await screen.findByText('已启用')).toBeInTheDocument()
        expect(versionWrites).toBe(1)
      } else {
        expect(await screen.findByRole('alert')).toHaveTextContent(
          outcome === 'conflict' ? '策略已被其他管理员修改' : '暂时无法启用策略',
        )
        expect(screen.getByRole('button', { name: '确认启用策略' })).toBeDisabled()
        await user.click(screen.getByRole('button', { name: '关闭并重新读取' }))
        expect(await screen.findByRole('button', { name: '启用' })).toBeDisabled()
        expect(versionWrites).toBe(0)
      }
      expect(stateWrites).toBe(1)
    },
  )
})
