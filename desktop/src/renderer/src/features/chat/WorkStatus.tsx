import { Avatar } from '../../components/Avatar'
import type { Agent } from '../workspace/model'

/** 仅在实际执行时展示头像；模型摘要尚未到达时使用简短提示。 */
export function WorkStatus({
  summary = '',
  agents,
}: {
  summary?: string
  agents: Pick<Agent, 'id' | 'avatar' | 'customAvatar'>[]
}) {
  return (
    <div className="work-status" role="status" aria-live="polite">
      <div className="work-status-cats" aria-hidden="true">
        {agents.slice(0, 3).map((agent) => (
          <Avatar
            key={agent.id}
            preset={agent.avatar}
            customAvatar={agent.customAvatar}
            agentId={agent.id}
            size="small"
            activity="working"
          />
        ))}
        {agents.length > 3 && <span className="work-status-count">+{agents.length - 3}</span>}
      </div>
      <span className="work-status-summary">{summary || '正在思考…'}</span>
    </div>
  )
}
