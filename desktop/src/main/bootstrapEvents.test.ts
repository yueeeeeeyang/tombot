import { describe, expect, it } from 'vitest'
import { normalizeStatus, parseEventBlock } from './bootstrapEvents'

describe('引导事件', () => {
  it('忽略心跳，并保留步骤、阶段和游标', () => {
    expect(parseEventBlock(': ping\n', 4)).toEqual({ kind: 'ignore' })
    expect(parseEventBlock('event: conversation.updated\ndata: {}', 4)).toEqual({ kind: 'ignore' })
    expect(
      parseEventBlock(
        'id: 8\nevent: bootstrap.step\ndata: {"phase":"preparing","steps":[{"id":"install_fonts","title":"安装中文字体","status":"running","duration_ms":0}],"error":""}',
        4,
      ),
    ).toEqual({
      kind: 'status',
      status: {
        phase: 'preparing',
        cursor: 8,
        steps: [{ id: 'install_fonts', title: '安装中文字体', status: 'running', duration_ms: 0 }],
      },
    })
  })

  it('resync 要求重新读取，损坏的事件和状态直接失败', () => {
    expect(parseEventBlock('event: resync\ndata: {}', 2)).toEqual({ kind: 'resync' })
    expect(() => parseEventBlock('event: bootstrap.step\ndata: {', 1)).toThrow('无法识别')
    expect(() => normalizeStatus({ steps: [] })).toThrow('缺少阶段')
    expect(() => normalizeStatus({ phase: 'failed', steps: [{ id: 'ensure_browser' }] })).toThrow(
      '缺少标识',
    )
    expect(
      normalizeStatus({ phase: 'failed', error: '夹具没有浏览器', cursor: 3, steps: null }),
    ).toEqual({
      phase: 'failed',
      error: '夹具没有浏览器',
      cursor: 3,
      steps: [],
    })
  })
})
