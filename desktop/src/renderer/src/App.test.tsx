import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import App from './App'
import { AGENT_ACTIVITY_EVENT } from './features/avatars/activity'
import { loadWorkspace, STORAGE_KEY } from './features/workspace/storage'
import {
  SIDEBAR_STORAGE_KEY,
  loadSidebarPreferences,
} from './features/workspace/sidebarPreferences'

/** 通过真实按钮和菜单验证自定义选择器。 */
async function chooseOption(
  user: ReturnType<typeof userEvent.setup>,
  label: string,
  option: string,
) {
  await user.click(screen.getByRole('combobox', { name: label }))
  await user.click(screen.getByRole('option', { name: option }))
}

/** 在侧栏指定猫咪上打开右键菜单，并返回触发按钮用于校验焦点。 */
function openAgentMenu(name: string | RegExp): HTMLElement {
  const trigger = screen.getByRole('button', { name })
  fireEvent.contextMenu(trigger, { clientX: 220, clientY: 320 })
  return trigger
}

describe('客户端交互', () => {
  it('保存未发送草稿不会让猫咪进入工作中', async () => {
    const user = userEvent.setup()
    render(<App />)
    const avatar = screen.getByRole('button', { name: '小黑' }).querySelector('.avatar')
    expect(avatar).toHaveAttribute('data-pose', 'idle')
    await user.type(screen.getByRole('textbox', { name: '给小黑发消息' }), '只是草稿')
    await user.click(screen.getByRole('button', { name: '保存为本地草稿' }))
    expect(screen.getByRole('button', { name: '未发送，点击重试' })).toBeInTheDocument()
    expect(screen.queryByText(/· 未发送/)).not.toBeInTheDocument()
    expect(avatar).toHaveAttribute('data-pose', 'idle')
    expect(
      screen.getByRole('button', { name: '查看小黑资料' }).querySelector('.avatar'),
    ).toHaveAttribute('data-pose', 'idle')
  })

  it('工作中事件只改变对应猫咪的姿态，不生成回复', () => {
    render(<App />)
    const avatar = screen.getByRole('button', { name: '小黑' }).querySelector('.avatar')
    const log = screen.getByRole('log').textContent
    act(() => {
      window.dispatchEvent(
        new CustomEvent(AGENT_ACTIVITY_EVENT, {
          detail: { agentId: 'assistant', activity: 'working' },
        }),
      )
    })
    expect(avatar).toHaveAttribute('data-pose', 'working')
    expect(
      screen.getByRole('button', { name: '查看小黑资料' }).querySelector('.avatar'),
    ).toHaveAttribute('data-pose', 'working')
    expect(
      screen.getByRole('button', { name: /^游戏程序/ }).querySelector('.avatar'),
    ).toHaveAttribute('data-pose', 'idle')
    const workspaceCat = screen
      .getByRole('button', { name: '我的工作空间' })
      .querySelector('.avatar')
    expect(workspaceCat).toHaveAttribute('data-avatar', 'chinese-pastoral')
    expect(workspaceCat).toHaveAttribute('data-pose', 'idle')
    expect(workspaceCat?.querySelector('.cat-work-mark')).toBeNull()
    expect(screen.getByRole('log').textContent).toBe(log)
  })

  it('输入保存为明确未发送的消息，刷新后仍可恢复', async () => {
    const user = userEvent.setup()
    const first = render(<App />)
    await user.type(screen.getByRole('textbox', { name: '给小黑发消息' }), '一起设计客户端')
    await user.click(screen.getByRole('button', { name: '保存为本地草稿' }))
    expect(within(screen.getByRole('log')).getByText('一起设计客户端')).toBeInTheDocument()
    expect(screen.queryByText(/· 未发送/)).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '未发送，点击重试' })).toBeInTheDocument()
    expect(screen.getByRole('textbox', { name: '给小黑发消息' })).toHaveValue('')
    first.unmount()
    render(<App />)
    expect(within(screen.getByRole('log')).getByText('一起设计客户端')).toBeInTheDocument()
  })

  it('切换助手保留各自尚未提交的输入', async () => {
    const user = userEvent.setup()
    render(<App />)
    await user.type(screen.getByRole('textbox', { name: '给小黑发消息' }), '小黑的草稿')
    await user.click(screen.getByRole('button', { name: /^游戏程序/ }))
    expect(screen.getByRole('textbox', { name: '给游戏程序发消息' })).toHaveValue('')
    await user.type(screen.getByRole('textbox', { name: '给游戏程序发消息' }), '程序的草稿')
    await user.click(screen.getByRole('button', { name: '小黑' }))
    expect(screen.getByRole('textbox', { name: '给小黑发消息' })).toHaveValue('小黑的草稿')
  })

  it('中文输入法确认和 Shift+Enter 不会误提交，普通 Enter 才保存', () => {
    render(<App />)
    const input = screen.getByRole('textbox', { name: '给小黑发消息' })
    fireEvent.change(input, { target: { value: '中文输入内容' } })
    fireEvent.keyDown(input, { key: 'Enter', isComposing: true })
    fireEvent.keyDown(input, { key: 'Enter', keyCode: 229 })
    fireEvent.keyDown(input, { key: 'Enter', shiftKey: true })
    expect(within(screen.getByRole('log')).queryByText('中文输入内容')).not.toBeInTheDocument()
    fireEvent.keyDown(input, { key: 'Enter' })
    expect(within(screen.getByRole('log')).getByText('中文输入内容')).toBeInTheDocument()
  })

  it('招聘猫咪校验重复名称，失败保留输入，成功切换会话', async () => {
    const user = userEvent.setup()
    render(<App />)
    await user.click(screen.getByRole('button', { name: '打开设置' }))
    await user.click(screen.getByRole('button', { name: '模型设置' }))
    await user.type(screen.getByRole('textbox', { name: '模型名称' }), '写作模型')
    await user.click(screen.getByRole('button', { name: '关闭设置' }))
    await user.click(screen.getByRole('button', { name: '招聘猫咪' }))
    const avatars = screen.getByRole('group', { name: '预设猫咪头像' })
    expect(screen.queryByText('预设头像')).not.toBeInTheDocument()
    expect(within(avatars).getAllByRole('button')).toHaveLength(8)
    await user.click(within(avatars).getByRole('button', { name: '三花猫' }))
    await chooseOption(user, '模型', '写作模型')
    await chooseOption(user, '分组', '游戏开发')
    const nameInput = screen.getByRole('textbox', { name: '助手名称' })
    await user.type(nameInput, '小黑')
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: '招聘猫咪' }))
    expect(screen.getByRole('alert')).toHaveTextContent('名称已存在')
    expect(nameInput).toHaveValue('小黑')
    expect(screen.getByRole('combobox', { name: '模型' })).toHaveTextContent('写作模型')
    expect(within(avatars).getByRole('button', { name: '三花猫' })).toHaveAttribute(
      'aria-pressed',
      'true',
    )
    await user.clear(nameInput)
    await user.type(nameInput, '创作搭档')
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: '招聘猫咪' }))
    expect(screen.getByRole('textbox', { name: '给创作搭档发消息' })).toBeInTheDocument()
    expect(loadWorkspace(localStorage).agents.at(-1)?.name).toBe('创作搭档')
    expect(loadWorkspace(localStorage).agents.at(-1)?.avatar).toBe('calico')
    expect(loadWorkspace(localStorage).agents.at(-1)?.group).toBe('游戏开发')
    expect(
      screen.getByRole('button', { name: '查看创作搭档资料' }).querySelector('.avatar'),
    ).toHaveAttribute('data-avatar', 'calico')
    await user.click(screen.getByRole('button', { name: '查看创作搭档资料' }))
    expect(screen.getByRole('dialog')).toHaveTextContent('写作模型')
  })

  it('未配置模型时禁止招聘，可转到设置后返回且保留已填猫咪资料', async () => {
    const user = userEvent.setup()
    render(<App />)
    await user.click(screen.getByRole('button', { name: '招聘猫咪' }))
    const dialog = screen.getByRole('dialog')
    expect(within(dialog).getByRole('button', { name: '招聘猫咪' })).toBeDisabled()
    expect(screen.getByRole('combobox', { name: '模型' })).toBeDisabled()
    expect(screen.getByRole('combobox', { name: '模型' })).toHaveTextContent('暂无已配置模型')
    await user.type(screen.getByRole('textbox', { name: '助手名称' }), '专属猫咪')
    await user.click(screen.getByRole('button', { name: '布偶猫' }))
    await user.click(screen.getByRole('button', { name: '前往模型设置' }))
    await user.type(screen.getByRole('textbox', { name: '模型名称' }), 'my-custom-model')
    await user.click(screen.getByRole('button', { name: '关闭设置' }))
    expect(screen.getByRole('dialog', { name: '招聘猫咪' })).toBeInTheDocument()
    expect(screen.getByRole('textbox', { name: '助手名称' })).toHaveValue('专属猫咪')
    expect(screen.getByRole('button', { name: '布偶猫' })).toHaveAttribute('aria-pressed', 'true')
    await chooseOption(user, '模型', 'my-custom-model')
    expect(screen.getByRole('combobox', { name: '模型' })).toHaveTextContent('my-custom-model')
    await user.click(screen.getByRole('combobox', { name: '模型' }))
    expect(screen.getAllByRole('option')).toHaveLength(1)
    expect(screen.queryByRole('option', { name: '跟随工作空间设置' })).not.toBeInTheDocument()
  })

  it('搜索无结果有空状态，搜索结果可跳转', async () => {
    const user = userEvent.setup()
    render(<App />)
    await user.click(screen.getByRole('button', { name: '搜索助手和消息' }))
    const input = screen.getByRole('combobox', { name: '搜索内容' })
    await user.type(input, '不存在的搭档')
    expect(screen.getByText('还没有找到相关内容')).toBeInTheDocument()
    await user.clear(input)
    await user.type(input, '游戏音效')
    await user.click(within(screen.getByRole('dialog')).getByRole('option', { name: /游戏音效/ }))
    expect(screen.getByRole('textbox', { name: '给游戏音效发消息' })).toBeInTheDocument()
  })

  it('搜索支持方向键循环选择、数字快捷键和设置导航', async () => {
    const user = userEvent.setup()
    render(<App />)
    await user.click(screen.getByRole('button', { name: '搜索助手和消息' }))
    await user.click(screen.getByRole('combobox', { name: '搜索内容' }))
    await user.keyboard('{ArrowUp}')
    expect(screen.getByRole('option', { selected: true })).toHaveTextContent('整理助手')
    await user.keyboard('{ArrowDown}')
    expect(screen.getByRole('option', { selected: true })).toHaveTextContent('小黑')
    await user.keyboard('{Control>}2{/Control}')
    expect(screen.getByRole('textbox', { name: '给游戏程序发消息' })).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '搜索助手和消息' }))
    await user.type(screen.getByRole('combobox', { name: '搜索内容' }), '模型')
    await user.keyboard('{Enter}')
    expect(screen.getByRole('dialog', { name: '设置' })).toBeInTheDocument()
    expect(screen.getByRole('combobox', { name: '模型供应商' })).toBeInTheDocument()
  })

  it('搜索空结果及中文输入法确认不误触发导航', () => {
    render(<App />)
    fireEvent.click(screen.getByRole('button', { name: '搜索助手和消息' }))
    const input = screen.getByRole('combobox', { name: '搜索内容' })
    fireEvent.change(input, { target: { value: '没有这个结果' } })
    fireEvent.keyDown(input, { key: 'ArrowDown' })
    fireEvent.keyDown(input, { key: 'Enter' })
    expect(screen.getByRole('dialog', { name: '搜索' })).toBeInTheDocument()
    fireEvent.change(input, { target: { value: '游戏程序' } })
    fireEvent.keyDown(input, { key: 'Enter', isComposing: true })
    expect(screen.getByRole('dialog', { name: '搜索' })).toBeInTheDocument()
    fireEvent.keyDown(input, { key: 'Enter' })
    expect(screen.getByRole('textbox', { name: '给游戏程序发消息' })).toBeInTheDocument()
  })

  it('设置入口显示两项分类，主题即时应用并独立保存，语言预览在关闭重开后保留', async () => {
    const user = userEvent.setup()
    render(<App />)
    expect(screen.queryByText(/草稿仅保存在这台设备/)).not.toBeInTheDocument()
    expect(screen.queryByText('Enter 保存 · Shift + Enter 换行')).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '打开设置' }))
    const navigation = screen.getByRole('navigation', { name: '设置分类' })
    expect(within(navigation).getAllByRole('button')).toHaveLength(2)
    expect(screen.getByRole('combobox', { name: '主题' })).toHaveTextContent('跟随系统')
    await chooseOption(user, '主题', '深色')
    await chooseOption(user, '语言', '英语')
    await user.click(screen.getByRole('button', { name: '关闭设置' }))
    await user.click(screen.getByRole('button', { name: '打开设置' }))
    expect(screen.getByRole('combobox', { name: '主题' })).toHaveTextContent('深色')
    expect(screen.getByRole('combobox', { name: '语言' })).toHaveTextContent('英语')
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull()
    expect(localStorage.getItem('tombot.theme.v1')).toBe('dark')
    expect(document.documentElement).toHaveAttribute('data-theme', 'dark')
  })

  it('工作空间与齿轮打开独立弹窗，聊天页不再显示分享或收缩按钮', async () => {
    const user = userEvent.setup()
    render(<App />)
    expect(screen.queryByRole('button', { name: '分享' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '切换侧边栏' })).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '我的工作空间' }))
    expect(screen.getByRole('dialog', { name: '我的工作空间' })).toHaveTextContent('服务端连接')
    expect(screen.queryByRole('combobox', { name: '主题' })).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '关闭弹窗' }))
    await user.click(screen.getByRole('button', { name: '打开设置' }))
    expect(screen.getByRole('dialog', { name: '设置' })).toBeInTheDocument()
  })

  it('模型表单支持参数预览、边界校验及栏目切换，凭据入口保持禁用', async () => {
    const user = userEvent.setup()
    render(<App />)
    await user.click(screen.getByRole('button', { name: '打开设置' }))
    await user.click(screen.getByRole('button', { name: '模型设置' }))
    await chooseOption(user, '模型供应商', '自定义（兼容 OpenAI）')
    expect(screen.getByLabelText('API 密钥')).toBeDisabled()
    const address = screen.getByLabelText('API 地址')
    fireEvent.change(address, { target: { value: '无效地址' } })
    expect(screen.getByRole('alert')).toHaveTextContent('完整的 HTTP 或 HTTPS')
    fireEvent.change(address, { target: { value: 'https://models.example.test/api' } })
    fireEvent.change(screen.getByRole('slider', { name: '温度' }), { target: { value: '1.2' } })
    fireEvent.change(screen.getByRole('slider', { name: 'Top P' }), { target: { value: '0.85' } })
    await user.click(screen.getByRole('switch', { name: /流式响应/ }))
    expect(screen.getByRole('switch', { name: /流式响应/ })).not.toBeChecked()
    const outputLength = screen.getByRole('spinbutton', { name: /最大输出长度/ })
    for (const value of ['', '0', '131073', '2.5']) {
      fireEvent.change(outputLength, { target: { value } })
      expect(screen.getByRole('alert')).toHaveTextContent('之间的整数')
    }
    fireEvent.change(outputLength, { target: { value: '131072' } })
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '通用' }))
    await user.click(screen.getByRole('button', { name: '模型设置' }))
    expect(screen.getByRole('combobox', { name: '模型供应商' })).toHaveTextContent(
      '自定义（兼容 OpenAI）',
    )
    expect(screen.getByRole('slider', { name: '温度' })).toHaveValue('1.2')
    expect(screen.getByRole('slider', { name: 'Top P' })).toHaveValue('0.85')
    expect(screen.getByRole('spinbutton', { name: /最大输出长度/ })).toHaveValue(131072)
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull()
  })

  it('存储写入失败保留内存草稿并允许重试', () => {
    render(<App />)
    const setItem = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('写入失败')
    })
    fireEvent.change(screen.getByRole('textbox', { name: '给小黑发消息' }), {
      target: { value: '不能丢失的想法' },
    })
    expect(screen.getByRole('alert')).toHaveTextContent('保存失败')
    expect(screen.getByRole('textbox', { name: '给小黑发消息' })).toHaveValue('不能丢失的想法')
    setItem.mockRestore()
    fireEvent.click(screen.getByRole('button', { name: '重试保存' }))
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(loadWorkspace(localStorage).agents[0]?.draft).toBe('不能丢失的想法')
  })

  it('存储损坏时禁止输入，不覆盖原内容', () => {
    localStorage.setItem(STORAGE_KEY, '损坏内容')
    render(<App />)
    expect(screen.getByRole('alert')).toHaveTextContent('原始内容已保留')
    expect(screen.getByRole('textbox', { name: '给小黑发消息' })).toBeDisabled()
    expect(localStorage.getItem(STORAGE_KEY)).toBe('损坏内容')
  })

  it('剪贴板失败明确提示，空会话禁止导出', async () => {
    const user = userEvent.setup()
    render(<App />)
    await user.click(screen.getByRole('button', { name: '查看小黑资料' }))
    await user.click(screen.getByRole('button', { name: '导出草稿' }))
    expect(screen.getByRole('button', { name: '复制内容' })).toBeDisabled()
    await user.click(screen.getByRole('button', { name: '关闭弹窗' }))
    fireEvent.change(screen.getByRole('textbox', { name: '给小黑发消息' }), {
      target: { value: '待导出内容' },
    })
    await user.click(screen.getByRole('button', { name: '查看小黑资料' }))
    await user.click(screen.getByRole('button', { name: '导出草稿' }))
    vi.spyOn(navigator.clipboard, 'writeText').mockRejectedValue(new Error('拒绝剪贴板访问'))
    await user.click(screen.getByRole('button', { name: '复制内容' }))
    expect(screen.getByRole('alert')).toHaveTextContent('复制失败')
  })

  it('消息纯文本呈现，不执行输入中的 HTML', async () => {
    const user = userEvent.setup()
    render(<App />)
    fireEvent.change(screen.getByRole('textbox', { name: '给小黑发消息' }), {
      target: { value: '<img src=x onerror=alert(1)>' },
    })
    await user.click(screen.getByRole('button', { name: '保存为本地草稿' }))
    expect(
      within(screen.getByRole('log')).getByText('<img src=x onerror=alert(1)>'),
    ).toBeInTheDocument()
    expect(screen.getByRole('log').querySelector('img')).toBeNull()
  })

  it('右键菜单置顶、取消置顶和未读状态，默认猫咪的固定操作禁用', async () => {
    const user = userEvent.setup()
    render(<App />)
    const defaultButton = openAgentMenu('小黑')
    const defaultMenu = screen.getByRole('menu', { name: '小黑的菜单' })
    expect(within(defaultMenu).getByRole('menuitem', { name: '取消置顶' })).toBeDisabled()
    expect(within(defaultMenu).getByRole('menuitem', { name: '移至' })).toBeDisabled()
    expect(within(defaultMenu).getByRole('menuitem', { name: '从侧边栏隐藏' })).toBeDisabled()
    expect(within(defaultMenu).getByRole('menuitem', { name: '删除' })).toBeDisabled()
    await user.keyboard('{Escape}')
    expect(defaultButton).toHaveFocus()

    openAgentMenu('游戏程序')
    await user.click(screen.getByRole('menuitem', { name: '置顶' }))
    expect(screen.queryByRole('menu')).not.toBeInTheDocument()
    const pinnedArea = document.querySelector('.pinned-area') as HTMLElement
    expect(within(pinnedArea).getByRole('button', { name: '游戏程序' })).toBeInTheDocument()
    expect(
      within(document.querySelector('.agent-groups') as HTMLElement).queryByRole('button', {
        name: '游戏程序',
      }),
    ).not.toBeInTheDocument()
    expect(loadSidebarPreferences(localStorage).pinnedIds).toEqual(['developer'])

    fireEvent.contextMenu(within(pinnedArea).getByRole('button', { name: '游戏程序' }), {
      clientX: 220,
      clientY: 320,
    })
    await user.click(screen.getByRole('menuitem', { name: '取消置顶' }))
    expect(loadSidebarPreferences(localStorage).pinnedIds).toEqual([])
    expect(
      within(document.querySelector('.agent-groups') as HTMLElement).getByRole('button', {
        name: '游戏程序',
      }),
    ).toBeInTheDocument()

    openAgentMenu('游戏程序')
    await user.click(screen.getByRole('menuitem', { name: '标为未读' }))
    const unreadButton = screen.getByRole('button', { name: '游戏程序，有未读消息' })
    expect(unreadButton.querySelector('.agent-unread-dot')).toBeInTheDocument()
    expect(loadSidebarPreferences(localStorage).unreadIds).toEqual(['developer'])
    await user.click(unreadButton)
    expect(screen.getByRole('button', { name: '游戏程序' })).toBeInTheDocument()
    expect(screen.getByRole('textbox', { name: '给游戏程序发消息' })).toBeInTheDocument()
    expect(loadSidebarPreferences(localStorage).unreadIds).toEqual([])
  })

  it('右键菜单支持键盘导航、分组子菜单和关闭后恢复焦点', async () => {
    const user = userEvent.setup()
    render(<App />)
    const trigger = screen.getByRole('button', { name: '游戏程序' })
    trigger.focus()
    await user.keyboard('{Shift>}{F10}{/Shift}')
    expect(screen.getByRole('menuitem', { name: '置顶' })).toHaveFocus()
    await user.keyboard('{Escape}')
    expect(trigger).toHaveFocus()
    fireEvent.contextMenu(trigger, { clientX: 220, clientY: 320 })
    expect(screen.getByRole('menuitem', { name: '置顶' })).toHaveFocus()
    await user.keyboard('{ArrowDown}')
    expect(screen.getByRole('menuitem', { name: '移至' })).toHaveFocus()
    await user.keyboard('{ArrowRight}')
    expect(screen.getByRole('menu', { name: '移至分组' })).toBeInTheDocument()
    expect(screen.getByRole('menuitemradio', { name: '游戏开发' })).toHaveFocus()
    await user.keyboard('{Escape}')
    expect(screen.queryByRole('menu', { name: '移至分组' })).not.toBeInTheDocument()
    expect(screen.getByRole('menuitem', { name: '移至' })).toHaveFocus()
    await user.keyboard('{Escape}')
    expect(screen.queryByRole('menu')).not.toBeInTheDocument()
    expect(trigger).toHaveFocus()
  })

  it('右键菜单复制对话 ID，剪贴板失败时明确提示', async () => {
    const user = userEvent.setup()
    render(<App />)
    const writeText = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue(undefined)
    openAgentMenu('游戏程序')
    await user.click(screen.getByRole('menuitem', { name: '复制对话 ID' }))
    expect(writeText).toHaveBeenCalledWith('developer')
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('已复制'))
    writeText.mockRejectedValueOnce(new Error('拒绝剪贴板访问'))
    openAgentMenu('游戏程序')
    await user.click(screen.getByRole('menuitem', { name: '复制对话 ID' }))
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('复制对话 ID 失败'))
  })

  it('右键菜单移动分组、重命名并删除活动会话', async () => {
    const user = userEvent.setup()
    render(<App />)
    openAgentMenu('游戏负责人 + 策划')
    await user.click(screen.getByRole('menuitem', { name: '移至' }))
    expect(screen.getByRole('menu', { name: '移至分组' })).toBeInTheDocument()
    await user.click(screen.getByRole('menuitemradio', { name: '未分组' }))
    expect(screen.queryByRole('menu')).not.toBeInTheDocument()
    expect(loadWorkspace(localStorage).agents.find((item) => item.id === 'producer')?.group).toBe(
      '未分组',
    )

    openAgentMenu('游戏负责人 + 策划')
    await user.click(screen.getByRole('menuitem', { name: '重命名猫咪' }))
    const nameInput = screen.getByRole('textbox', { name: '猫咪名称' })
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(nameInput).toHaveFocus()
    expect(nameInput).toHaveValue('游戏负责人 + 策划')
    await user.clear(nameInput)
    await user.type(nameInput, '游戏程序')
    await user.keyboard('{Enter}')
    expect(screen.getByRole('alert')).toHaveTextContent('名称已存在')
    expect(screen.getByRole('textbox', { name: '猫咪名称' })).toHaveValue('游戏程序')
    await user.keyboard('{Escape}')
    expect(screen.queryByRole('textbox', { name: '猫咪名称' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '游戏负责人 + 策划' })).toBeInTheDocument()

    openAgentMenu('游戏负责人 + 策划')
    await user.click(screen.getByRole('menuitem', { name: '重命名猫咪' }))
    const nextNameInput = screen.getByRole('textbox', { name: '猫咪名称' })
    await user.clear(nextNameInput)
    await user.type(nextNameInput, '制作人')
    await user.keyboard('{Enter}')
    expect(screen.queryByRole('textbox', { name: '猫咪名称' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '制作人' })).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(loadWorkspace(localStorage).agents.find((item) => item.id === 'producer')?.name).toBe(
      '制作人',
    )

    await user.click(screen.getByRole('button', { name: '制作人' }))
    expect(screen.getByRole('textbox', { name: '给制作人发消息' })).toBeInTheDocument()
    openAgentMenu('制作人')
    await user.click(screen.getByRole('menuitem', { name: '置顶' }))
    openAgentMenu('制作人')
    await user.click(screen.getByRole('menuitem', { name: '删除' }))
    const deleteDialog = screen.getByRole('dialog', { name: '删除猫咪' })
    expect(deleteDialog).toHaveTextContent('制作人')
    await user.click(within(deleteDialog).getByRole('button', { name: '删除猫咪' }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '制作人' })).not.toBeInTheDocument()
    expect(screen.getByRole('textbox', { name: '给小黑发消息' })).toBeInTheDocument()
    expect(loadWorkspace(localStorage).agents.some((item) => item.id === 'producer')).toBe(false)
    expect(loadSidebarPreferences(localStorage).pinnedIds).toEqual([])
    expect(
      within(document.querySelector('.pinned-area') as HTMLElement).getAllByRole('button'),
    ).toHaveLength(1)
  })

  it('隐藏活动会话后回到默认猫咪，并可在工作空间恢复', async () => {
    const user = userEvent.setup()
    render(<App />)
    await user.click(screen.getByRole('button', { name: '游戏程序' }))
    expect(screen.getByRole('textbox', { name: '给游戏程序发消息' })).toBeInTheDocument()
    openAgentMenu('游戏程序')
    await user.click(screen.getByRole('menuitem', { name: '从侧边栏隐藏' }))
    expect(screen.queryByRole('button', { name: '游戏程序' })).not.toBeInTheDocument()
    expect(screen.getByRole('textbox', { name: '给小黑发消息' })).toBeInTheDocument()
    expect(loadSidebarPreferences(localStorage).hiddenIds).toEqual(['developer'])
    expect(screen.getByRole('alert')).toHaveTextContent('已从侧边栏隐藏')

    await user.click(screen.getByRole('button', { name: '我的工作空间' }))
    const workspaceDialog = screen.getByRole('dialog', { name: '我的工作空间' })
    expect(
      within(workspaceDialog).getByRole('region', { name: '已隐藏的助手' }),
    ).toBeInTheDocument()
    await user.click(within(workspaceDialog).getByRole('button', { name: '恢复显示游戏程序' }))
    expect(screen.getByRole('button', { name: '游戏程序' })).toBeInTheDocument()
    expect(loadSidebarPreferences(localStorage).hiddenIds).toEqual([])
    expect(
      within(workspaceDialog).queryByRole('region', { name: '已隐藏的助手' }),
    ).not.toBeInTheDocument()
  })

  it('侧栏偏好损坏时提示并保留原文，展示操作不会覆盖', async () => {
    const user = userEvent.setup()
    localStorage.setItem(SIDEBAR_STORAGE_KEY, '损坏内容')
    render(<App />)
    expect(screen.getByRole('alert')).toHaveTextContent('格式异常')
    openAgentMenu('游戏程序')
    await user.click(screen.getByRole('menuitem', { name: '标为未读' }))
    expect(screen.getByRole('alert')).toHaveTextContent('格式异常')
    expect(localStorage.getItem(SIDEBAR_STORAGE_KEY)).toBe('损坏内容')
  })

  it('右键创建分组后直接内联命名并移入猫咪', async () => {
    const user = userEvent.setup()
    render(<App />)
    openAgentMenu('游戏负责人 + 策划')
    await user.click(screen.getByRole('menuitem', { name: '移至' }))
    await user.click(screen.getByRole('menuitem', { name: '创建分组' }))
    expect(screen.queryByRole('menu')).not.toBeInTheDocument()
    const groupInput = screen.getByRole('textbox', { name: '分组名称' })
    expect(groupInput).toHaveFocus()
    expect(groupInput).toHaveValue('新分组')
    const section = groupInput.closest('section') as HTMLElement
    expect(within(section).getByRole('button', { name: '游戏负责人 + 策划' })).toBeInTheDocument()
    expect(loadWorkspace(localStorage).agents.find((item) => item.id === 'producer')?.group).toBe(
      '新分组',
    )

    await user.clear(groupInput)
    await user.type(groupInput, '制作组')
    await user.keyboard('{Enter}')
    expect(screen.queryByRole('textbox', { name: '分组名称' })).not.toBeInTheDocument()
    expect(screen.getByRole('heading', { name: '制作组' })).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(loadWorkspace(localStorage).agents.find((item) => item.id === 'producer')?.group).toBe(
      '制作组',
    )

    openAgentMenu('游戏程序')
    await user.click(screen.getByRole('menuitem', { name: '移至' }))
    await user.click(screen.getByRole('menuitem', { name: '创建分组' }))
    const secondGroupInput = screen.getByRole('textbox', { name: '分组名称' })
    expect(secondGroupInput).toHaveValue('新分组')
    await user.clear(secondGroupInput)
    await user.type(secondGroupInput, '制作组')
    await user.keyboard('{Enter}')
    expect(screen.getByRole('alert')).toHaveTextContent('分组名称已存在')
    expect(screen.getByRole('textbox', { name: '分组名称' })).toHaveValue('制作组')
    await user.keyboard('{Escape}')
    expect(screen.getByRole('heading', { name: '新分组' })).toBeInTheDocument()
    expect(loadWorkspace(localStorage).agents.find((item) => item.id === 'developer')?.group).toBe(
      '新分组',
    )
  })

  it('置顶猫咪在原位重命名，取消后恢复原名', async () => {
    const user = userEvent.setup()
    render(<App />)
    openAgentMenu('游戏程序')
    await user.click(screen.getByRole('menuitem', { name: '置顶' }))
    const pinnedArea = document.querySelector('.pinned-area') as HTMLElement
    fireEvent.contextMenu(within(pinnedArea).getByRole('button', { name: '游戏程序' }), {
      clientX: 220,
      clientY: 320,
    })
    await user.click(screen.getByRole('menuitem', { name: '重命名猫咪' }))
    const pinnedInput = within(pinnedArea).getByRole('textbox', { name: '猫咪名称' })
    expect(pinnedInput).toHaveFocus()
    await user.clear(pinnedInput)
    await user.type(pinnedInput, '程序猫')
    await user.keyboard('{Enter}')
    expect(within(pinnedArea).getByRole('button', { name: '程序猫' })).toBeInTheDocument()
    expect(loadWorkspace(localStorage).agents.find((item) => item.id === 'developer')?.name).toBe(
      '程序猫',
    )

    fireEvent.contextMenu(within(pinnedArea).getByRole('button', { name: '程序猫' }), {
      clientX: 220,
      clientY: 320,
    })
    await user.click(screen.getByRole('menuitem', { name: '重命名猫咪' }))
    const cancelInput = within(pinnedArea).getByRole('textbox', { name: '猫咪名称' })
    await user.clear(cancelInput)
    await user.type(cancelInput, '临时名称')
    await user.keyboard('{Escape}')
    expect(within(pinnedArea).getByRole('button', { name: '程序猫' })).toBeInTheDocument()

    fireEvent.contextMenu(within(pinnedArea).getByRole('button', { name: '程序猫' }), {
      clientX: 220,
      clientY: 320,
    })
    await user.click(screen.getByRole('menuitem', { name: '移至' }))
    await user.click(screen.getByRole('menuitem', { name: '创建分组' }))
    expect(within(pinnedArea).queryByRole('button', { name: '程序猫' })).not.toBeInTheDocument()
    const groupInput = screen.getByRole('textbox', { name: '分组名称' })
    expect(groupInput).toHaveFocus()
    expect(
      within(groupInput.closest('section') as HTMLElement).getByRole('button', {
        name: '程序猫',
      }),
    ).toBeInTheDocument()
    expect(loadSidebarPreferences(localStorage).pinnedIds).toEqual([])
  })
})
