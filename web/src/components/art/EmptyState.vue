<script setup>
/* Friday 空状态插画：单色细线，与 24 网格线性图标同族（viewBox 120 / stroke 1.5）。
   约束：一帧一个主体名词；accent 只落在引导元素上；虚线恒表示「尚未发生」；
   断开处用留白而非 fill 挖洞（深色 --card 半透明，挖洞会漏线）。 */
import { computed } from 'vue'

const props = defineProps({
  kind: { type: String, required: true },
  size: { type: [Number, String], default: 108 },
})

const FRAMES = {
  trace: `<path d="M18 96h84" opacity=".32" stroke-dasharray="2 6"/>
    <path class="dr" pathLength="1" d="M24 76l18-26 16 12 20-32 18 12"/>
    <circle class="ac" cx="42" cy="50" r="4"/>
    <circle class="ac pip" cx="42" cy="50" r="1.6"/>
    <circle cx="58" cy="62" r="3" opacity=".5"/>
    <circle cx="78" cy="30" r="3" opacity=".5"/>
    <circle cx="96" cy="42" r="6" opacity=".32" stroke-dasharray="3 4"/>`,

  todo: `<path class="dr" pathLength="1" d="M30 40a10 10 0 0 1 10-10h32l14 14v50a10 10 0 0 1-10 10H40a10 10 0 0 1-10-10z"/>
    <path d="M72 30v14h14" opacity=".32"/>
    <rect x="42" y="52" width="11" height="11" rx="3"/>
    <path class="ac" d="M44.5 57.5l2.5 2.5 5-6"/>
    <path d="M58 58h20" opacity=".5"/>
    <rect x="42" y="70" width="11" height="11" rx="3" opacity=".5"/>
    <path d="M58 76h14" opacity=".32"/>
    <rect x="42" y="88" width="11" height="11" rx="3" opacity=".32" stroke-dasharray="3 3"/>`,

  wiki: `<path d="M32 52v40a10 10 0 0 0 10 10h30" opacity=".32"/>
    <path class="dr" pathLength="1" d="M46 44a10 10 0 0 1 10-10h22l14 14v42a10 10 0 0 1-10 10H56a10 10 0 0 1-10-10z"/>
    <path d="M78 34v14h14" opacity=".32"/>
    <path d="M56 60h20M56 70h16M56 80h10" opacity=".5"/>
    <path class="ac" d="M66 34v16l4-3 4 3V36"/>
    <path d="M98 20v10M93 25h10" opacity=".32"/>`,

  wikiPick: `<path d="M18 34h18M18 46h18M18 58h12" opacity=".32"/>
    <path class="ac" d="M44 52h12m-5-5 5 5-5 5"/>
    <rect x="62" y="28" width="42" height="60" rx="10"/>
    <path d="M72 44h22M72 54h18M72 64h12" opacity=".5"/>
    <path d="M18 86h18" opacity=".32" stroke-dasharray="2 6"/>`,

  notif: `<path class="dr" pathLength="1" d="M60 24a22 22 0 0 1 22 22v14l6 10H32l6-10V46a22 22 0 0 1 22-22z"/>
    <path d="M52 84a8 8 0 0 0 16 0" opacity=".5"/>
    <path d="M60 16v8" opacity=".5"/>
    <path class="ac" d="M30 40a30 30 0 0 1 12-18" opacity=".5" stroke-dasharray="4 5"/>
    <path class="ac" d="M90 40a30 30 0 0 0-12-18" opacity=".5" stroke-dasharray="4 5"/>
    <path d="M20 96h80" opacity=".32" stroke-dasharray="2 6"/>`,

  mcp: `<rect x="16" y="48" width="28" height="28" rx="9"/>
    <path d="M24 58h12M24 66h7" opacity=".5"/>
    <rect x="76" y="48" width="28" height="28" rx="9" opacity=".32" stroke-dasharray="4 4"/>
    <path d="M44 62h6M70 62h6" opacity=".5"/>
    <circle class="ac" cx="60" cy="62" r="10"/>
    <path class="ac" d="M55.5 62h9M60 57.5v9"/>
    <path d="M22 92h26" opacity=".32" stroke-dasharray="2 6"/>`,

  usage: `<path d="M18 94h84" opacity=".32" stroke-dasharray="2 6"/>
    <rect x="28" y="68" width="14" height="26" rx="4" opacity=".5"/>
    <rect x="53" y="52" width="14" height="42" rx="4"/>
    <rect x="78" y="74" width="14" height="20" rx="4" opacity=".32" stroke-dasharray="4 4"/>
    <circle class="ac" cx="60" cy="46" r="4"/>
    <circle class="ac pip" cx="60" cy="46" r="1.6"/>`,

  session: `<path class="dr" pathLength="1" d="M54 62h20a12 12 0 0 0 12-12V36a12 12 0 0 0-12-12H36a12 12 0 0 0-12 12v14a12 12 0 0 0 12 12h4"/>
    <path d="M40 62v10l14-10"/>
    <path d="M36 40h30" opacity=".5"/>
    <circle class="ac pip" cx="42" cy="52" r="1.9"/>
    <circle class="ac pip" cx="50" cy="52" r="1.9"/>
    <circle class="ac pip" cx="58" cy="52" r="1.9"/>
    <path d="M34 80h50a10 10 0 0 0 10-10" opacity=".32" stroke-dasharray="4 4"/>`,

  skill: `<path class="dr" pathLength="1" d="M60 30c4 14 10 20 24 24-14 4-20 10-24 24-4-14-10-20-24-24 14-4 20-10 24-24z"/>
    <path class="ac" d="M88 74c1.4 5.6 3.8 8 9.4 9.6-5.6 1.6-8 4-9.4 9.6-1.6-5.6-4-8-9.6-9.6 5.6-1.4 8-4 9.6-9.4z" opacity=".5"/>
    <path d="M20 104h44" opacity=".32" stroke-dasharray="2 6"/>`,

  schedule: `<circle cx="52" cy="52" r="24"/>
    <path class="dr" pathLength="1" d="M52 40v12l9 6"/>
    <circle cx="52" cy="52" r="32" opacity=".32" stroke-dasharray="3 7"/>
    <circle class="ac" cx="52" cy="20" r="4"/>
    <path d="M20 100h36" opacity=".32" stroke-dasharray="2 6"/>`,

  error: `<path d="M36 80a26 26 0 0 1 48 0" opacity=".32" stroke-dasharray="4 5"/>
    <path d="M46 88a16 16 0 0 1 28 0" opacity=".5" stroke-dasharray="4 5"/>
    <circle cx="60" cy="96" r="3.5"/>
    <circle class="dg" cx="60" cy="42" r="18"/>
    <path class="dg" d="M60 33v11"/>
    <path class="dg" d="M60 49.5v.5"/>`,
}

const frame = computed(() => FRAMES[props.kind] || '')
const px = computed(() => `${Number(props.size)}px`)
</script>

<template>
  <div class="empty-art">
    <span class="art" :style="{ width: px, height: px }">
      <svg viewBox="0 0 120 120" aria-hidden="true" v-html="frame"></svg>
    </span>
    <div class="txt"><slot /></div>
  </div>
</template>
