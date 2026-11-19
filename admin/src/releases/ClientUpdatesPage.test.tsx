import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createHash, webcrypto } from 'node:crypto'
import { expect, it, vi } from 'vitest'
import { json, makeClient, failure } from '../../tests/fixtures'
import { ClientUpdatesPage } from './ClientUpdatesPage'

it.each([false, true])(
  '只有全部分片上传成功才发布指定平台的版本与说明，分片失败=%s',
  async (fails) => {
    vi.stubGlobal('crypto', webcrypto)
    const chunks: string[] = []
    const releases: unknown[] = []
    const catalog = {
      releases: [],
      platforms: { 'darwin-arm64': ['.dmg'], 'win32-x64': ['.exe'] },
      chunk_size: 3,
      max_size: 30,
    }
    const fixture = makeClient((path, options) => {
      if (path.startsWith('/client-updates/parts/')) {
        chunks.push(path.split('/').at(-1)!)
        return fails ? failure('PART_INVALID', '上传失败') : json({})
      }
      if (options.method === 'POST') {
        releases.push(JSON.parse(String(options.body)))
        return json({ id: 'new-release' }, 201)
      }
      return json(catalog)
    })
    render(<ClientUpdatesPage client={fixture.client} />)
    const user = userEvent.setup()
    await screen.findByText('尚未发布客户端版本')
    await user.click(screen.getByRole('button', { name: '发布版本' }))
    await user.click(screen.getByRole('combobox', { name: '目标平台' }))
    await user.click(screen.getByRole('option', { name: 'macOS · arm64' }))
    expect(screen.getByText(/要支持「立即升级」，请上传签名应用 ZIP/)).toBeVisible()
    await user.type(screen.getByLabelText('版本号'), '1.10.0')
    await user.type(screen.getByLabelText('升级内容'), '消息显示改进')
    await user.upload(screen.getByLabelText('安装包'), new File(['abcdefg'], 'TomBot.dmg'))
    const submit = screen.getByRole('button', { name: '上传并发布' })
    expect(submit).toBeEnabled()
    // jsdom 不同步虚拟文件选择器的原生 validity，提交事件仍走真实表单和上传逻辑。
    fireEvent.submit(submit.closest('form')!)
    if (fails) {
      expect(await screen.findByRole('alert')).toHaveTextContent('上传失败')
      expect(releases).toEqual([])
      expect(chunks).toHaveLength(1)
    } else {
      await waitFor(() => expect(releases).toHaveLength(1))
      expect(chunks).toEqual(
        ['abc', 'def', 'g'].map((s) => createHash('sha256').update(s).digest('hex')),
      )
      expect(releases[0]).toEqual({
        version: '1.10.0',
        platform: 'darwin-arm64',
        extension: '.dmg',
        notes: '消息显示改进',
        size: 7,
        chunks,
      })
      expect(await screen.findByText(/已发布 macOS/)).toBeVisible()
    }
  },
)
