import type { CatTask } from '../../../../shared/tasks'
import { X } from 'lucide-react'
import { useEffect, useState, type KeyboardEvent as ReactKeyboardEvent } from 'react'
import { Avatar } from '../../components/Avatar'
import type { Agent } from '../workspace/model'
import { ChatLibrary } from './ChatLibrary'
import { ComputerPane } from './ComputerPane'
import { getBackend } from '../desktop/backend'
import { TaskPane } from './TaskPane'

const PANEL_TABS = ['activity', 'library'] as const
type PanelTab = (typeof PANEL_TABS)[number]
const TAB_LABELS: Record<PanelTab, string> = {
  activity: '动态',
  library: '资料库',
}

/** 会话右侧的猫咪面板；Esc 关闭，但下拉菜单或弹窗先处理自己的 Esc。 */
export function CatSidePanel({
  agent,
  isLead = false,
  onClose,
  onComputerUrl,
  onConnectComputer,
  onPrepareComputer,
}: {
  agent: Agent
  isLead?: boolean
  onClose: () => void
  onComputerUrl: (computerUrl: string) => void
  onConnectComputer?: () => void
  onPrepareComputer?: () => void
}) {
  const [tab, setTab] = useState<PanelTab>('activity')

  useEffect(() => {
    function onKeyDown(event: globalThis.KeyboardEvent): void {
      if (event.key !== 'Escape' || event.defaultPrevented) return
      // 下拉和弹窗自己消费 Esc；收起的列表仍留在文档里，不能据此挡住面板关闭。
      if (document.querySelector('dialog[open], [role="listbox"]:not([hidden])')) return
      onClose()
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [onClose])

  function moveTab(event: ReactKeyboardEvent<HTMLButtonElement>): void {
    if (event.key !== 'ArrowRight' && event.key !== 'ArrowLeft') return
    event.preventDefault()
    const index = PANEL_TABS.indexOf(tab)
    const next =
      event.key === 'ArrowRight'
        ? PANEL_TABS[(index + 1) % PANEL_TABS.length]!
        : PANEL_TABS[(index + PANEL_TABS.length - 1) % PANEL_TABS.length]!
    setTab(next)
    event.currentTarget.parentElement?.querySelector<HTMLButtonElement>(`#cat-tab-${next}`)?.focus()
  }

  return (
    <aside className="cat-panel" aria-label={agent.name}>
      <button
        type="button"
        className="cat-panel-close round-button"
        aria-label="关闭猫咪面板"
        onClick={onClose}
      >
        <X size={18} strokeWidth={1.7} />
      </button>
      <div className="cat-panel-hero">
        <Avatar
          preset={agent.avatar}
          customAvatar={agent.customAvatar}
          agentId={agent.id}
          size="large"
          starred={isLead}
        />
        <h2>{agent.name}</h2>
      </div>
      <div className="cat-panel-tabs" role="tablist" aria-label="猫咪栏目">
        {PANEL_TABS.map((item) => (
          <button
            key={item}
            id={`cat-tab-${item}`}
            type="button"
            role="tab"
            aria-selected={tab === item}
            aria-controls={`cat-panel-${item}`}
            tabIndex={tab === item ? 0 : -1}
            onClick={() => setTab(item)}
            onKeyDown={moveTab}
          >
            {TAB_LABELS[item]}
          </button>
        ))}
      </div>
      {tab === 'activity' && (
        <div
          className="cat-panel-body"
          role="tabpanel"
          id="cat-panel-activity"
          aria-labelledby="cat-tab-activity"
        >
          <section className="cat-panel-section" aria-labelledby="cat-computer-heading">
            <h3 id="cat-computer-heading">电脑</h3>
            <ComputerPane
              key={`${agent.id}:${agent.computerUrl}`}
              agentName={agent.name}
              computerUrl={agent.computerUrl}
              onSave={onComputerUrl}
              onConnect={onConnectComputer}
              onPrepare={onPrepareComputer}
            />
          </section>
          <section className="cat-panel-section" aria-labelledby="cat-task-heading">
            <h3 id="cat-task-heading">任务</h3>
            <TaskPane
              key={agent.id}
              agentId={agent.id}
              onLoad={loadTasks}
              onToggle={toggleTask}
              onDelete={deleteTask}
              onWatch={watchTasks}
            />
          </section>
        </div>
      )}
      {tab === 'library' && (
        <div
          className="cat-panel-body"
          role="tabpanel"
          id="cat-panel-library"
          aria-labelledby="cat-tab-library"
        >
          <ChatLibrary conversationId={agent.conversationId ?? ''} />
        </div>
      )}
    </aside>
  )
}

// 回调保持稳定，聊天消息更新不应重新发起整组任务请求。
function loadTasks(agentId: string): Promise<CatTask[]> {
  const backend = getBackend()
  return backend.listTasks
    ? backend.listTasks(agentId)
    : Promise.reject(new Error('当前服务不支持读取任务。'))
}

function toggleTask(task: CatTask): Promise<void> {
  const backend = getBackend()
  return backend.setTaskEnabled
    ? backend.setTaskEnabled(task.bot_id, task.id, task.revision, !task.is_enabled)
    : Promise.reject(new Error('当前服务不支持修改任务。'))
}

function deleteTask(task: CatTask): Promise<void> {
  const backend = getBackend()
  return backend.deleteTask
    ? backend.deleteTask(task.bot_id, task.id, task.revision)
    : Promise.reject(new Error('当前服务不支持删除任务。'))
}

function watchTasks(agentId: string, onChange: () => void): () => void {
  return (
    getBackend().watchEvents?.((event) => {
      if (
        event.kind === 'workspace.refresh' ||
        (event.payload.bot_id === agentId &&
          (event.kind === 'task.updated' || event.kind === 'task.run.finished'))
      )
        onChange()
    }, onChange) ?? (() => {})
  )
}
