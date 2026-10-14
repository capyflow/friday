<script setup>
// 描边风下拉框：原生 <select> 的弹出列表由操作系统渲染，无法用 CSS 定制，
// 这里自绘弹出层保证与全局墨线/硬投影风格一致。
// 弹出层 Teleport 到 body + fixed 定位：避免被表格 overflow:hidden 裁切；
// 页面滚动/缩放时直接收起（同时也是无障碍约定行为）。
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'

defineOptions({ inheritAttrs: false })

const props = defineProps({
  modelValue: { type: [String, Number], default: '' },
  options: { type: Array, default: () => [] },   // [{ value, label }]
  compact: { type: Boolean, default: false },    // 表格内小尺寸
  disabled: { type: Boolean, default: false },
  ariaLabel: { type: String, default: '' },
})
const emit = defineEmits(['update:modelValue'])

const root = ref(null)
const trigger = ref(null)
const pop = ref(null)
const open = ref(false)
const activeIdx = ref(-1)
const popStyle = ref({})

let seq = 0
const instId = ++seq
const optId = i => `selbox-${instId}-opt-${i}`

const label = computed(() => {
  const hit = props.options.find(o => o.value === props.modelValue)
  return hit ? hit.label : (props.options[0]?.label ?? '')
})

function choose(o) {
  emit('update:modelValue', o.value)
  close()
  trigger.value && trigger.value.focus()
}

async function openPop() {
  if (props.disabled || open.value) return
  const rect = trigger.value.getBoundingClientRect()
  activeIdx.value = Math.max(0, props.options.findIndex(o => o.value === props.modelValue))
  popStyle.value = { left: rect.left + 'px', top: rect.bottom + 6 + 'px', minWidth: rect.width + 'px' }
  open.value = true
  await nextTick()
  // 底部放不下时向上弹
  const h = pop.value ? pop.value.offsetHeight : 0
  if (rect.bottom + 6 + h > window.innerHeight) {
    popStyle.value = { ...popStyle.value, top: Math.max(6, rect.top - h - 6) + 'px' }
  }
}

function close() {
  if (!open.value) return
  open.value = false
  activeIdx.value = -1
}

function toggle() { open.value ? close() : openPop() }

function nav(delta) {
  const n = props.options.length
  if (!n) return
  activeIdx.value = (activeIdx.value + delta + n) % n
  nextTick(() => {
    const el = pop.value && pop.value.children[activeIdx.value]
    if (el) el.scrollIntoView({ block: 'nearest' })
  })
}

function onKeydown(e) {
  if (!open.value) {
    if (['ArrowDown', 'ArrowUp', 'Enter', ' '].includes(e.key)) { e.preventDefault(); openPop() }
    return
  }
  switch (e.key) {
    case 'ArrowDown': e.preventDefault(); nav(1); break
    case 'ArrowUp': e.preventDefault(); nav(-1); break
    case 'Home': e.preventDefault(); activeIdx.value = 0; break
    case 'End': e.preventDefault(); activeIdx.value = props.options.length - 1; break
    case 'Enter': case ' ':
      e.preventDefault()
      if (activeIdx.value >= 0) choose(props.options[activeIdx.value])
      break
    case 'Escape': case 'Tab':
      // 列表展开时这两个键只收起列表，不冒泡给弹窗的关闭逻辑
      e.preventDefault(); e.stopPropagation(); close(); break
  }
}

function onDocPointerdown(e) {
  if (!open.value) return
  if (root.value && root.value.contains(e.target)) return
  if (pop.value && pop.value.contains(e.target)) return
  close()
}
function onScrollOrResize() { if (open.value) close() }

onMounted(() => {
  document.addEventListener('pointerdown', onDocPointerdown)
  window.addEventListener('scroll', onScrollOrResize, true)
  window.addEventListener('resize', onScrollOrResize)
})
onBeforeUnmount(() => {
  document.removeEventListener('pointerdown', onDocPointerdown)
  window.removeEventListener('scroll', onScrollOrResize, true)
  window.removeEventListener('resize', onScrollOrResize)
})
</script>

<template>
  <span ref="root" class="selbox" :class="{ open, compact }" :style="$attrs.style">
    <button ref="trigger" type="button" class="selbox-trigger" :disabled="disabled"
            :aria-label="ariaLabel || undefined" aria-haspopup="listbox" :aria-expanded="open"
            :aria-activedescendant="open && activeIdx >= 0 ? optId(activeIdx) : undefined"
            @click="toggle" @keydown="onKeydown">
      <span class="selbox-label">{{ label }}</span>
      <svg class="selbox-chev" width="14" height="14" viewBox="0 0 24 24" fill="none"
           stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
        <polyline points="6 9 12 15 18 9"/>
      </svg>
    </button>
  </span>
  <Teleport to="body">
    <div v-if="open" ref="pop" class="selbox-pop" :style="popStyle" role="listbox"
         :aria-label="ariaLabel || undefined">
      <button v-for="(o, i) in options" :id="optId(i)" :key="o.value" type="button" role="option"
              class="selbox-opt" :class="{ selected: o.value === modelValue, active: i === activeIdx }"
              :aria-selected="o.value === modelValue"
              @pointerenter="activeIdx = i" @click="choose(o)">
        {{ o.label }}
      </button>
    </div>
  </Teleport>
</template>
