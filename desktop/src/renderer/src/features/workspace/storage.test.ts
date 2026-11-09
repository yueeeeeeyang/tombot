import { describe, expect, it, vi } from 'vitest'
import { createWorkspace, updateDraft } from './model'
import { loadWorkspace, persistWorkspace, STORAGE_KEY } from './storage'

describe('本地存储', () => {
  it('首次启动创建工作空间，保存后可完整恢复中文草稿', () => {
    const workspace = updateDraft(loadWorkspace(localStorage), 'assistant', '下次继续这个想法')
    persistWorkspace(localStorage, workspace)
    expect(loadWorkspace(localStorage)).toEqual(workspace)
  })

  it.each(['不是 JSON', '{"version":99}', '{"version":1,"agents":[]}'])(
    '损坏内容不得被覆盖：%s',
    (serialized) => {
      localStorage.setItem(STORAGE_KEY, serialized)
      expect(() => loadWorkspace(localStorage)).toThrow('原始内容已保留')
      expect(localStorage.getItem(STORAGE_KEY)).toBe(serialized)
    },
  )

  it('读取权限错误明确上报', () => {
    const storage = {
      getItem: () => {
        throw new Error('读取失败')
      },
      setItem: vi.fn(),
    }
    expect(() => loadWorkspace(storage)).toThrow('无法读取')
    expect(storage.setItem).not.toHaveBeenCalled()
  })

  it('写入容量错误明确上报且旧内容保持可读', () => {
    persistWorkspace(localStorage, createWorkspace())
    const original = localStorage.getItem(STORAGE_KEY)
    const storage = {
      getItem: localStorage.getItem.bind(localStorage),
      setItem: () => {
        throw new DOMException('空间不足', 'QuotaExceededError')
      },
    }
    expect(() =>
      persistWorkspace(storage, updateDraft(createWorkspace(), 'assistant', '新内容')),
    ).toThrow('保存失败')
    expect(localStorage.getItem(STORAGE_KEY)).toBe(original)
  })

  it('拒绝超过总字节上限的内容，写入前检查容量', () => {
    const workspace = createWorkspace()
    workspace.agents[0]!.messages = Array.from({ length: 200 }, (_, index) => ({
      id: `message-${index}`,
      content: '字'.repeat(10000),
      createdAt: '2026-11-09T07:00:00.000Z',
    }))
    const storage = { getItem: vi.fn(), setItem: vi.fn() }
    expect(() => persistWorkspace(storage, workspace)).toThrow('空间已满')
    expect(storage.setItem).not.toHaveBeenCalled()
  })
})
