import { useRef, useState } from 'react'
import { Avatar } from '../../components/Avatar'
import type { Agent } from '../workspace/model'

/** 提及选择使用成员标识；正文保持自然的 @名称，方向键与回车可完成选择。 */
export function GroupComposer({
  title,
  draft,
  members,
  onChange,
  onSend,
}: {
  title: string
  draft: string
  members: Agent[]
  onChange: (value: string, mentions: string[]) => void
  onSend: () => void
}) {
  const input = useRef<HTMLTextAreaElement>(null)
  const [query, setQuery] = useState<{ start: number; end: number; text: string } | null>(null)
  const [active, setActive] = useState(0)
  const [selected, setSelected] = useState<Array<{ id: string; name: string }>>([])
  const matches = query
    ? members.filter((cat) => cat.name.toLocaleLowerCase().includes(query.text.toLocaleLowerCase()))
    : []
  function update(value: string, position: number) {
    const chosen = selected.filter((cat) => value.includes(`@${cat.name}`))
    setSelected(chosen)
    onChange(
      value,
      chosen.map((cat) => cat.id),
    )
    const prefix = value.slice(0, position)
    const match = /(?:^|[^a-zA-Z0-9._%+-])@([^@\n]*)$/u.exec(prefix)
    setQuery(match ? { start: prefix.lastIndexOf('@'), end: position, text: match[1]! } : null)
    setActive(0)
  }
  function select(cat: Agent) {
    if (!query) return
    const chosen = [
      ...selected.filter((item) => item.id !== cat.id),
      { id: cat.id, name: cat.name },
    ]
    const value = draft.slice(0, query.start) + `@${cat.name} ` + draft.slice(query.end)
    setSelected(chosen)
    onChange(
      value,
      chosen.map((item) => item.id),
    )
    setQuery(null)
    const position = query.start + cat.name.length + 2
    requestAnimationFrame(() => {
      input.current?.focus()
      input.current?.setSelectionRange(position, position)
    })
  }
  return (
    <div className="group-composer-input">
      {query && (
        <div className="mention-menu" role="listbox" aria-label="提及成员">
          {matches.map((cat, index) => (
            <button
              type="button"
              role="option"
              aria-selected={active === index}
              id={`mention-${cat.id}`}
              key={cat.id}
              onMouseDown={(event) => event.preventDefault()}
              onClick={() => select(cat)}
            >
              <Avatar preset={cat.avatar} customAvatar={cat.customAvatar} size="small" />
              <span>{cat.name}</span>
            </button>
          ))}
          {matches.length === 0 && <p>没有找到群成员</p>}
        </div>
      )}
      <textarea
        ref={input}
        rows={1}
        value={draft}
        aria-label={`给${title}发消息`}
        placeholder={`给 ${title} 发消息，@ 提及成员`}
        aria-expanded={query !== null}
        aria-autocomplete="list"
        aria-activedescendant={
          query && matches[active] ? `mention-${matches[active]!.id}` : undefined
        }
        onChange={(event) => update(event.target.value, event.target.selectionStart)}
        onBlur={() => setQuery(null)}
        onKeyDown={(event) => {
          if (event.nativeEvent.isComposing) return
          if (query) {
            if (event.key === 'Escape') {
              event.preventDefault()
              event.stopPropagation()
              setQuery(null)
              return
            }
            if ((event.key === 'ArrowDown' || event.key === 'ArrowUp') && matches.length) {
              event.preventDefault()
              setActive(
                (value) =>
                  (value + (event.key === 'ArrowDown' ? 1 : matches.length - 1)) % matches.length,
              )
              return
            }
            if (event.key === 'Enter' && matches[active]) {
              event.preventDefault()
              select(matches[active]!)
              return
            }
          }
          if (event.key === 'Enter' && !event.shiftKey) {
            event.preventDefault()
            onSend()
          }
        }}
      />
    </div>
  )
}
