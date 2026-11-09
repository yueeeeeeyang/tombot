import { useEffect, useId, useRef, useState, type KeyboardEvent } from 'react'
import { Search, Settings, SlidersHorizontal } from 'lucide-react'
import { Avatar } from '../../components/Avatar'
import { Dialog } from '../../components/Dialog'
import { searchAgents, type Agent } from '../workspace/model'
import type { SettingsSection } from '../settings/settings'

interface SearchResult {
  id: string
  name: string
  preview: string
  agent?: Agent
  section?: SettingsSection
}

const SETTINGS_RESULTS = [
  {
    id: 'settings-general',
    name: '通用',
    preview: '外观、主题与语言',
    section: 'general' as const,
    keywords: '设置 通用 外观 主题 浅色 深色 语言',
  },
  {
    id: 'settings-models',
    name: '模型设置',
    preview: '模型供应商与模型参数',
    section: 'models' as const,
    keywords: '设置 模型 供应商 参数 温度 API',
  },
]

/** 提供助手、消息和设置搜索，支持方向键、回车及前九项快捷键；选中结果时由父组件导航。 */
export function SearchDialog({
  agents,
  activeAgentId,
  onClose,
  onSelect,
  onSettings,
}: {
  agents: Agent[]
  activeAgentId: string
  onClose: () => void
  onSelect: (id: string) => void
  onSettings: (section: SettingsSection) => void
}) {
  const [query, setQuery] = useState('')
  const [activeIndex, setActiveIndex] = useState(
    Math.max(
      0,
      agents.findIndex((agent) => agent.id === activeAgentId),
    ),
  )
  const listId = useId()
  const activeOption = useRef<HTMLButtonElement>(null)
  const normalizedQuery = query.trim().toLocaleLowerCase()
  const agentResults: SearchResult[] = searchAgents(agents, query).map((agent) => {
    const matchedMessage = normalizedQuery
      ? agent.messages
          .slice()
          .reverse()
          .find((message) => message.content.toLocaleLowerCase().includes(normalizedQuery))
      : undefined
    return {
      id: `agent-${agent.id}`,
      name: agent.name,
      agent,
      preview: matchedMessage
        ? `未发送：${matchedMessage.content}`
        : agent.draft
          ? `草稿：${agent.draft}`
          : agent.messages.length
            ? `未发送：${agent.messages.at(-1)!.content}`
            : agent.description || agent.group,
    }
  })
  const settingsResults: SearchResult[] = normalizedQuery
    ? SETTINGS_RESULTS.filter((result) =>
        result.keywords.toLocaleLowerCase().includes(normalizedQuery),
      )
    : []
  const results = [...agentResults, ...settingsResults]
  const selectedIndex = Math.min(activeIndex, results.length - 1)
  const selectedResult = results[selectedIndex]
  const shortcutPrefix = window.desktop?.platform === 'darwin' ? '⌘' : 'Ctrl+'

  useEffect(() => {
    activeOption.current?.scrollIntoView({ block: 'nearest' })
  }, [selectedResult?.id])

  function selectResult(result: SearchResult): void {
    if (result.agent) onSelect(result.agent.id)
    else if (result.section) onSettings(result.section)
  }

  function handleKeyDown(event: KeyboardEvent): void {
    if (event.nativeEvent.isComposing || event.keyCode === 229) return
    if ((event.metaKey || event.ctrlKey) && /^[1-9]$/.test(event.key)) {
      const result = results[Number(event.key) - 1]
      if (result) {
        event.preventDefault()
        selectResult(result)
      }
    } else if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      event.preventDefault()
      if (results.length)
        setActiveIndex(
          (selectedIndex + (event.key === 'ArrowDown' ? 1 : -1) + results.length) % results.length,
        )
    } else if (event.key === 'Enter' && selectedResult) {
      event.preventDefault()
      selectResult(selectedResult)
    }
  }

  return (
    <Dialog title="搜索" variant="search" onClose={onClose}>
      <div className="search-palette" onKeyDown={handleKeyDown}>
        <label className="search-input-row">
          <Search size={21} strokeWidth={1.7} />
          <input
            autoFocus
            role="combobox"
            aria-label="搜索内容"
            aria-autocomplete="list"
            aria-controls={listId}
            aria-expanded="true"
            aria-activedescendant={selectedResult ? `${listId}-${selectedResult.id}` : undefined}
            value={query}
            placeholder="搜索助手、聊天、设置…"
            onChange={(event) => {
              setQuery(event.target.value)
              setActiveIndex(0)
            }}
          />
        </label>
        <div className="search-results" id={listId} role="listbox" aria-label="搜索结果">
          {[
            { label: '助手', items: agentResults },
            { label: '设置', items: settingsResults },
          ].map(
            (group) =>
              group.items.length > 0 && (
                <div role="group" aria-label={group.label} key={group.label}>
                  <p className="search-section-label">{group.label}</p>
                  {group.items.map((result) => {
                    const index = results.findIndex((item) => item.id === result.id)
                    return (
                      <button
                        key={result.id}
                        id={`${listId}-${result.id}`}
                        role="option"
                        aria-selected={index === selectedIndex}
                        ref={index === selectedIndex ? activeOption : undefined}
                        tabIndex={-1}
                        className="search-result"
                        onMouseEnter={() => setActiveIndex(index)}
                        onClick={() => selectResult(result)}
                      >
                        {result.agent ? (
                          <Avatar preset={result.agent.avatar} />
                        ) : (
                          <span className="search-setting-icon">
                            {result.section === 'general' ? (
                              <Settings size={18} />
                            ) : (
                              <SlidersHorizontal size={18} />
                            )}
                          </span>
                        )}
                        <span className="search-result-copy">
                          <strong>{result.name}</strong>
                          <small>{result.preview}</small>
                        </span>
                        {index < 9 && (
                          <kbd aria-hidden="true">
                            {shortcutPrefix}
                            {index + 1}
                          </kbd>
                        )}
                      </button>
                    )
                  })}
                </div>
              ),
          )}
        </div>
        {results.length === 0 && (
          <div className="search-empty" role="status">
            <Search size={25} />
            <strong>还没有找到相关内容</strong>
            <p>试试助手名称、消息内容或设置选项。</p>
          </div>
        )}
      </div>
    </Dialog>
  )
}
