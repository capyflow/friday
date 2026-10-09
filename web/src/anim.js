// GSAP 动效编排（移植自 design/modern-ui-preview 样例）。
// prefers-reduced-motion 下 animate 为 false，所有动效降级为静态。
import gsap from 'gsap'

export const animate =
  typeof window !== 'undefined' &&
  !window.matchMedia('(prefers-reduced-motion: reduce)').matches

export { gsap }

/* 极光光斑有机漂移（两主题共用结构；浅色下光斑 opacity:0 不可见） */
export function auroraDrift(scope = document) {
  if (!animate) return
  const moves = [[130, 70], [-110, -60], [-80, 90]]
  scope.querySelectorAll('.aurora i').forEach((b, i) => {
    gsap.to(b, {
      x: moves[i][0], y: moves[i][1], scale: 1.12,
      duration: 16 + i * 5, ease: 'sine.inOut',
      yoyo: true, repeat: -1, delay: -i * 4,
    })
  })
}

/* 外壳入场（登录成功后的侧栏 / 聊天头尾） */
export function shellIn() {
  if (!animate) return
  gsap.from('.brand', { x: -18, opacity: 0, duration: .5, ease: 'power2.out' })
  gsap.from('.nav-item', { x: -14, opacity: 0, duration: .4, stagger: .05,
    delay: .1, ease: 'power2.out' })
  gsap.from('.side-foot', { opacity: 0, duration: .5, delay: .45 })
  gsap.from('#chatHead', { y: -12, opacity: 0, duration: .45, delay: .15, ease: 'power2.out' })
  gsap.from('#chatFoot', { y: 18, opacity: 0, duration: .5, delay: .3, ease: 'power2.out' })
}

/* 登录卡入场 */
export function authIn() {
  if (!animate) return
  gsap.fromTo('.auth-logo', { scale: .4, opacity: 0, y: 14 },
    { scale: 1, opacity: 1, y: 0, duration: .7, ease: 'back.out(1.8)', clearProps: 'transform' })
  gsap.fromTo('.auth-box h1', { y: 14, opacity: 0 },
    { y: 0, opacity: 1, duration: .5, delay: .2, clearProps: 'opacity,transform' })
  gsap.fromTo('.auth-box .sub', { y: 12, opacity: 0 },
    { y: 0, opacity: 1, duration: .45, delay: .28, clearProps: 'opacity,transform' })
  gsap.fromTo('.auth-box .field', { y: 14, opacity: 0 },
    { y: 0, opacity: 1, duration: .4, stagger: .08, delay: .34, clearProps: 'opacity,transform' })
  gsap.fromTo('.auth-box .btn.primary', { y: 12, opacity: 0 },
    { y: 0, opacity: 1, duration: .45, delay: .52, ease: 'back.out(1.4)', clearProps: 'opacity,transform' })
  gsap.fromTo('.auth-foot', { opacity: 0 }, { opacity: 1, duration: .5, delay: .65 })
}

/* 视图切换：整体上浮 + 子元素错峰入场 */
export function viewIn(view) {
  if (!view) return
  if (!animate) { runCountUp(view); return }
  gsap.fromTo(view, { opacity: 0, y: 12 },
    { opacity: 1, y: 0, duration: .32, ease: 'power2.out', clearProps: 'opacity,transform' })
  const items = view.querySelectorAll('h2, .trace-bar, .cards .card, .panel, .ev, .wiki-toc, .wiki-reader')
  gsap.fromTo(items, { y: 16, opacity: 0 },
    { y: 0, opacity: 1, duration: .45, stagger: .05, delay: .06,
      ease: 'power3.out', clearProps: 'opacity,transform' })
  runCountUp(view)
}

/* 数字滚动（.v[data-count]，支持 decimals/suffix） */
export function runCountUp(scope) {
  if (!scope) return
  scope.querySelectorAll('[data-count]').forEach(el => {
    const target = parseFloat(el.dataset.count)
    if (Number.isNaN(target)) return
    const decimals = parseInt(el.dataset.decimals || '0')
    const suffix = el.dataset.suffix || ''
    const fmt = v => (decimals ? v.toFixed(decimals) : Math.round(v).toLocaleString('en-US')) + suffix
    if (!animate) { el.textContent = fmt(target); return }
    const obj = { v: 0 }
    gsap.to(obj, { v: target, duration: .9, delay: .25, ease: 'power2.out',
      onUpdate: () => { el.textContent = fmt(obj.v) } })
  })
}

/* 欢迎页：标题逐字 + 副标题 + 建议 chips（传字符 span 数组） */
export function welcomeIn(chars) {
  if (!animate) return
  gsap.set(chars, { yPercent: 110, opacity: 0 })
  gsap.fromTo('.welcome-logo', { scale: .4, opacity: 0, y: 12 },
    { scale: 1, opacity: 1, y: 0, duration: .8, ease: 'back.out(1.7)', clearProps: 'transform' })
  gsap.to(chars, { yPercent: 0, opacity: 1, duration: .6,
    stagger: .055, ease: 'back.out(1.6)', delay: .28 })
  gsap.fromTo('.welcome-sub', { y: 12, opacity: 0 },
    { y: 0, opacity: 1, duration: .5, delay: .5, ease: 'power2.out', clearProps: 'opacity,transform' })
  gsap.fromTo('.suggest', { y: 18, opacity: 0, scale: .94 },
    { y: 0, opacity: 1, scale: 1, duration: .45, stagger: .07, delay: .58,
      ease: 'back.out(1.5)', clearProps: 'opacity,transform' })
}

/* 气泡 / 打字态 / 弹层入场 */
export function bubbleIn(el) {
  if (!animate) return
  gsap.fromTo(el, { opacity: 0, y: 14, scale: .97 },
    { opacity: 1, y: 0, scale: 1, duration: .42, ease: 'back.out(1.4)', clearProps: 'opacity,transform' })
}
export function popIn(el) {
  if (!animate) return
  gsap.fromTo(el, { opacity: 0, y: 12, scale: .94 },
    { opacity: 1, y: 0, scale: 1, duration: .24, ease: 'back.out(1.5)',
      transformOrigin: 'left bottom', clearProps: 'opacity,transform' })
  gsap.fromTo(el.querySelectorAll('.pop-anim'), { opacity: 0, y: 6 },
    { opacity: 1, y: 0, duration: .17, stagger: .026, delay: .05,
      clearProps: 'opacity,transform' })
}
