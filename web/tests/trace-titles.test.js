import { describe, expect, it } from 'vitest'
import { EV_TITLE } from '../src/format.js'

// 后端每新增一种轨迹条目，前端就要登记标题与配色；漏了会直接显示裸 key。
describe('轨迹条目标题', () => {
  it('技能/计划/目标/成本条目都有中文标题', () => {
    for (const kind of ['skill', 'plan_step', 'goal_run', 'goal_step', 'budget', 'approval', 'pause']) {
      expect(EV_TITLE[kind], kind).toBeTruthy()
    }
  })
})
