<script setup>
// DirPickerDialog 是共用的目录选择弹窗：逐层点选或在路径栏直接粘贴回车跳转，
// 绝对路径由后端返回（浏览器直访拿不到目录句柄，所以不用 showDirectoryPicker）。
// 工作区页（给会话选主目录、给工作区加源文件夹）与工作模式的聊天页（给新会话
// 选项目目录）都走这一个，别再各写一份列表与状态。
//
// 用法：父组件 ref 上调 open(落点回调, 起始路径)，落点回调拿到选中的绝对路径。
import { computed, reactive } from 'vue'
import { api } from '../api.js'
import { toast } from '../store.js'

const picker = reactive({
  open: false, path: '', parent: '', input: '', dirs: [], loading: false, showHidden: false,
})
let onPick = null

// 点开头的隐藏目录默认不展示，弹窗内按钮切换；过滤在前端做，切换不用重新请求
const visibleDirs = computed(() =>
  picker.showHidden ? picker.dirs : picker.dirs.filter(d => !d.name.startsWith('.')))

async function loadDirs(path) {
  picker.loading = true
  try {
    const r = await api('GET', '/api/acp/dirs' + (path ? '?path=' + encodeURIComponent(path) : ''))
    picker.path = r.path
    picker.parent = r.parent || ''
    picker.input = r.path
    picker.dirs = r.dirs || []
  } catch (e) { toast('读取目录失败：' + e.message, true) } finally { picker.loading = false }
}

function submitDirPath() {
  if (picker.input.trim()) loadDirs(picker.input.trim())
}

function close() { picker.open = false }

function choose() {
  const pick = onPick
  onPick = null
  close()
  if (pick && picker.path) pick(picker.path)
}

function open(cb, startPath) {
  onPick = cb
  picker.open = true
  picker.showHidden = false
  loadDirs(startPath || '')
}

defineExpose({ open, close })
</script>

<template>
  <Teleport to="body">
    <div v-if="picker.open" class="dlg-mask" @click.self="close">
      <div class="dlg" role="dialog" aria-modal="true" aria-label="选择工作目录" @keydown.esc="close">
        <!-- .dlg 是横向 flex 外壳，纵向堆叠靠 .dlg-form（同 AutoView 的弹窗结构） -->
        <div class="dlg-form">
          <header class="dlg-head">
            <h3 class="dlg-title">选择工作目录</h3>
            <button type="button" class="set-close" aria-label="关闭目录选择" @click="close">
              <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2"
                   stroke-linecap="round" aria-hidden="true"><path d="M18 6 6 18M6 6l12 12"/></svg>
            </button>
          </header>
          <div class="dlg-body">
            <div class="ag-dir-bar">
              <input v-model="picker.input" aria-label="目录路径" placeholder="输入路径后回车跳转"
                     :disabled="picker.loading" @keydown.enter="submitDirPath">
              <button type="button" class="ib-btn" title="上一级" aria-label="上一级"
                      :disabled="!picker.parent || picker.loading" @click="loadDirs(picker.parent)">
                <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
                     stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 19V5M5 12l7-7 7 7"/></svg>
              </button>
              <button type="button" class="ib-btn ag-dir-show" :class="{ active: picker.showHidden }"
                      :title="picker.showHidden ? '不显示隐藏目录' : '显示隐藏目录'"
                      :aria-label="picker.showHidden ? '不显示隐藏目录' : '显示隐藏目录'"
                      :aria-pressed="picker.showHidden" @click="picker.showHidden = !picker.showHidden">
                <svg v-if="picker.showHidden" width="18" height="18" viewBox="0 0 24 24" fill="none"
                     stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
                  <path d="M17.94 17.94A10.07 10.07 0 0 1 12 20c-7 0-11-8-11-8a18.45 18.45 0 0 1 5.06-5.94M9.9 4.24A9.12 9.12 0 0 1 12 4c7 0 11 8 11 8a18.5 18.5 0 0 1-2.16 3.19m-6.72-1.07a3 3 0 1 1-4.24-4.24"/>
                  <path d="M1 1l22 22"/>
                </svg>
                <svg v-else width="18" height="18" viewBox="0 0 24 24" fill="none"
                     stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
                  <path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/>
                </svg>
              </button>
            </div>
            <ul class="ag-dirlist" aria-label="子目录列表">
              <li v-for="d in visibleDirs" :key="d.path">
                <button type="button" @click="loadDirs(d.path)">{{ d.name }}</button>
              </li>
              <li v-if="!visibleDirs.length && !picker.loading" class="meta ag-dir-empty">
                {{ picker.dirs.length ? '（只有隐藏子目录，可点上方按钮显示）' : '（没有子目录）' }}
              </li>
            </ul>
          </div>
          <footer class="dlg-foot">
            <span class="meta ag-dir-cur" :title="picker.path">{{ picker.path }}</span>
            <button class="btn sm primary" :disabled="picker.loading || !picker.path" @click="choose">使用此目录</button>
            <button class="btn sm" @click="close">取消</button>
          </footer>
        </div>
      </div>
    </div>
  </Teleport>
</template>
