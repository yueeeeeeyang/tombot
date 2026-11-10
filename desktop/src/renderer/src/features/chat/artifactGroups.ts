import type { Artifact } from '../workspace/model'

export const ARTIFACT_PREVIEW_COUNT = 5
export const ARTIFACT_GROUP_LABELS = ['今天', '本周', '更早'] as const
export type ArtifactGroupLabel = (typeof ARTIFACT_GROUP_LABELS)[number]

export interface ArtifactGroup {
  label: ArtifactGroupLabel
  artifacts: Artifact[]
}

function startOfLocalDay(date: Date): number {
  return new Date(date.getFullYear(), date.getMonth(), date.getDate()).getTime()
}

/** 以周一为一周的开始，和中文里的“本周”一致。 */
function startOfLocalWeek(date: Date): number {
  const day = date.getDay()
  const mondayOffset = day === 0 ? 6 : day - 1
  return new Date(date.getFullYear(), date.getMonth(), date.getDate() - mondayOffset).getTime()
}

function groupLabel(createdAt: string, now: Date): ArtifactGroupLabel {
  const timestamp = new Date(createdAt)
  if (startOfLocalDay(timestamp) >= startOfLocalDay(now)) return '今天'
  if (startOfLocalWeek(timestamp) === startOfLocalWeek(now)) return '本周'
  return '更早'
}

/** 按今天、本周、更早分组，组内和组间都是新的在前；没有产物的组不出现。 */
export function groupArtifacts(artifacts: readonly Artifact[], now: Date): ArtifactGroup[] {
  const buckets = new Map<ArtifactGroupLabel, Artifact[]>()
  const sorted = [...artifacts].sort(
    (left, right) => new Date(right.createdAt).getTime() - new Date(left.createdAt).getTime(),
  )
  for (const artifact of sorted) {
    const label = groupLabel(artifact.createdAt, now)
    const group = buckets.get(label)
    if (group) group.push(artifact)
    else buckets.set(label, [artifact])
  }
  return ARTIFACT_GROUP_LABELS.flatMap((label) => {
    const group = buckets.get(label)
    return group ? [{ label, artifacts: group }] : []
  })
}
