<script setup>
// WorkspaceGit 是工作区「Git 管理」标签页的内容：分支、状态、历史三类只读查询，
// 外加切分支 / 建分支 / 暂存 / 取消暂存 / 提交这几类写操作。
//
// 与验证面板同一条原则：写操作不直接落地。POST /git/ops 只是把命令排进审批队列
// （state=awaiting），用户到审批卡上点「批准」才真正执行。所以本组件不能在
// POST 返回时就宣称「已提交」，只能老老实实轮询 GET /git/ops，等这条 op 走到
// 终态（ok / failed / denied / error）再刷新仓库状态并把结果讲清楚。
//
// 只读查询不设闸（它不改仓库），所以状态/分支/历史可以直接拉。
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { api } from '../api.js'

const props = defineProps({
  thread: { type: Object, default: null },
  messages: { type: Array, default: () => [] },
  // 轮询间隔做成 prop 只是为了测试能用毫秒级间隔跑轮询；面板本身不传，用默认 1.5 秒。
  pollInterval: { type: Number, default: 1500 },
})

const status = ref(null)
const branches = ref([])
const commits = ref([])
const ops = ref([])
const loading = ref(false)
const error = ref('')
const notice = ref('')
const noticeKind = ref('info')
const busy = ref(false)
const newBranch = ref('')
const commitMsg = ref('')
const expanded = ref({})

// 四个请求各带一个代号：切会话后旧回包必须作废，否则上一个仓库的状态会盖到当前面板上。
let statusGen = 0
let branchGen = 0
let logGen = 0
let opsGen = 0

const TERMINAL = ['ok', 'failed', 'denied', 'error']
const ACTIVE = ['awaiting', 'running']

// waitOpId：正在等它落地的那个 op。轮询只盯它，终态一到就收工并刷新仓库。
let waitOpId = ''
let pollTimer = null
// waitDeadline：等一个 op 的上限。列表里迟迟不出现它（后端还没登记）也不能
// 无限轮询下去，超时就放弃等待并把状态留在操作记录里。
let waitDeadline = 0

const files = computed(() => status.value?.files || [])
const localBranches = computed(() => branches.value.filter(b => !b.remote))
const remoteBranches = computed(() => branches.value.filter(b => b.remote))
const activeOps = computed(() => ops.value.filter(o => ACTIVE.includes(o.state)))

function isActiveOp(o) {
  return ACTIVE.includes(o?.state)
}

async function loadStatus() {
  const th = props.thread
  if (!th?.id) return
  const g = ++statusGen
  try {
    const r = await api('GET', `/api/acp/threads/${th.id}/git/status`)
    if (g !== statusGen) return
    status.value = r.status || null
  } catch (e) {
    if (g !== statusGen) return
    error.value = e.message || String(e)
  }
}

async function loadBranches() {
  const th = props.thread
  if (!th?.id) return
  const g = ++branchGen
  try {
    const r = await api('GET', `/api/acp/threads/${th.id}/git/branches`)
    if (g !== branchGen) return
    branches.value = r.branches || []
  } catch (e) {
    if (g !== branchGen) return
    error.value = e.message || String(e)
  }
}

async function loadLog() {
  const th = props.thread
  if (!th?.id) return
  const g = ++logGen
  try {
    const r = await api('GET', `/api/acp/threads/${th.id}/git/log?limit=30`)
    if (g !== logGen) return
    commits.value = r.commits || []
  } catch (e) {
    if (g !== logGen) return
    error.value = e.message || String(e)
  }
}

async function loadOps() {
  const th = props.thread
  if (!th?.id) return
  const g = ++opsGen
  try {
    const r = await api('GET', `/api/acp/threads/${th.id}/git/ops`)
    if (g !== opsGen) return
    ops.value = r.ops || []
    // 盯着的那条 op 到终态了就收工：停轮询、刷新仓库、把结果讲出来。
    if (waitOpId) {
      const mine = ops.value.find(o => o.id === waitOpId)
      if (mine && TERMINAL.includes(mine.state)) {
        waitOpId = ''
        stopPolling()
        onOpFinished(mine)
      }
    }
    ensurePolling()
  } catch (e) {
    if (g !== opsGen) return
    error.value = e.message || String(e)
  }
}

// ensurePolling：还有待批/在跑的 op，或正在等某条 op 落地时才轮询，
// 都结束了就停，不空转网络。
function ensurePolling() {
  if (waitOpId && Date.now() > waitDeadline) {
    // 等太久了：不再追这条 op，避免面板一直转圈
    waitOpId = ''
  }
  if (activeOps.value.length > 0 || waitOpId) {
    if (!pollTimer) pollTimer = setInterval(loadOps, Math.max(50, props.pollInterval || 1500))
    return
  }
  stopPolling()
}

function stopPolling() {
  if (pollTimer) {
    clearInterval(pollTimer)
    pollTimer = null
  }
}

function onOpFinished(op) {
  if (op.state === 'ok') {
    notice.value = op.message || `${opLabel(op.op)}成功`
    noticeKind.value = 'ok'
  } else {
    notice.value = op.message || `${opLabel(op.op)}没成功：${stateLabel(op.state)}`
    noticeKind.value = 'err'
  }
  refreshRepo()
}

// 一次写操作落地后仓库的样子一定变了：状态、分支、历史都要重拉。
async function refreshRepo() {
  await loadStatus()
  if (status.value?.isRepo) {
    await Promise.all([loadBranches(), loadLog()])
  }
}

async function loadAll() {
  const th = props.thread
  if (!th?.id) return
  loading.value = true
  error.value = ''
  await loadStatus()
  // 不是仓库就没有分支和历史可言，也就不发这两个请求——省一次往返，也少一份误导。
  if (status.value?.isRepo) {
    await Promise.all([loadBranches(), loadLog()])
  }
  await loadOps()
  loading.value = false
}

// resetState：切会话时把上一个仓库的一切痕迹清干净（分支列表、轮询、等待中的 op）。
function resetState() {
  statusGen++
  branchGen++
  logGen++
  opsGen++
  stopPolling()
  waitOpId = ''
  waitDeadline = 0
  status.value = null
  branches.value = []
  commits.value = []
  ops.value = []
  loading.value = false
  error.value = ''
  notice.value = ''
  noticeKind.value = 'info'
  busy.value = false
  newBranch.value = ''
  commitMsg.value = ''
  expanded.value = {}
}

async function submitOp(op, arg) {
  const th = props.thread
  if (!th?.id || busy.value) return
  const value = String(arg ?? '').trim()
  if (!value) return
  busy.value = true
  error.value = ''
  try {
    const r = await api('POST', `/api/acp/threads/${th.id}/git/ops`, { op, arg: value })
    notice.value = r.message || '已提交，等你在审批卡上批准后执行'
    noticeKind.value = 'info'
    waitOpId = r.op?.id || ''
    waitDeadline = Date.now() + 120000
    await loadOps()
  } catch (e) {
    error.value = e.message || String(e)
  } finally {
    busy.value = false
  }
}

async function stopOp(o) {
  const th = props.thread
  if (!th?.id || !o) return
  error.value = ''
  try {
    const r = await api('POST', `/api/acp/threads/${th.id}/git/ops/${encodeURIComponent(o.id)}/stop`)
    notice.value = r.message || '已停止这次操作'
    noticeKind.value = 'info'
    await loadOps()
  } catch (e) {
    error.value = e.message || String(e)
  }
}

function toggleBody(hash) {
  expanded.value = { ...expanded.value, [hash]: !expanded.value[hash] }
}

// statusLabel：后端给的是两字符 porcelain 位（X=暂存区，Y=工作区），
// 这里翻译成中文；? 是未跟踪，冲突单独说。
function statusLabel(s) {
  const xy = String(s || '')
  if (xy.includes('?')) return '未跟踪'
  if (xy.includes('U')) return '冲突'
  if (xy.includes('A')) return '新增'
  if (xy.includes('D')) return '删除'
  if (xy.includes('M')) return '修改'
  return '变更'
}

function statusClass(s) {
  const xy = String(s || '')
  if (xy.includes('?')) return 'is-untracked'
  if (xy.includes('U')) return 'is-conflict'
  if (xy.includes('A')) return 'is-added'
  if (xy.includes('D')) return 'is-deleted'
  return 'is-modified'
}

function opLabel(op) {
  return ({ checkout: '切换分支', createBranch: '新建分支', stage: '暂存',
    unstage: '取消暂存', commit: '提交' })[op] || op
}

function stateLabel(s) {
  return ({ awaiting: '待批准', running: '执行中', ok: '成功',
    failed: '失败', denied: '未批准', error: '未执行' })[s] || s
}

// relTime：提交时间用相对时间更易读，超过一周再退回日期。
function relTime(s) {
  if (!s) return ''
  const t = new Date(s).getTime()
  if (!Number.isFinite(t)) return ''
  const diff = Date.now() - t
  if (diff < 0) return '刚刚'
  if (diff < 60000) return '刚刚'
  if (diff < 3600000) return Math.floor(diff / 60000) + ' 分钟前'
  if (diff < 86400000) return Math.floor(diff / 3600000) + ' 小时前'
  if (diff < 7 * 86400000) return Math.floor(diff / 86400000) + ' 天前'
  return new Date(t).toLocaleDateString()
}

onMounted(() => {
  if (props.thread?.id) loadAll()
})

onUnmounted(() => stopPolling())

watch(() => props.thread?.id, () => {
  resetState()
  if (props.thread?.id) loadAll()
})
</script>

<template>
  <section class="wg" aria-label="Git 管理">
    <div v-if="!thread?.id" class="wg-empty">
      <svg width="30" height="30" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"
           stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
        <line x1="6" y1="3" x2="6" y2="15"/><circle cx="18" cy="6" r="3"/><circle cx="6" cy="18" r="3"/>
        <path d="M18 9a9 9 0 0 1-9 9"/>
      </svg>
      <b>没有会话</b>
      <p class="meta">先选一条会话，才能看它的 Git 状态。</p>
    </div>

    <template v-else>
      <header class="wg-head">
        <div v-if="loading && !status" class="wg-status">读取中…</div>
        <template v-else-if="status && !status.isRepo">
          <div class="wg-note" role="status">{{ status.note || '这个目录不是 git 仓库' }}</div>
        </template>
        <template v-else-if="status">
          <div class="wg-head-row">
            <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"
                 stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
              <line x1="6" y1="3" x2="6" y2="15"/><circle cx="18" cy="6" r="3"/><circle cx="6" cy="18" r="3"/>
              <path d="M18 9a9 9 0 0 1-9 9"/>
            </svg>
            <b class="wg-branch" :title="status.branch">{{ status.branch || '未知分支' }}</b>
            <span v-if="status.detached" class="wg-tag">分离 HEAD</span>
            <span v-if="status.upstream" class="meta">上游 {{ status.upstream }}</span>
            <span v-if="status.ahead" class="wg-cnt is-ahead">领先 {{ status.ahead }}</span>
            <span v-if="status.behind" class="wg-cnt is-behind">落后 {{ status.behind }}</span>
          </div>
          <div class="wg-counts">
            <span class="wg-cnt">暂存 {{ status.staged || 0 }}</span>
            <span class="wg-cnt">修改 {{ status.modified || 0 }}</span>
            <span class="wg-cnt">未跟踪 {{ status.untracked || 0 }}</span>
            <span class="wg-cnt">删除 {{ status.deleted || 0 }}</span>
            <span v-if="status.conflicted" class="wg-cnt is-conflict">冲突 {{ status.conflicted }}</span>
          </div>
        </template>
        <div v-else class="wg-status">还没读到仓库状态。</div>
      </header>

      <p v-if="error" class="wg-status is-error" role="alert">{{ error }}</p>
      <p v-if="notice" class="wg-notice" :class="'is-' + noticeKind" role="status">{{ notice }}</p>

      <template v-if="status?.isRepo">
        <section class="wg-sec" aria-label="改动文件">
          <h3 class="wg-sec-title">改动文件 <span class="meta">{{ files.length }}</span></h3>
          <div v-if="loading && !files.length" class="wg-status">读取中…</div>
          <div v-else-if="!files.length" class="wg-status">工作区是干净的，没有改动文件。</div>
          <div v-for="f in files" :key="f.path" class="wg-file">
            <span class="wg-badge" :class="statusClass(f.status)" :title="'git 状态 ' + f.status">
              {{ statusLabel(f.status) }}
            </span>
            <span class="wg-path" :title="f.path">{{ f.path }}</span>
            <button v-if="f.stage === 'staged'" type="button" class="btn sm"
                    :aria-label="'取消暂存 ' + f.path" title="取消暂存" :disabled="busy"
                    @click="submitOp('unstage', f.path)">取消暂存</button>
            <button v-else-if="f.stage === 'unstaged' || f.stage === 'both'" type="button" class="btn sm"
                    :aria-label="'暂存 ' + f.path" title="暂存" :disabled="busy"
                    @click="submitOp('stage', f.path)">暂存</button>
          </div>
        </section>

        <section class="wg-sec" aria-label="分支">
          <h3 class="wg-sec-title">分支</h3>
          <div v-if="!localBranches.length && !remoteBranches.length" class="wg-status">还没有分支信息。</div>
          <div v-for="b in localBranches" :key="'l-' + b.name" class="wg-branch-row" :class="{ on: b.current }">
            <span class="wg-branch-name" :title="b.name">{{ b.name }}</span>
            <span v-if="b.current" class="wg-tag is-current">当前</span>
            <span v-if="b.last" class="meta mono">{{ b.last }}</span>
            <span class="meta">{{ relTime(b.date) }}</span>
            <button v-if="!b.current" type="button" class="btn sm"
                    :aria-label="'切换到分支 ' + b.name" title="切换到这个分支" :disabled="busy"
                    @click="submitOp('checkout', b.name)">切换</button>
          </div>
          <div v-for="b in remoteBranches" :key="'r-' + b.name" class="wg-branch-row is-remote">
            <span class="wg-branch-name" :title="b.name">{{ b.name }}</span>
            <span class="wg-tag">远端</span>
            <span v-if="b.last" class="meta mono">{{ b.last }}</span>
            <span class="meta">{{ relTime(b.date) }}</span>
          </div>
          <p class="wg-hint">远端分支只读：这里只能切到本地分支。</p>
        </section>

        <section class="wg-sec" aria-label="新建分支">
          <h3 class="wg-sec-title">新建分支</h3>
          <div class="wg-row">
            <input v-model="newBranch" aria-label="新分支名称" placeholder="feature/xxx"
                   :disabled="busy" @keyup.enter="submitOp('createBranch', newBranch.trim())">
            <button type="button" class="btn sm primary" :disabled="!newBranch.trim() || busy"
                    aria-label="新建并切换分支" title="新建并切换到这个分支"
                    @click="submitOp('createBranch', newBranch.trim())">新建并切换</button>
          </div>
          <p class="wg-hint">写操作要先在审批卡上批准，才会在仓库里落地。</p>
        </section>

        <section class="wg-sec" aria-label="提交历史">
          <h3 class="wg-sec-title">提交历史 <span class="meta">{{ commits.length }}</span></h3>
          <div v-if="!commits.length" class="wg-status">还没有提交历史。</div>
          <div v-for="c in commits" :key="c.hash" class="wg-commit">
            <button type="button" class="wg-commit-head" :aria-expanded="!!expanded[c.hash]"
                    :aria-label="'提交 ' + (c.subject || c.short)"
                    @click="toggleBody(c.hash)">
              <span class="wg-commit-sub">{{ c.subject || '(没有提交信息)' }}</span>
              <span class="wg-commit-meta mono">{{ c.short }} · {{ c.author }} · {{ relTime(c.date) }}</span>
            </button>
            <pre v-if="expanded[c.hash] && c.body" class="wg-out">{{ c.body }}</pre>
          </div>
        </section>

        <section class="wg-sec" aria-label="提交改动">
          <h3 class="wg-sec-title">提交</h3>
          <textarea v-model="commitMsg" rows="3" aria-label="提交信息" placeholder="写清楚这次改了什么"
                    :disabled="busy"></textarea>
          <div class="wg-row">
            <button type="button" class="btn sm primary" :disabled="!commitMsg.trim() || busy"
                    aria-label="提交改动" title="提交已暂存的改动"
                    @click="submitOp('commit', commitMsg.trim())">提交</button>
            <span class="meta">提交的是已暂存的改动。</span>
          </div>
        </section>

        <section class="wg-sec" aria-label="操作记录">
          <h3 class="wg-sec-title">操作记录</h3>
          <div v-if="!ops.length" class="wg-status">还没有提交过 Git 写操作。</div>
          <div v-for="o in ops" :key="o.id" class="wg-op">
            <div class="wg-op-head">
              <span class="wg-op-name">{{ opLabel(o.op) }}<template v-if="o.arg"> {{ o.arg }}</template></span>
              <span class="wg-state" :class="'st-' + o.state">{{ stateLabel(o.state) }}</span>
              <button v-if="isActiveOp(o)" type="button" class="btn sm danger"
                      :aria-label="'停止 ' + opLabel(o.op)" title="停止这次操作"
                      @click="stopOp(o)">停止</button>
            </div>
            <div v-if="o.message" class="wg-op-msg">{{ o.message }}</div>
            <pre v-if="o.output" class="wg-out">{{ o.output }}</pre>
          </div>
        </section>
      </template>
    </template>
  </section>
</template>

<style scoped>
/* Git 面板整体是一个纵向可滚的列：紧凑信息在上，细节往下展开。 */
.wg { display: flex; flex-direction: column; gap: 14px; padding: 12px; min-width: 0; }
.wg-head { display: flex; flex-direction: column; gap: 8px; padding: 10px 12px;
           border: 1px solid var(--border); border-radius: var(--radius-sm);
           background: var(--card); box-shadow: var(--shadow-sm); }
.wg-head-row { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; color: var(--fg); }
.wg-branch { font-size: 14px; }
.wg-counts { display: flex; gap: 8px; flex-wrap: wrap; }
.wg-cnt { font-size: 12px; color: var(--muted-fg); padding: 2px 8px; border-radius: 999px;
          background: var(--bg-subtle); }
.wg-cnt.is-ahead { color: var(--ok); }
.wg-cnt.is-behind { color: var(--warn); }
.wg-cnt.is-conflict { color: var(--danger); }
.wg-tag { font-size: 11.5px; color: var(--muted-fg); padding: 1px 7px; border-radius: 999px;
          border: 1px solid var(--border); }
.wg-tag.is-current { color: var(--accent-fg); background: var(--accent-subtle); border-color: transparent; }
.wg-note { font-size: 13px; color: var(--muted-fg); }
.wg-status { font-size: 12.5px; color: var(--muted-fg); }
.wg-status.is-error { color: var(--danger); }
.wg-notice { margin: 0; font-size: 12.5px; color: var(--muted-fg); padding: 8px 10px;
             border: 1px solid var(--border); border-radius: var(--radius-sm); background: var(--bg-subtle); }
.wg-notice.is-ok { color: var(--ok); border-color: var(--ok); background: var(--ok-subtle); }
.wg-notice.is-err { color: var(--danger); border-color: var(--danger-border); background: var(--danger-subtle); }
.wg-empty { display: flex; flex-direction: column; align-items: center; gap: 8px; padding: 36px 16px;
             text-align: center; color: var(--muted-fg); }
.wg-empty .meta { margin: 0; }
.wg-sec { display: flex; flex-direction: column; gap: 8px; padding: 10px 12px;
          border: 1px solid var(--border); border-radius: var(--radius-sm); background: var(--card); }
.wg-sec-title { margin: 0; font-size: 12.5px; font-weight: 600; color: var(--fg); }
.wg-file { display: flex; align-items: center; gap: 8px; padding: 4px 0; }
.wg-path { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
           font-size: 12.5px; color: var(--fg); }
.wg-badge { flex: 0 0 auto; font-size: 11px; padding: 1px 7px; border-radius: 999px;
            background: var(--bg-subtle); color: var(--muted-fg); }
.wg-badge.is-added { color: var(--ok); background: var(--ok-subtle); }
.wg-badge.is-deleted { color: var(--danger); background: var(--danger-subtle); }
.wg-badge.is-conflict { color: var(--danger); background: var(--danger-subtle); }
.wg-badge.is-untracked { color: var(--warn); background: var(--warn-subtle); }
.wg-branch-row { display: flex; align-items: center; gap: 8px; padding: 4px 0; font-size: 12.5px; }
.wg-branch-row.on .wg-branch-name { color: var(--accent-fg); font-weight: 600; }
.wg-branch-row.is-remote .wg-branch-name { color: var(--muted-fg); }
.wg-branch-name { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
                  color: var(--fg); }
.wg-row { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }
.wg-row input { flex: 1; min-width: 0; }
.wg-row textarea, .wg-sec textarea { width: 100%; box-sizing: border-box; resize: vertical;
                                     font: inherit; font-size: 12.5px; line-height: 1.5; }
.wg-hint { margin: 0; font-size: 11.5px; color: var(--faint-fg); }
.wg-commit { border-top: 1px solid var(--border); padding-top: 6px; }
.wg-commit:first-of-type { border-top: 0; padding-top: 0; }
.wg-commit-head { display: flex; flex-direction: column; align-items: flex-start; gap: 2px; width: 100%;
                   border: 0; background: transparent; padding: 2px 0; cursor: pointer; text-align: left;
                   font: inherit; color: inherit; }
.wg-commit-sub { font-size: 12.5px; color: var(--fg); overflow-wrap: anywhere; }
.wg-commit-meta { font-size: 11.5px; color: var(--faint-fg); }
.wg-op { border-top: 1px solid var(--border); padding-top: 6px; }
.wg-op:first-of-type { border-top: 0; padding-top: 0; }
.wg-op-head { display: flex; align-items: center; gap: 8px; }
.wg-op-name { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
              font-size: 12.5px; color: var(--fg); }
.wg-state { font-size: 11px; padding: 1px 7px; border-radius: 999px; background: var(--bg-subtle);
            color: var(--muted-fg); }
.wg-state.st-ok { color: var(--ok); background: var(--ok-subtle); }
.wg-state.st-failed, .wg-state.st-error { color: var(--danger); background: var(--danger-subtle); }
.wg-state.st-denied { color: var(--warn); background: var(--warn-subtle); }
.wg-state.st-running { color: var(--accent-fg); background: var(--accent-subtle); }
.wg-op-msg { margin-top: 4px; font-size: 12px; color: var(--muted-fg); overflow-wrap: anywhere; }
/* 命令输出按原样看：等宽 + 保留换行，别让长行把面板撑破。 */
.wg-out { margin: 6px 0 0; padding: 8px 10px; border-radius: var(--radius-xs); background: var(--bg-subtle);
          color: var(--fg); font-family: ui-monospace, "SF Mono", Menlo, Consolas, monospace;
          font-size: 12px; line-height: 1.5; white-space: pre-wrap; overflow-wrap: anywhere; max-height: 240px;
          overflow: auto; }
</style>
