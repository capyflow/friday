<script setup>
// RunsView 是全局运行看板：一屏汇总 ACP 当前轮次、计划、目标、定时任务、
// 待审批与失败项。数据来自只读聚合接口 /api/runs；动作全部走各自原有接口，
// 不再造一套运行状态。页面按需挂载（切走即卸载并停轮询），避免后台空转。
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { api } from '../api.js'
import { store, toast } from '../store.js'
import EmptyState from './art/EmptyState.vue'

const data = reactive({
  generatedAt: '', acp: [], plans: [], goals: [], tasks: [], approvals: [], failures: [],
  counts: {},
})
const loading = ref(true)
const refreshing = ref(false)
const error = ref('')
const busy = reactive({})
const confirmPlan = ref('')
const confirmAcp = ref('')
const confirmGoal = ref('')
let timer = null
let confirmTimer = null

const counts = computed(() => data.counts || {})
const hasAnything = computed(() =>
  data.acp.length || data.plans.length || data.goals.length || data.tasks.length ||
  data.approvals.length || data.failures.length)

const PLAN_STATE = { awaiting_confirm: '待确认', running: '执行中', paused: '已暂停', draft: '草稿' }
const GOAL_STATE = { active: '等待推进', paused: '已暂停' }

async function load(manual = false) {
  if (manual) refreshing.value = true
  try {
    const r = await api('GET', '/api/runs')
    data.acp = r.acp || []
    data.plans = r.plans || []
    data.goals = r.goals || []
    data.tasks = r.tasks || []
    data.approvals = r.approvals || []
    data.failures = r.failures || []
    data.counts = r.counts || {}
    data.generatedAt = r.generatedAt || ''
    error.value = ''
  } catch (e) {
    error.value = e.message || String(e)
  } finally {
    loading.value = false
    refreshing.value = false
  }
}

async function act(key, fn, success) {
  if (busy[key]) return
  busy[key] = true
  try {
    await fn()
    if (success) toast(success)
    await load(false)
  } catch (e) {
    toast(e.message || String(e), true)
  } finally {
    busy[key] = false
  }
}

function armOnce(refValue, id) {
  refValue.value = id
  clearTimeout(confirmTimer)
  confirmTimer = setTimeout(() => { refValue.value = '' }, 5000)
}

function decideApproval(a, approve) {
  act('a' + a.id, () => api('POST', '/api/approvals/' + a.id, { approve }),
    approve ? '已批准' : '已拒绝')
}

function stopAcp(t) {
  if (confirmAcp.value !== t.threadId) { armOnce(confirmAcp, t.threadId); return }
  confirmAcp.value = ''
  act('t' + t.threadId, () => api('POST', '/api/acp/threads/' + t.threadId + '/stop', {}), '已请求停止本轮')
}

function openAcp(t) {
  store.acpOpenThread = t.threadId
  store.view = 'agent'
}

function planAct(p, action) {
  if (action === 'cancel' && confirmPlan.value !== p.id) { armOnce(confirmPlan, p.id); return }
  if (action === 'cancel') confirmPlan.value = ''
  const label = action === 'pause' ? '已暂停计划' : action === 'resume' ? '已继续计划' : '已取消计划'
  act('p' + p.id + action, () => api('POST', '/api/plans/' + p.id + '/' + action, {}), label)
}

function stopGoal(g) {
  if (confirmGoal.value !== g.id) { armOnce(confirmGoal, g.id); return }
  confirmGoal.value = ''
  act('g' + g.id, () => api('POST', '/api/goals/' + g.id + '/stop', {}), '已请求停止目标（步边界生效）')
}

function toggleTask(t) {
  const action = t.status === 'paused' ? 'resume' : 'pause'
  act('k' + t.id, () => api('POST', '/api/tasks/' + t.id + '/' + action, {}),
    action === 'resume' ? '已恢复定时任务' : '已暂停定时任务')
}

function retryFailure(f) {
  if (f.kind === 'acp') { openAcp({ threadId: f.id }); return }
  act('f' + f.id, () => api('POST', '/api/notifications/' + f.id + '/retry', {}), '已重新排队')
}

function fmtWhen(v) {
  if (!v) return '未排期'
  const d = new Date(v)
  if (Number.isNaN(d.getTime())) return '未排期'
  return d.toLocaleString([], { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })
}
function since(v) {
  if (!v) return ''
  const ms = Date.now() - new Date(v).getTime()
  if (!Number.isFinite(ms) || ms < 0) return ''
  const min = Math.floor(ms / 60000)
  if (min < 1) return '刚刚'
  if (min < 60) return min + ' 分钟'
  const hr = Math.floor(min / 60)
  if (hr < 24) return hr + ' 小时'
  return Math.floor(hr / 24) + ' 天'
}
function ctxPct(t) {
  if (!t.contextSize) return 0
  return Math.min(100, Math.round(t.contextUsed * 100 / t.contextSize))
}
function planStateLabel(p) { return PLAN_STATE[p.state] || p.state }
function goalStateLabel(g) { return g.running ? '推进中' : (GOAL_STATE[g.status] || g.status) }
function taskKind(k) {
  return { once: '单次', repeat: '多次', cron: '循环', agent: '智能' }[k] || k
}
function shortPath(p) {
  if (!p) return ''
  const parts = p.split('/').filter(Boolean)
  return parts.length > 3 ? '…/' + parts.slice(-2).join('/') : p
}

onMounted(() => {
  load(false)
  timer = setInterval(() => {
    if (document.visibilityState === 'visible') load(false)
  }, 5000)
})
onUnmounted(() => {
  clearInterval(timer)
  clearTimeout(confirmTimer)
})
</script>

<template>
  <section class="view" id="view-runs">
    <div class="view-inner">
      <header class="runs-head">
        <div>
          <h2>运行 <span class="count">{{ hasAnything ? '正在看 ' + (data.acp.length + data.plans.length + data.goals.length) + ' 项' : '' }}</span></h2>
          <p class="meta runs-sub">所有正在跑、排队和需要你处理的事。审批与停止都在这里。</p>
        </div>
        <div class="runs-head-actions">
          <span class="meta runs-updated" role="status" aria-live="polite">
            {{ refreshing ? '刷新中…' : data.generatedAt ? '更新于 ' + new Date(data.generatedAt).toLocaleTimeString() : '' }}
          </span>
          <button class="btn sm" type="button" :disabled="refreshing" @click="load(true)">刷新</button>
        </div>
      </header>

      <div class="runs-summary" role="status" aria-live="polite">
        <span class="run-chip" v-for="c in [
          { key: 'acp', label: '智能体', n: counts.acp || 0 },
          { key: 'plans', label: '计划', n: counts.plans || 0 },
          { key: 'goals', label: '目标', n: counts.goals || 0 },
          { key: 'tasks', label: '定时', n: counts.tasks || 0 },
          { key: 'approvals', label: '待审批', n: counts.approvals || 0, warn: true },
          { key: 'failures', label: '失败', n: counts.failures || 0, bad: true }
        ]" :key="c.key" :class="{ warn: c.warn && c.n, bad: c.bad && c.n }">
          <b>{{ c.n }}</b><span>{{ c.label }}</span>
        </span>
        <span v-if="!hasAnything && !loading" class="meta">现在没有在跑的任务</span>
      </div>

      <p v-if="error" class="msg err" role="alert">{{ error }}</p>
      <p v-if="loading" class="run-loading">正在读取运行状态…</p>

      <template v-if="!loading">
        <section v-if="data.approvals.length" class="panel runs-sec" aria-labelledby="runs-approvals-title">
          <div class="runs-sec-head">
            <h3 id="runs-approvals-title">待审批 <span class="count">{{ data.approvals.length }}</span></h3>
            <p class="meta">主聊天与工作区共用同一条审批队列；30 分钟无人处理会自动拒绝。</p>
          </div>
          <article v-for="a in data.approvals" :key="a.id" class="run-card approval-card">
            <div class="run-card-main">
              <div class="run-card-title"><span class="badge agent">{{ a.tool }}</span><b>{{ a.summary }}</b></div>
              <pre v-if="a.detail" class="run-detail">{{ a.detail }}</pre>
              <span class="meta">{{ since(a.createdAt) }}前 · 等待你决定</span>
            </div>
            <div class="run-actions">
              <button class="btn sm primary" type="button" :disabled="busy['a' + a.id]" @click="decideApproval(a, true)">批准</button>
              <button class="btn sm danger" type="button" :disabled="busy['a' + a.id]" @click="decideApproval(a, false)">拒绝</button>
            </div>
          </article>
        </section>

        <section v-if="data.acp.length" class="panel runs-sec" aria-labelledby="runs-acp-title">
          <div class="runs-sec-head">
            <h3 id="runs-acp-title">智能体运行 <span class="count">{{ data.acp.length }}</span></h3>
            <p class="meta">同一目录的会话共享一条连接；中断的会话发下一句会尝试重接。</p>
          </div>
          <article v-for="t in data.acp" :key="t.threadId" class="run-card">
            <div class="run-card-main">
              <div class="run-card-title">
                <span class="run-dot" :class="t.state" aria-hidden="true"></span>
                <b>{{ t.title }}</b>
                <span v-if="t.state === 'interrupted'" class="badge cron">已中断</span>
              </div>
              <div class="run-meta">
                <span v-if="t.workspace">{{ t.workspace }}</span>
                <span>{{ t.agent }}</span>
                <span :title="t.cwd">{{ shortPath(t.cwd) }}</span>
                <span v-if="t.startedAt && t.state === 'running'">{{ since(t.startedAt) }}前开始</span>
                <span v-else-if="t.state === 'interrupted'">上次运行被中断</span>
                <span v-if="t.contextSize">上下文 {{ ctxPct(t) }}%</span>
              </div>
            </div>
            <div class="run-actions">
              <button class="btn sm" type="button" @click="openAcp(t)">打开</button>
              <button v-if="t.state === 'running'" class="btn sm danger" type="button" :disabled="busy['t' + t.threadId]"
                      @click="stopAcp(t)">
                {{ confirmAcp === t.threadId ? '确认停止' : '停止本轮' }}
              </button>
            </div>
          </article>
        </section>

        <section v-if="data.plans.length" class="panel runs-sec" aria-labelledby="runs-plans-title">
          <div class="runs-sec-head">
            <h3 id="runs-plans-title">计划 <span class="count">{{ data.plans.length }}</span></h3>
            <p class="meta">计划步骤在各自会话里串行执行；取消只停未来步骤，已发生的改动不回滚。</p>
          </div>
          <article v-for="p in data.plans" :key="p.id" class="run-card">
            <div class="run-card-main">
              <div class="run-card-title">
                <span class="badge" :class="p.state === 'awaiting_confirm' ? 'cron' : p.state === 'running' ? 'agent' : 'once'">{{ planStateLabel(p) }}</span>
                <b>{{ p.title || '未命名计划' }}</b>
              </div>
              <div class="run-meta">
                <span v-if="p.state === 'awaiting_confirm'">{{ p.totalSteps }} 个步骤待确认</span>
                <span v-else-if="p.currentStep">第 {{ p.currentStep }} / {{ p.totalSteps }} 步 · {{ p.currentTitle }}</span>
                <span v-else>{{ p.totalSteps }} 个步骤</span>
                <span v-if="p.updatedAt">{{ since(p.updatedAt) }}前更新</span>
              </div>
            </div>
            <div class="run-actions">
              <button v-if="p.state === 'awaiting_confirm'" class="btn sm primary" type="button"
                      @click="store.view = 'chat'">去确认</button>
              <button v-if="p.state === 'awaiting_confirm'" class="btn sm danger" type="button"
                      :disabled="busy['p' + p.id + 'cancel']" @click="planAct(p, 'cancel')">
                {{ confirmPlan === p.id ? '确认取消' : '取消' }}
              </button>
              <template v-else>
                <button class="btn sm" type="button" :disabled="busy['p' + p.id + 'pause']"
                        @click="planAct(p, p.state === 'paused' ? 'resume' : 'pause')">
                  {{ p.state === 'paused' ? '继续' : '暂停' }}
                </button>
                <button class="btn sm danger" type="button" :disabled="busy['p' + p.id + 'cancel']"
                        @click="planAct(p, 'cancel')">
                  {{ confirmPlan === p.id ? '确认取消' : '取消' }}
                </button>
              </template>
            </div>
          </article>
        </section>

        <section v-if="data.goals.length" class="panel runs-sec" aria-labelledby="runs-goals-title">
          <div class="runs-sec-head">
            <h3 id="runs-goals-title">目标 <span class="count">{{ data.goals.length }}</span></h3>
            <p class="meta">目标在独立会话里多轮推进；停止在步边界生效，已产出的结论仍会送达。</p>
          </div>
          <article v-for="g in data.goals" :key="g.id" class="run-card">
            <div class="run-card-main">
              <div class="run-card-title">
                <span class="badge goal">{{ goalStateLabel(g) }}</span>
                <b>{{ g.text }}</b>
              </div>
              <div class="run-meta">
                <span>{{ g.usedTokens.toLocaleString() }} / {{ g.tokensLimit.toLocaleString() }} tokens</span>
                <span>步数上限 {{ g.stepsLimit }}</span>
                <span v-if="!g.running && g.next">下次推进 {{ fmtWhen(g.next) }}</span>
              </div>
            </div>
            <div class="run-actions">
              <button v-if="g.running" class="btn sm danger" type="button" :disabled="busy['g' + g.id]"
                      @click="stopGoal(g)">
                {{ confirmGoal === g.id ? '确认停止' : '停止这次推进' }}
              </button>
              <button v-else class="btn sm" type="button" @click="store.view = 'auto'">去自动化</button>
            </div>
          </article>
        </section>

        <section v-if="data.tasks.length" class="panel runs-sec" aria-labelledby="runs-tasks-title">
          <div class="runs-sec-head">
            <h3 id="runs-tasks-title">定时任务 <span class="count">{{ data.tasks.length }}</span></h3>
            <p class="meta">只列最近几条；编辑触发时间与推送通道去「自动化」。</p>
          </div>
          <article v-for="k in data.tasks" :key="k.id" class="run-card task-run">
            <div class="run-card-main">
              <div class="run-card-title">
                <span class="badge" :class="k.kind">{{ taskKind(k.kind) }}</span>
                <b>{{ k.text }}</b>
                <span v-if="k.status === 'paused'" class="badge once">已暂停</span>
              </div>
              <div class="run-meta"><span>{{ k.status === 'paused' ? '暂停中' : '下次 ' + fmtWhen(k.next) }}</span></div>
            </div>
            <div class="run-actions">
              <button class="btn sm" type="button" :disabled="busy['k' + k.id]" @click="toggleTask(k)">
                {{ k.status === 'paused' ? '恢复' : '暂停' }}
              </button>
            </div>
          </article>
          <button class="run-link" type="button" @click="store.view = 'auto'">去自动化管理全部任务 →</button>
        </section>

        <section v-if="data.failures.length" class="panel runs-sec" aria-labelledby="runs-failures-title">
          <div class="runs-sec-head">
            <h3 id="runs-failures-title">需要处理 <span class="count">{{ data.failures.length }}</span></h3>
            <p class="meta">失败通知可重新排队；中断的会话打开后发下一句即可重接。</p>
          </div>
          <article v-for="f in data.failures" :key="f.kind + f.id" class="run-card">
            <div class="run-card-main">
              <div class="run-card-title">
                <span class="badge" :class="f.kind === 'acp' ? 'agent' : 'cron'">{{ f.kind === 'acp' ? '会话' : '通知' }}</span>
                <b>{{ f.title }}</b>
              </div>
              <p v-if="f.detail" class="run-detail">{{ f.detail }}</p>
              <span class="meta">{{ since(f.at) }}前</span>
            </div>
            <div class="run-actions">
              <button v-if="f.kind === 'acp'" class="btn sm" type="button" @click="retryFailure(f)">打开会话</button>
              <button v-else class="btn sm" type="button" :disabled="busy['f' + f.id]" @click="retryFailure(f)">重新排队</button>
            </div>
          </article>
        </section>

        <EmptyState v-if="!hasAnything && !error" class="panel" kind="schedule" :size="96">
          现在没有在跑的任务。定时提醒、待办和工作区里的新消息仍会照常触发。
        </EmptyState>
      </template>
    </div>
  </section>
</template>
