import { Star } from 'lucide-react'
import { useEffect, useState, type CSSProperties } from 'react'
import { useAgentActivity } from '../features/avatars/activity'
import {
  catMotionId,
  useCatEye,
  useCatFrameStatus,
  useWorkingTwitch,
} from '../features/avatars/catMotion'
import {
  ENTER_SETTLE_MS,
  ENTER_TWITCH_MS,
  PERK_HALF_MS,
  resolveEarFrame,
  type HoverEntry,
} from '../features/avatars/ears'
import { catEyeOverlaySource, catPortraitSource } from '../features/avatars/frames'
import { resolveAvatarPresentation, type AgentActivity } from '../features/avatars/pose'
import type { CatAvatarId } from '../features/avatars/presets'
import { useAvatarPointer } from '../features/avatars/useAvatarPointer'
import { useLookTilt } from '../features/avatars/useLookTilt'
import { usePrefersReducedMotion } from '../features/avatars/usePrefersReducedMotion'

/**
 * 显示打包好的猫咪帧。姿态只来自 resolveAvatarPresentation。
 * 跟随指针是侧栏专属：会话标题和选择器不传 tracksPointer，避免满屏一起转头。
 * 等待区通过 activity 表示当前请求的工作状态，不反向修改侧栏的全局活动。
 */
export function Avatar({
  preset,
  size = 'medium',
  starred = false,
  agentId,
  tracksPointer = false,
  activity: explicitActivity,
  customAvatar,
}: {
  preset: CatAvatarId
  size?: 'small' | 'medium' | 'large'
  starred?: boolean
  agentId?: string
  tracksPointer?: boolean
  activity?: AgentActivity
  customAvatar?: string
}) {
  const activity = useAgentActivity(agentId)
  const prefersReducedMotion = usePrefersReducedMotion()
  const frameStatus = useCatFrameStatus()
  const framesReady = frameStatus === 'ready'
  const motionId = catMotionId(agentId, preset)
  const eye = useCatEye(motionId)
  const { ref, pointer, hoverEntry } = useAvatarPointer(tracksPointer, agentId)
  const presentation = resolveAvatarPresentation({
    activity: explicitActivity ?? activity,
    pointer,
    prefersReducedMotion,
  })
  const hoverNow = useHoverClock(hoverEntry)
  const workingTwitch = useWorkingTwitch(
    motionId,
    presentation.pose === 'working' && framesReady && !prefersReducedMotion,
  )
  const ear = resolveEarFrame({
    pose: presentation.pose,
    framesReady,
    prefersReducedMotion,
    now: hoverEntry ? hoverNow : 0,
    hoverEntry,
    workingTwitch,
  })
  const overlay =
    !customAvatar && framesReady && !prefersReducedMotion ? catEyeOverlaySource(preset, eye) : null
  useLookTilt(ref, presentation.lookX, presentation.lookY, prefersReducedMotion)
  return (
    <span
      ref={ref}
      className={`avatar avatar--${size}${customAvatar ? ' avatar--custom' : ''}`}
      data-avatar={customAvatar ? 'custom' : preset}
      data-pose={presentation.pose}
      data-pressed={presentation.isPressed ? 'true' : 'false'}
      data-frames={frameStatus}
      data-eye={prefersReducedMotion || !framesReady ? 'open' : eye}
      data-ear={ear}
      style={
        {
          '--look-x': String(presentation.lookX),
          '--look-y': String(presentation.lookY),
        } as CSSProperties
      }
      aria-hidden="true"
    >
      <span className="cat-stage">
        <span className="cat-tilt">
          <span className="cat-motion">
            <img
              className="cat-portrait"
              src={customAvatar || catPortraitSource(preset, ear)}
              alt=""
              draggable={false}
              decoding="sync"
            />
            {overlay && (
              <img className="cat-eyes" src={overlay} alt="" draggable={false} decoding="sync" />
            )}
          </span>
        </span>
      </span>
      {presentation.pose === 'working' && (
        <span className="cat-work-mark">
          <svg className="cat-work-dots" viewBox="0 0 22 8">
            <circle cx="2.2" cy="4" r="1.6" />
            <circle cx="11" cy="4" r="1.6" />
            <circle cx="19.8" cy="4" r="1.6" />
          </svg>
        </span>
      )}
      {starred && (
        <span className="avatar-star">
          <Star size={10} fill="currentColor" strokeWidth={1.5} />
        </span>
      )}
    </span>
  )
}

/** 只在进场后的一小段里跟帧，竖耳稳住后就停，避免每只猫都空转。 */
function useHoverClock(entry: HoverEntry | null): number {
  const [now, setNow] = useState(0)
  useEffect(() => {
    if (!entry) return
    const ends = entry.at + PERK_HALF_MS + ENTER_TWITCH_MS + ENTER_SETTLE_MS
    let frame = 0
    const tick = (time: number) => {
      setNow(time)
      if (time < ends) frame = requestAnimationFrame(tick)
    }
    frame = requestAnimationFrame(tick)
    return () => cancelAnimationFrame(frame)
  }, [entry])
  return now
}
