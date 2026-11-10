import { Plus, Search } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import { Avatar } from '../../components/Avatar'
import type { Agent } from '../workspace/model'

/** 下拉菜单只列出可加入的猫咪，搜索与焦点都留在当前右栏。 */
export function GroupMemberPicker({
  agents,
  disabled,
  onAdd,
}: {
  agents: Agent[]
  disabled: boolean
  onAdd: (id: string) => Promise<boolean>
}) {
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState('')
  const root = useRef<HTMLDivElement>(null)
  const trigger = useRef<HTMLButtonElement>(null)
  useEffect(() => {
    if (!open) return
    function outside(event: PointerEvent) {
      if (!root.current?.contains(event.target as Node)) setOpen(false)
    }
    document.addEventListener('pointerdown', outside)
    return () => document.removeEventListener('pointerdown', outside)
  }, [open])
  const filtered = agents.filter((agent) =>
    agent.name.toLocaleLowerCase().includes(query.trim().toLocaleLowerCase()),
  )
  return (
    <div
      ref={root}
      className="member-picker"
      onKeyDown={(event) => {
        if (event.key === 'Escape' && open) {
          event.stopPropagation()
          setOpen(false)
          trigger.current?.focus()
        }
      }}
    >
      <button
        ref={trigger}
        className="group-add-trigger"
        type="button"
        disabled={disabled || agents.length === 0}
        aria-expanded={open}
        onClick={() => {
          setQuery('')
          setOpen(!open)
        }}
      >
        <Plus size={18} />
        添加成员
      </button>
      {open && (
        <div className="member-picker-menu">
          <label className="member-picker-search">
            <Search size={16} />
            <input
              autoFocus
              aria-label="搜索猫咪名称"
              placeholder="搜索猫咪"
              value={query}
              onChange={(event) => setQuery(event.target.value)}
            />
          </label>
          <div className="member-picker-results" role="listbox" aria-label="可加入的猫咪">
            {filtered.map((agent) => (
              <button
                role="option"
                aria-selected={false}
                key={agent.id}
                className="group-member-row group-member-add"
                disabled={disabled}
                onClick={() =>
                  void onAdd(agent.id).then((added) => {
                    if (added) {
                      setOpen(false)
                      trigger.current?.focus()
                    }
                  })
                }
              >
                <Avatar preset={agent.avatar} customAvatar={agent.customAvatar} />
                <span>{agent.name}</span>
              </button>
            ))}
            {filtered.length === 0 && <p className="member-picker-empty">没有找到猫咪</p>}
          </div>
        </div>
      )}
    </div>
  )
}
