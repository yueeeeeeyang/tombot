import { describe, expect, it } from 'vitest'
import type { Artifact } from '../workspace/model'
import { groupArtifacts } from './artifactGroups'

const now = new Date(2026, 10, 10, 18, 0, 0)

function artifact(id: string, day: number, hour: number, month = 10): Artifact {
  return {
    id,
    title: id,
    kind: 'file',
    createdAt: new Date(2026, month, day, hour, 0, 0).toISOString(),
  }
}

describe('资料库时间分组', () => {
  it('分成今天、本周和更早，组内新的在前，空组不出现', () => {
    const groups = groupArtifacts(
      [
        artifact('older', 1, 9),
        artifact('today-late', 10, 17),
        artifact('today-early', 10, 8),
        artifact('week', 9, 12),
      ],
      now,
    )
    expect(groups.map((group) => group.label)).toEqual(['今天', '本周', '更早'])
    expect(groups[0]?.artifacts.map((item) => item.id)).toEqual(['today-late', 'today-early'])
    expect(groups[1]?.artifacts.map((item) => item.id)).toEqual(['week'])
    expect(groups[2]?.artifacts.map((item) => item.id)).toEqual(['older'])
  })

  it('周一没有更早的本周日期时不产生本周分组', () => {
    const monday = new Date(2026, 10, 9, 12, 0, 0)
    const groups = groupArtifacts([artifact('today', 9, 10), artifact('last-week', 8, 10)], monday)
    expect(groups.map((group) => group.label)).toEqual(['今天', '更早'])
  })

  it('没有产物时不返回分组', () => {
    expect(groupArtifacts([], now)).toEqual([])
  })
})
