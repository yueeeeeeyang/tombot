import { act, fireEvent, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'
import { memoryBackend } from './features/desktop/memoryBackend'
import { SettingsDialog } from './features/settings/SettingsDialog'

describe('通用设置账号', () => {
  beforeEach(() => {
    memoryBackend.serverUrl = 'https://tom.example.test'
  })

  it('展示本人头像、昵称和用户名，账号卡片只提供退出登录', async () => {
    memoryBackend.userProfile = {
      username: 'settings-user',
      displayName: '设置测试用户',
      avatar: 'https://assets.example.test/avatar.png',
    }
    const user = userEvent.setup()
    render(<App />)
    await user.click(screen.getByRole('button', { name: '打开设置' }))
    const account = screen.getByRole('region', { name: '账号' })
    expect(within(account).getByText('设置测试用户')).toBeInTheDocument()
    expect(within(account).getByText('settings-user')).toBeInTheDocument()
    expect(account.querySelector('img')).toHaveAttribute('src', memoryBackend.userProfile.avatar)
    expect(within(account).getAllByRole('button')).toHaveLength(1)
    expect(within(account).getByRole('button', { name: '退出登录' })).toBeEnabled()
    expect(screen.queryByRole('button', { name: '添加账户' })).not.toBeInTheDocument()
  })

  it('没有昵称或头像时使用用户名和默认头像', async () => {
    memoryBackend.userProfile = { username: 'settings-user', displayName: '', avatar: '' }
    const user = userEvent.setup()
    render(<App />)
    await user.click(screen.getByRole('button', { name: '打开设置' }))
    const account = screen.getByRole('region', { name: '账号' })
    expect(account.querySelector('strong')).toHaveTextContent('settings-user')
    expect(account.querySelector('img')).toHaveAttribute(
      'src',
      expect.stringContaining('workspace-cat'),
    )
  })

  it('资料加载中和加载失败均明确展示状态，失败仍可退出登录', async () => {
    let reject!: (error: Error) => void
    vi.spyOn(memoryBackend, 'loadUserProfile').mockImplementation(
      () =>
        new Promise((_, fail) => {
          reject = fail
        }),
    )
    const user = userEvent.setup()
    render(<App />)
    await user.click(screen.getByRole('button', { name: '打开设置' }))
    const account = screen.getByRole('region', { name: '账号' })
    expect(within(account).getByText('正在读取账号信息…')).toBeInTheDocument()
    await act(async () => reject(new Error('资料服务不可用')))
    expect(
      within(screen.getByRole('region', { name: '通用' })).getByRole('alert'),
    ).toHaveTextContent('资料服务不可用')
    expect(within(account).getByText('暂时无法读取账号信息')).toBeInTheDocument()
    await user.click(within(account).getByRole('button', { name: '退出登录' }))
    expect(await screen.findByRole('heading', { name: '登录 TomBot' })).toBeInTheDocument()
  })

  it('退出期间禁止重复提交，完成后清空工作空间界面并返回登录页', async () => {
    let finish!: () => void
    const logout = vi.spyOn(memoryBackend, 'logout').mockImplementation(
      () =>
        new Promise<void>((resolve) => {
          finish = () => {
            memoryBackend.phase = 'login'
            resolve()
          }
        }),
    )
    const user = userEvent.setup()
    render(<App />)
    await user.click(screen.getByRole('button', { name: '打开设置' }))
    const button = screen.getByRole('button', { name: '退出登录' })
    await user.dblClick(button)
    fireEvent.click(button)
    expect(screen.getByRole('button', { name: '正在退出…' })).toBeDisabled()
    expect(logout).toHaveBeenCalledTimes(1)
    await act(async () => finish())
    expect(screen.getByRole('heading', { name: '登录 TomBot' })).toBeInTheDocument()
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(screen.queryByRole('textbox', { name: '给小黑发消息' })).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '在浏览器中登录' }))
    expect(await screen.findByRole('textbox', { name: '给小黑发消息' })).toBeInTheDocument()
  })

  it('退出失败且会话仍有效时保留设置、展示错误并允许重试', async () => {
    const logout = vi
      .spyOn(memoryBackend, 'logout')
      .mockRejectedValueOnce(new Error('注销暂时失败'))
    const user = userEvent.setup()
    render(<App />)
    await user.click(screen.getByRole('button', { name: '打开设置' }))
    await user.click(screen.getByRole('button', { name: '退出登录' }))
    expect(
      within(screen.getByRole('region', { name: '通用' })).getByRole('alert'),
    ).toHaveTextContent('注销暂时失败')
    expect(screen.getByRole('button', { name: '退出登录' })).toBeEnabled()
    await user.click(screen.getByRole('button', { name: '退出登录' }))
    expect(await screen.findByRole('heading', { name: '登录 TomBot' })).toBeInTheDocument()
    expect(logout).toHaveBeenCalledTimes(2)
  })

  it('服务端注销失败但本地凭据已清理时返回登录页并保留失败提示', async () => {
    vi.spyOn(memoryBackend, 'logout').mockImplementation(async () => {
      memoryBackend.phase = 'login'
      throw new Error('服务端暂时不可达，本地已退出')
    })
    const user = userEvent.setup()
    render(<App />)
    await user.click(screen.getByRole('button', { name: '打开设置' }))
    await user.click(screen.getByRole('button', { name: '退出登录' }))
    expect(await screen.findByRole('heading', { name: '登录 TomBot' })).toBeInTheDocument()
    expect(screen.getByRole('alert')).toHaveTextContent('服务端暂时不可达，本地已退出')
    expect(screen.queryByRole('textbox', { name: '给小黑发消息' })).not.toBeInTheDocument()
  })

  it('退出后无法读取会话时显示重连入口，不继续显示已登录工作空间', async () => {
    vi.spyOn(memoryBackend, 'session').mockRejectedValue(new Error('会话检查失败'))
    const user = userEvent.setup()
    render(<App />)
    await user.click(screen.getByRole('button', { name: '打开设置' }))
    await user.click(screen.getByRole('button', { name: '退出登录' }))
    expect(await screen.findByRole('heading', { name: '无法连接服务' })).toBeInTheDocument()
    expect(screen.getByRole('alert')).toHaveTextContent('会话检查失败')
    expect(screen.getByRole('button', { name: '重新连接' })).toBeInTheDocument()
  })
})

it('更新页展示实际客户端与服务端信息，失败可重试并返回通用', async () => {
  const load = vi
    .spyOn(memoryBackend, 'loadAbout')
    .mockResolvedValueOnce({
      clientVersion: '1.2.3',
      serverUrl: 'https://tom.example.test',
      serverVersion: '',
      serverError: '版本查询失败',
    })
    .mockResolvedValue({
      clientVersion: '1.2.3',
      serverUrl: 'https://tom.example.test',
      serverVersion: '2.3.4',
    })
  const user = userEvent.setup()
  render(<App />)
  await user.click(screen.getByRole('button', { name: '打开设置' }))
  await user.click(screen.getByRole('button', { name: '更新' }))
  const pane = screen.getByRole('region', { name: '更新' })
  expect(await within(pane).findByText('1.2.3')).toBeInTheDocument()
  expect(within(pane).getByText('https://tom.example.test')).toBeInTheDocument()
  expect(within(pane).getByRole('alert')).toHaveTextContent('版本查询失败')
  expect(pane.querySelector('h2')?.nextElementSibling).toContainElement(
    within(pane).getByRole('alert'),
  )
  await user.click(
    within(within(pane).getByRole('alert')).getByRole('button', { name: '重试读取版本' }),
  )
  expect(await within(pane).findByText('2.3.4')).toBeInTheDocument()
  expect(load).toHaveBeenCalledTimes(2)
  await user.click(screen.getByRole('button', { name: '通用' }))
  expect(screen.getByRole('combobox', { name: '主题' })).toBeInTheDocument()
})

it('通用设置的多项错误同时放在标题下方、账号与外观内容上方', () => {
  render(
    <SettingsDialog
      theme="light"
      themeError="主题保存失败"
      onThemeChange={vi.fn()}
      language="zh-CN"
      languageError="语言保存失败"
      onLanguageChange={vi.fn()}
      profileError="资料读取失败"
      isSigningOut={false}
      signOutError="退出登录失败"
      onSignOut={vi.fn().mockResolvedValue(undefined)}
      onClose={vi.fn()}
    />,
  )
  const pane = screen.getByRole('region', { name: '通用' })
  const errors = within(pane).getAllByRole('alert')
  expect(errors.map((error) => error.textContent)).toEqual([
    '资料读取失败',
    '退出登录失败',
    '主题保存失败',
    '语言保存失败',
  ])
  const notices = within(pane).getByRole('heading', { name: '通用' }).nextElementSibling
  for (const error of errors) expect(notices).toContainElement(error)
  expect(notices?.nextElementSibling).toBe(within(pane).getByRole('region', { name: '账号' }))
})
