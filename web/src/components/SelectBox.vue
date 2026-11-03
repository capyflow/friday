<script setup>
// 全站唯一的下拉框：原生 <select> 的弹出列表由操作系统渲染，无法用 CSS 定制，
// 这里自绘弹出层保证与全局描边/浅底风格一致。约定：新增下拉一律用本组件，
// 不要再写原生 <select>（style.css 里的 appearance 兜底只是防漏网，不是许可）。
// 弹出层 Teleport 到 body + fixed 定位：避免被表格/抽屉 overflow:hidden 裁切；
// 页面滚动/缩放时直接收起（同时也是无障碍约定行为）。
// 用法与原生 select 的对应关系：
//   v-model            → modelValue；选项 [{ value, label, disabled? }]（value 可为数字）
//   <option disabled>  → 选项上的 disabled：列表里照常列出但不可选，键盘导航会跳过
//   @change            → 用 :model-value + @update:model-value 自己收口（见 ToolsSection）
//   <label for="x">    → id 透传到触发按钮上
import { computed, nextTick, onBeforeUnmount, onMounted, ref, useAttrs } from 'vue'

defineOptions({ inheritAttrs: false })

const props = defineProps({
  modelValue: { type: [String, Number], default: '' },
  options: { type: Array, default: () => [] },   // [{ value, label, disabled? }]
  compact: { type: Boolean, default: false },    // 表格/抽屉内小尺寸
  disabled: { type: Boolean, default: false },
  ariaLabel: { type: String, default: '' },
})
const emit = defineEmits(['update:modelValue'])

const attrs = useAttrs()
// class/style 落在外层包装上（它才是布局盒），其余属性（id、data-*…）交给触发按钮。
const passThrough = computed(() => {
  const { class: _c, style: _s, ...rest } = attrs
  return rest
})

const root = ref(null)
const trigger = ref(null)
const pop = ref(null)
const open = ref(false)
const activeIdx = ref(-1)
const popStyle = ref({})

let seq = 0
const instId = ++seq
const optId = i => `selbox-${instId}-opt-${i}`

const isOn = o => o.value === props.modelValue
const selectable = o => !o.disabled
const label = computed(() => {
  const hit = props.options.find(isOn)              // 按值命中：值是谁就显示谁（含禁用项）
  if (hit) return hit.label
  const first = props.options.find(selectable)      // 没值时回落到第一个可选档
  return first ? first.label : (props.options[0]?.label ?? '')
})

function choose(o) {
  if (!o || o.disabled) return
  emit('update:modelValue', o.value)
  close()
  trigger.value && trigger.value.focus()
}

async function openPop() {
  if (props.disabled || open.value) return
  const rect = trigger.value.getBoundingClientRect()
  activeIdx.value = Math.max(0, props.options.findIndex(isOn))
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

// 上下键只在「可选」项之间走，禁用项直接跨过去；一圈都没有可选项就原地不动。
function nav(delta) {
  const n = props.options.length
  if (!n) return
  let i = activeIdx.value
  for (let step = 0; step < n; step++) {
    i = (i + delta + n) % n
    if (selectable(props.options[i])) { activeIdx.value = i; break }
  }
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
  <span ref="root" class="selbox" :class="[{ open, compact }, attrs.class]" :style="attrs.style">
    <button ref="trigger" v-bind="passThrough" type="button" class="selbox-trigger" :disabled="disabled"
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
              class="selbox-opt" :class="{ selected: isOn(o), active: i === activeIdx }"
              :disabled="!!o.disabled" :aria-selected="isOn(o)" :aria-disabled="!!o.disabled || undefined"
              @pointerenter="!o.disabled && (activeIdx = i)" @click="choose(o)">
        {{ o.label }}
      </button>
    </div>
  </Teleport>
</template>
