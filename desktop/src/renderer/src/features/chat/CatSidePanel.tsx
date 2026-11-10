import { Download, X } from 'lucide-react'
import { useEffect, useState, type KeyboardEvent as ReactKeyboardEvent } from 'react'
import { Avatar } from '../../components/Avatar'
import { Select, type SelectOption } from '../../components/Select'
import { catModelOptions } from '../models/catalog'
import type { Agent } from '../workspace/model'
import { ArtifactLibrary } from './ArtifactLibrary'
import { ComputerPane } from './ComputerPane'

const PANEL_TABS = ['details', 'library', 'computer'] as const
type PanelTab = (typeof PANEL_TABS)[number]
function modelOptions(modelId: string): readonly SelectOption[] {
  const catalog = catModelOptions()
  if (catalog.some((option) => option.value === modelId)) return catalog
  return [{ value: modelId, label: modelId }, ...catalog]
}

const TAB_LABELS: Record<PanelTab, string> = {
  details: '详情',
  library: '资料库',
  computer: '电脑',
}

/** 会话右侧的猫咪面板；Esc 关闭，但下拉菜单或弹窗先处理自己的 Esc。 */
export function CatSidePanel({
  agent,
  onClose,
  onModelChange,
  onComputerUrl,
  onConnectComputer,
  onExport,
  now,
}: {
  agent: Agent
  onClose: () => void
  onModelChange: (modelId: string) => void
  onComputerUrl: (computerUrl: string) => void
  onConnectComputer?: () => void
  onExport: () => void
  now?: Date
}) {
  const [tab, setTab] = useState<PanelTab>('details')

  useEffect(() => {
    function onKeyDown(event: globalThis.KeyboardEvent): void {
      if (event.key !== 'Escape') return
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
          agentId={agent.id}
          size="large"
          starred={agent.id === 'assistant'}
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
      {tab === 'details' && (
        <div
          className="cat-panel-body"
          role="tabpanel"
          id="cat-panel-details"
          aria-labelledby="cat-tab-details"
        >
          <section className="cat-panel-card">
            <span>名称</span>
            <strong>{agent.name}</strong>
          </section>
          <section className="cat-panel-card">
            <span>介绍</span>
            <strong>{agent.description || '还没有介绍'}</strong>
          </section>
          <section className="cat-panel-card">
            <span>分组</span>
            <strong>{agent.group}</strong>
          </section>
          <section className="cat-panel-card">
            <span>模型</span>
            <Select
              label="模型"
              value={agent.modelId}
              options={modelOptions(agent.modelId)}
              onChange={onModelChange}
            />
            <small>选择会保存到服务。回复时使用这只猫咪自己的模型。</small>
          </section>
          <button type="button" className="secondary-button cat-panel-export" onClick={onExport}>
            <Download size={15} />
            导出草稿
          </button>
        </div>
      )}
      {tab === 'library' && (
        <div
          className="cat-panel-body"
          role="tabpanel"
          id="cat-panel-library"
          aria-labelledby="cat-tab-library"
        >
          <ArtifactLibrary artifacts={agent.artifacts} now={now} />
        </div>
      )}
      {tab === 'computer' && (
        <div
          className="cat-panel-body"
          role="tabpanel"
          id="cat-panel-computer"
          aria-labelledby="cat-tab-computer"
        >
          <ComputerPane
            key={agent.computerUrl}
            agentName={agent.name}
            computerUrl={agent.computerUrl}
            onSave={onComputerUrl}
            onConnect={onConnectComputer}
          />
        </div>
      )}
    </aside>
  )
}
