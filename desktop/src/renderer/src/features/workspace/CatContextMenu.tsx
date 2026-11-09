import {
  Bell,
  Check,
  ChevronRight,
  Copy,
  EyeOff,
  Folder,
  FolderOpen,
  Pencil,
  Pin,
  PinOff,
  SquarePlus,
  Trash2,
} from 'lucide-react'
import { useLayoutEffect, useRef, useState, type KeyboardEvent, type RefObject } from 'react'
import type { Agent } from './model'

export interface CatMenuAnchor {
  agentId: string
  x: number
  y: number
  trigger: HTMLElement
}
export type CatMenuAction = 'pin' | 'unread' | 'rename' | 'copy' | 'hide' | 'delete'

/** 在菜单的可用项中循环移动焦点；只处理导航，不改变猫咪状态。 */
function navigateMenu(event: KeyboardEvent<HTMLDivElement>): void {
  if (!['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) return
  event.preventDefault()
  const buttons = [
    ...event.currentTarget.querySelectorAll<HTMLButtonElement>('button:not(:disabled)'),
  ]
  const current = buttons.findIndex((button) => button === document.activeElement)
  const direction = event.key === 'ArrowDown' ? 1 : -1
  const index =
    event.key === 'Home'
      ? 0
      : event.key === 'End'
        ? buttons.length - 1
        : current === -1
          ? direction === 1
            ? 0
            : buttons.length - 1
          : (current + direction + buttons.length) % buttons.length
  buttons[index]?.focus()
}

/** 自绘猫咪右键菜单；只对右键目标执行操作，层级、边缘定位和键盘关闭由菜单管理。 */
export function CatContextMenu({
  anchor,
  agent,
  groups,
  isPinned,
  isUnread,
  onAction,
  onMove,
  onCreateGroup,
  onClose,
}: {
  anchor: CatMenuAnchor
  agent: Agent
  groups: string[]
  isPinned: boolean
  isUnread: boolean
  onAction: (action: CatMenuAction) => void
  onMove: (group: string) => void
  onCreateGroup: () => void
  onClose: () => void
}) {
  const menu = useRef<HTMLDivElement>(null)
  const moveButton = useRef<HTMLButtonElement>(null)
  const submenu = useRef<HTMLDivElement>(null)
  const [isMoveOpen, setMoveOpen] = useState(false)
  const [focusSubmenu, setFocusSubmenu] = useState(false)
  const isDefault = agent.id === 'assistant'

  useLayoutEffect(() => {
    const element = menu.current!
    element.showPopover()
    const bounds = element.getBoundingClientRect()
    element.style.left = `${Math.max(8, Math.min(anchor.x, innerWidth - bounds.width - 8))}px`
    element.style.top = `${Math.max(8, Math.min(anchor.y, innerHeight - bounds.height - 8))}px`
    element
      .querySelector<HTMLButtonElement>('button:not(:disabled)')
      ?.focus({ preventScroll: true })
    function closeOutside(event: PointerEvent): void {
      if (
        event.target instanceof Node &&
        !element.contains(event.target) &&
        !submenu.current?.contains(event.target)
      )
        onClose()
    }
    function closeOnScroll(event: Event): void {
      if (
        event.target instanceof Node &&
        (element.contains(event.target) || submenu.current?.contains(event.target))
      )
        return
      onClose()
    }
    document.addEventListener('pointerdown', closeOutside, true)
    document.addEventListener('scroll', closeOnScroll, true)
    window.addEventListener('resize', onClose)
    window.addEventListener('blur', onClose)
    return () => {
      element.hidePopover()
      document.removeEventListener('pointerdown', closeOutside, true)
      document.removeEventListener('scroll', closeOnScroll, true)
      window.removeEventListener('resize', onClose)
      window.removeEventListener('blur', onClose)
      if (anchor.trigger.isConnected) anchor.trigger.focus({ preventScroll: true })
    }
  }, [anchor, onClose])

  const actions = [
    {
      action: 'pin',
      label: isPinned || isDefault ? '取消置顶' : '置顶',
      Icon: isPinned ? PinOff : Pin,
      disabled: isDefault,
      title: isDefault ? '默认猫咪固定置顶' : undefined,
    },
    { action: 'unread', label: isUnread ? '标为已读' : '标为未读', Icon: Bell },
    { action: 'rename', label: '重命名猫咪', Icon: Pencil, separator: true },
    { action: 'copy', label: '复制对话 ID', Icon: Copy, separator: true },
    {
      action: 'hide',
      label: '从侧边栏隐藏',
      Icon: EyeOff,
      separator: true,
      disabled: isDefault,
      title: isDefault ? '默认猫咪不能隐藏' : undefined,
    },
    {
      action: 'delete',
      label: '删除',
      Icon: Trash2,
      disabled: isDefault,
      title: isDefault ? '默认猫咪不能删除' : undefined,
    },
  ] as const

  return (
    <>
      <div
        ref={menu}
        className="cat-context-menu"
        role="menu"
        aria-label={`${agent.name}的菜单`}
        popover="manual"
        onContextMenu={(event) => event.preventDefault()}
        onKeyDown={(event) => {
          if (event.key === 'Escape' || event.key === 'Tab') {
            event.preventDefault()
            onClose()
          } else if (event.key === 'ArrowRight' && document.activeElement === moveButton.current) {
            event.preventDefault()
            setFocusSubmenu(true)
            setMoveOpen(true)
          } else navigateMenu(event)
        }}
      >
        {actions.map((item, index) => (
          <div key={item.action}>
            {'separator' in item && item.separator && (
              <div className="cat-menu-separator" role="separator" />
            )}
            <button
              type="button"
              role="menuitem"
              className={item.action === 'delete' ? 'is-danger' : ''}
              disabled={'disabled' in item && item.disabled}
              title={'title' in item ? item.title : undefined}
              onPointerEnter={() => setMoveOpen(false)}
              onClick={() => onAction(item.action)}
            >
              <item.Icon size={15} strokeWidth={1.7} />
              <span>{item.label}</span>
            </button>
            {index === 0 && (
              <button
                ref={moveButton}
                type="button"
                role="menuitem"
                aria-haspopup="menu"
                aria-expanded={isMoveOpen}
                disabled={isDefault}
                title={isDefault ? '默认猫咪不参与分组' : undefined}
                onPointerEnter={() => {
                  if (!isDefault) {
                    setFocusSubmenu(false)
                    setMoveOpen(true)
                  }
                }}
                onClick={() => {
                  setFocusSubmenu(true)
                  setMoveOpen(true)
                }}
              >
                <Folder size={15} strokeWidth={1.7} />
                <span>移至</span>
                <ChevronRight className="cat-menu-chevron" size={14} />
              </button>
            )}
          </div>
        ))}
      </div>
      {isMoveOpen && (
        <MoveGroupMenu
          menuRef={submenu}
          anchorRef={moveButton}
          groups={groups}
          currentGroup={agent.group}
          focusOnOpen={focusSubmenu}
          onMove={onMove}
          onCreateGroup={onCreateGroup}
          onClose={() => {
            setMoveOpen(false)
            moveButton.current?.focus()
          }}
          onDismiss={onClose}
        />
      )}
    </>
  )
}

/** 分组子菜单按可用空间向左或向右展开，键盘返回时恢复“移至”焦点。 */
function MoveGroupMenu({
  menuRef,
  anchorRef,
  groups,
  currentGroup,
  focusOnOpen,
  onMove,
  onCreateGroup,
  onClose,
  onDismiss,
}: {
  menuRef: RefObject<HTMLDivElement | null>
  anchorRef: RefObject<HTMLButtonElement | null>
  groups: string[]
  currentGroup: string
  focusOnOpen: boolean
  onMove: (group: string) => void
  onCreateGroup: () => void
  onClose: () => void
  onDismiss: () => void
}) {
  useLayoutEffect(() => {
    const element = menuRef.current!
    element.showPopover()
    const anchor = anchorRef.current!.getBoundingClientRect()
    const bounds = element.getBoundingClientRect()
    const left =
      anchor.right + bounds.width + 12 <= innerWidth
        ? anchor.right + 6
        : anchor.left - bounds.width - 6
    element.style.left = `${Math.max(8, left)}px`
    element.style.top = `${Math.max(8, Math.min(anchor.top, innerHeight - bounds.height - 8))}px`
    return () => element.hidePopover()
  }, [menuRef, anchorRef])
  useLayoutEffect(() => {
    if (focusOnOpen)
      menuRef.current?.querySelector<HTMLButtonElement>('button')?.focus({ preventScroll: true })
  }, [focusOnOpen, menuRef])
  return (
    <div
      ref={menuRef}
      className="cat-context-menu cat-move-menu"
      role="menu"
      aria-label="移至分组"
      popover="manual"
      onKeyDown={(event) => {
        if (event.key === 'ArrowLeft' || event.key === 'Escape') {
          event.preventDefault()
          onClose()
        } else if (event.key === 'Tab') {
          event.preventDefault()
          onDismiss()
        } else navigateMenu(event)
      }}
    >
      {groups.map((group) => (
        <button
          type="button"
          role="menuitemradio"
          aria-checked={group === currentGroup}
          key={group}
          onClick={() => onMove(group)}
        >
          {group === '未分组' ? (
            <FolderOpen size={15} strokeWidth={1.7} />
          ) : (
            <Folder size={15} strokeWidth={1.7} />
          )}
          <span>{group}</span>
          {group === currentGroup && <Check size={14} />}
        </button>
      ))}
      <button
        type="button"
        role="menuitem"
        className="cat-menu-create-group"
        onClick={onCreateGroup}
      >
        <SquarePlus size={15} strokeWidth={1.7} />
        <span>创建分组</span>
      </button>
    </div>
  )
}
