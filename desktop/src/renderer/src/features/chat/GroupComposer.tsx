import { useLayoutEffect, useRef, useState } from 'react'
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
  const pendingCaret = useRef<{ value: string; position: number } | null>(null)
  useLayoutEffect(() => {
    const caret = pendingCaret.current
    pendingCaret.current = null
    if (caret && caret.value === draft) {
      input.current?.focus()
      input.current?.setSelectionRange(caret.position, caret.position)
    }
  }, [draft])
  const [query, setQuery] = useState<{ start: number; end: number; text: string } | null>(null)
  const [active, setActive] = useState(0)
  const [selected, setSelected] = useState<Array<{ id: string; name: string }>>([])
  const matches = query
    ? members.filter((cat) => cat.name.toLocaleLowerCase().includes(query.text.toLocaleLowerCase()))
    : []
  function update(value: string, position: number) {
    const chosen = selected.filter(
      (cat) =>
        members.some((member) => member.id === cat.id && member.name === cat.name) &&
        containsMention(value, cat.name),
    )
    setSelected(chosen)
    onChange(
      value,
      chosen.map((cat) => cat.id),
    )
    const prefix = value.slice(0, position)
    const match = /(?:^|[^a-zA-Z0-9._%+-])@([^@\n]*)$/u.exec(prefix)
    // 已完成的提及及其后续正文不再参与搜索；名称本身仍可包含空格。
    const isCompleted = match && members.some((cat) => match[1]!.startsWith(`${cat.name} `))
    setQuery(
      match && !isCompleted
        ? { start: prefix.lastIndexOf('@'), end: position, text: match[1]! }
        : null,
    )
    setActive(0)
  }
  function select(cat: Agent) {
    if (!query) return
    const chosen = [
      ...selected.filter((item) => item.id !== cat.id),
      { id: cat.id, name: cat.name },
    ]
    const value = draft.slice(0, query.start) + `@${cat.name} ` + draft.slice(query.end)
    pendingCaret.current = { value, position: query.start + cat.name.length + 2 }
    setSelected(chosen)
    onChange(
      value,
      chosen.map((item) => item.id),
    )
    setQuery(null)
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

/** 名称编辑后移除失效目标，避免把“@小黑猫”同时发给之前选择的“小黑”。 */
function containsMention(text: string, name: string): boolean {
  const escaped = name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  return new RegExp(`(?:^|[^a-zA-Z0-9._%+-])@${escaped}(?=$|[\\s\\p{P}@])`, 'u').test(text)
}
