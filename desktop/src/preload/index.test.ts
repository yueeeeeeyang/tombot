import { contextBridge, ipcRenderer } from 'electron'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type {
  DesktopBackend,
  SessionSnapshot,
  UserEvent,
} from '../renderer/src/features/desktop/backend'

vi.mock('electron', async () => {
  const { EventEmitter } = await import('node:events')
  return {
    contextBridge: { exposeInMainWorld: vi.fn() },
    ipcRenderer: Object.assign(new EventEmitter(), { invoke: vi.fn(), send: vi.fn() }),
  }
})

const watchChannel = 'desktop:watch-events'
const userEventChannel = 'desktop:user-event'
const ended = { ok: true, value: null }
const disconnected = {
  ok: false,
  code: 'EVENT_STREAM_UNAVAILABLE',
  message: '实时状态连接中断。',
}
const refreshEvent: UserEvent = {
  sequence: 3,
  kind: 'workspace.refresh',
  resource_id: '',
  payload: {},
  created_at: '2026-11-18T00:00:00Z',
}

describe('共享用户事件订阅', () => {
  let watches: Array<{ id: string; needsRefresh: boolean; finish: (result: unknown) => void }>
  let session: SessionSnapshot
  let backend: DesktopBackend

  beforeEach(async () => {
    vi.resetModules()
    vi.mocked(contextBridge.exposeInMainWorld).mockClear()
    vi.mocked(ipcRenderer.send).mockClear()
    ipcRenderer.removeAllListeners()
    watches = []
    session = { phase: 'ready' }
    vi.mocked(ipcRenderer.invoke).mockImplementation(
      (channel, watchId: string, needsRefresh: boolean) => {
        if (channel === 'desktop:session') return Promise.resolve({ ok: true, value: session })
        if (channel === watchChannel) {
          return new Promise((finish) => watches.push({ id: watchId, needsRefresh, finish }))
        }
        throw new Error(`测试收到未知的 IPC 请求：${channel}`)
      },
    )
    await import('./index')
    backend = vi.mocked(contextBridge.exposeInMainWorld).mock.calls[0]![1].backend
  })

  it('同一窗口的监听共享连接和消息，最后一个监听负责取消', () => {
    const first = vi.fn()
    const second = vi.fn()
    const stopFirst = backend.watchEvents!(first, vi.fn())
    const stopSecond = backend.watchEvents!(second, vi.fn())
    expect(watches).toHaveLength(1)
    expect(ipcRenderer.listenerCount(userEventChannel)).toBe(1)

    const watchId = watches[0]!.id
    ipcRenderer.emit(userEventChannel, {}, refreshEvent, watchId)
    expect(first).toHaveBeenCalledExactlyOnceWith(refreshEvent)
    expect(second).toHaveBeenCalledExactlyOnceWith(refreshEvent)
    stopFirst()
    stopFirst()
    expect(ipcRenderer.send).not.toHaveBeenCalled()
    ipcRenderer.emit(userEventChannel, {}, refreshEvent, watchId)
    expect(first).toHaveBeenCalledOnce()
    expect(second).toHaveBeenCalledTimes(2)

    stopSecond()
    expect(ipcRenderer.send).toHaveBeenCalledExactlyOnceWith('desktop:unwatch-events', watchId)
    expect(ipcRenderer.listenerCount(userEventChannel)).toBe(0)
  })

  it('重试耗尽后保留监听，直到会话探测成功才启动一次新订阅', async () => {
    const onEvent = vi.fn()
    const onError = vi.fn()
    const stop = backend.watchEvents!(onEvent, onError)
    const initialWatch = watches[0]!
    initialWatch.finish(disconnected)
    await vi.waitFor(() => expect(onError).toHaveBeenCalledOnce())
    expect(onError).toHaveBeenCalledWith(expect.objectContaining({ code: disconnected.code }))
    expect(ipcRenderer.listenerCount(userEventChannel)).toBe(0)

    const lateListener = vi.fn()
    const stopLate = backend.watchEvents!(lateListener, vi.fn())
    for (const phase of ['offline', 'login'] as const) {
      session = { phase }
      await expect(backend.session()).resolves.toEqual(session)
      expect(watches).toHaveLength(1)
    }

    session = { phase: 'ready' }
    await Promise.all([backend.session(), backend.session()])
    expect(watches).toHaveLength(2)
    const restoredWatch = watches[1]!
    expect(restoredWatch.id).not.toBe(initialWatch.id)
    expect(restoredWatch.needsRefresh).toBe(true)
    ipcRenderer.emit(userEventChannel, {}, refreshEvent, initialWatch.id)
    expect(onEvent).not.toHaveBeenCalled()
    expect(lateListener).not.toHaveBeenCalled()
    ipcRenderer.emit(userEventChannel, {}, refreshEvent, restoredWatch.id)
    expect(onEvent).toHaveBeenCalledExactlyOnceWith(refreshEvent)
    expect(lateListener).toHaveBeenCalledExactlyOnceWith(refreshEvent)
    stop()
    stopLate()
  })

  it('离线后首个成功探测会结束尚未退出的旧连接，后续成功探测不重复重连', async () => {
    const onEvent = vi.fn()
    const onError = vi.fn()
    const stop = backend.watchEvents!(onEvent, onError)
    const oldWatch = watches[0]!
    expect(oldWatch.needsRefresh).toBe(false)

    session = { phase: 'offline' }
    await backend.session()
    expect(watches).toHaveLength(1)
    expect(ipcRenderer.send).not.toHaveBeenCalled()

    session = { phase: 'ready' }
    await Promise.all([backend.session(), backend.session()])
    expect(watches).toHaveLength(2)
    const restoredWatch = watches[1]!
    expect(restoredWatch.needsRefresh).toBe(true)
    expect(ipcRenderer.send).toHaveBeenCalledExactlyOnceWith('desktop:unwatch-events', oldWatch.id)
    expect(vi.mocked(ipcRenderer.send).mock.invocationCallOrder[0]).toBeLessThan(
      vi.mocked(ipcRenderer.invoke).mock.invocationCallOrder.at(-1)!,
    )

    oldWatch.finish(disconnected)
    await backend.session()
    expect(watches).toHaveLength(2)
    expect(onError).not.toHaveBeenCalled()
    ipcRenderer.emit(userEventChannel, {}, refreshEvent, oldWatch.id)
    ipcRenderer.emit(userEventChannel, {}, refreshEvent, restoredWatch.id)
    expect(onEvent).toHaveBeenCalledExactlyOnceWith(refreshEvent)
    stop()
  })

  it.each([ended, disconnected])('取消后旧订阅的结束不影响新订阅：$ok', async (result) => {
    const firstError = vi.fn()
    backend.watchEvents!(vi.fn(), firstError)()
    const oldWatch = watches[0]!
    const onEvent = vi.fn()
    const onError = vi.fn()
    const stop = backend.watchEvents!(onEvent, onError)
    const nextWatch = watches[1]!
    oldWatch.finish(result)
    await backend.session()
    expect(watches).toHaveLength(2)
    expect(firstError).not.toHaveBeenCalled()
    expect(onError).not.toHaveBeenCalled()
    ipcRenderer.emit(userEventChannel, {}, refreshEvent, nextWatch.id)
    expect(onEvent).toHaveBeenCalledExactlyOnceWith(refreshEvent)
    stop()
  })
})
