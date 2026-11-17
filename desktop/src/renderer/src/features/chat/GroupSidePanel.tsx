import { Users, X } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import { GroupAvatar } from '../../components/GroupAvatar'
import { GroupMemberPicker } from './GroupMemberPicker'
import { GroupLibrary } from './GroupLibrary'
import { Avatar } from '../../components/Avatar'
import { getBackend, type GroupDetail } from '../desktop/backend'
import type { Agent } from '../workspace/model'

/** 群资料沿用猫咪右栏；修改串行提交，使用服务端版本，失败保留输入。 */
export function GroupSidePanel({
  group,
  agents,
  onClose,
  onChange,
}: {
  group: GroupDetail
  agents: Agent[]
  onClose: () => void
  onChange: (group: GroupDetail) => void
}) {
  const [tab, setTab] = useState<'members' | 'library'>('members')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState('')
  const locked = useRef(false)
  useEffect(() => {
    function close(event: KeyboardEvent) {
      if (event.key === 'Escape' && !document.querySelector('dialog[open], .member-picker-menu'))
        onClose()
    }
    window.addEventListener('keydown', close)
    return () => window.removeEventListener('keydown', close)
  }, [onClose])

  async function change(action: () => Promise<unknown>) {
    if (locked.current) return false
    locked.current = true
    setPending(true)
    setError('')
    try {
      await action()
      const load = getBackend().groupDetail
      if (!load) throw new Error('暂时无法读取群资料。')
      const next = await load(group.id)
      onChange(next)
      return true
    } catch (failure) {
      setError((failure as Error).message)
      // 冲突后取回版本，下一次用户操作才重试，避免重复添加或移除。
      try {
        const load = getBackend().groupDetail
        if (load) onChange(await load(group.id))
      } catch (refreshFailure) {
        setError(`${(failure as Error).message}；${(refreshFailure as Error).message}`)
      }
      return false
    } finally {
      locked.current = false
      setPending(false)
    }
  }

  const available = agents.filter(
    (agent) => !group.members.some((member) => member.bot_id === agent.id),
  )
  return (
    <aside className="cat-panel group-panel" aria-label={`${group.name}群资料`}>
      <button
        type="button"
        className="cat-panel-close round-button"
        aria-label="关闭群资料"
        onClick={onClose}
      >
        <X size={18} strokeWidth={1.7} />
      </button>
      <div className="cat-panel-hero">
        <GroupAvatar presets={group.avatar_presets} size="large" />
        <h2>{group.name}</h2>
        <p>{group.members.length} 位猫咪搭档</p>
      </div>
      <div className="cat-panel-tabs" role="tablist" aria-label="群栏目">
        {(['members', 'library'] as const).map((item) => (
          <button
            key={item}
            type="button"
            role="tab"
            aria-selected={tab === item}
            aria-controls={`group-panel-${item}`}
            id={`group-tab-${item}`}
            tabIndex={tab === item ? 0 : -1}
            onClick={() => setTab(item)}
            onKeyDown={(event) => {
              if (event.key !== 'ArrowLeft' && event.key !== 'ArrowRight') return
              event.preventDefault()
              const tabs = ['members', 'library'] as const
              const next = tabs[(tabs.indexOf(tab) + 1) % 2]!
              setTab(next)
              document.getElementById(`group-tab-${next}`)?.focus()
            }}
          >
            {item === 'library' ? '资料库' : '成员'}
          </button>
        ))}
      </div>
      <div
        className="cat-panel-body"
        role="tabpanel"
        id={`group-panel-${tab}`}
        aria-labelledby={`group-tab-${tab}`}
      >
        {tab === 'library' && <GroupLibrary groupId={group.id} />}
        {error && (
          <p role="alert" className="form-error">
            {error}
          </p>
        )}
        {tab === 'members' && (
          <section className="group-roster" aria-label="群成员">
            <h3>
              群成员 <span>{group.members.length + 1}</span>
            </h3>
            <div className="group-member-row">
              <Users size={24} />
              <span>你</span>
              <span>群主</span>
            </div>
            {group.members.map((member) => {
              const cat = agents.find((agent) => agent.id === member.bot_id)
              return (
                <div className="group-member-row" key={member.bot_id}>
                  {cat ? (
                    <Avatar preset={cat.avatar} customAvatar={cat.customAvatar} agentId={cat.id} />
                  ) : (
                    <Users size={24} />
                  )}
                  <span>{member.name}</span>
                  <button
                    type="button"
                    className="group-member-remove"
                    aria-label={`移出${member.name}`}
                    title={group.members.length === 1 ? '至少保留一位猫咪' : `移出${member.name}`}
                    disabled={pending || group.members.length === 1}
                    onClick={() =>
                      void change(async () => {
                        const remove = getBackend().removeGroupMember
                        if (!remove) throw new Error('暂时无法调整群成员。')
                        await remove(group.id, group.revision, member.bot_id)
                      })
                    }
                  >
                    移除
                  </button>
                </div>
              )
            })}
            <GroupMemberPicker
              agents={available}
              disabled={pending}
              onAdd={(id) =>
                change(async () => {
                  const add = getBackend().addGroupMembers
                  if (!add) throw new Error('暂时无法调整群成员。')
                  await add(group.id, group.revision, [id])
                })
              }
            />
          </section>
        )}
      </div>
    </aside>
  )
}
