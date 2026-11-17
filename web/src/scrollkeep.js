// scrollkeep.js：底栏（输入框）高度变化时，把滚动位置跟着挪一挪。
//
// 输入框随打字自动变高 → 底栏变高 → 滚动区被压矮。如果不管，贴底要等下一次
// 内容更新才补上：中间对话卡在旧位置，随后又被顶上去——打字时看着上下抖。
// 这里在底栏高度变化的当下就把内容底部重新贴到视口底部：对话跟着输入框一起
// 平移（输入框长高多少，对话就等量上移多少），新回复始终露在外面，而且这一步
// 是连续的，不会攒到下一次贴底才突然跳一下。
//
// 用户正在翻历史时不介入：底栏变高只是少露一点底部，不该把他拽回底部。
//
// 用法：createFootScrollKeep(() => scrollerEl, () => footEl)，返回清理函数。
// 老 WebView 没有 ResizeObserver 时退化为不做任何事（不做猜测式补偿）。

export function createFootScrollKeep(scroller, foot) {
  const pick = v => (typeof v === 'function' ? v() : v)
  if (typeof ResizeObserver === 'undefined') return () => {}
  let last = -1
  const ro = new ResizeObserver(() => {
    const s = pick(scroller)
    const f = pick(foot)
    if (!s || !f) return
    const h = f.getBoundingClientRect().height
    if (last < 0) { last = h; return } // 第一次只记基线
    const d = h - last
    last = h
    if (!d) return
    // 隐藏量（内容底部到视口底部的距离）只比刚才多了 d ⇒ 刚才还贴在底部
    // ⇒ 继续贴。对话因此跟着输入框等量平移，而不是等下一次内容更新才跳。
    const dist = s.scrollHeight - s.scrollTop - s.clientHeight
    if (dist > d + 2) return // 用户在翻历史，别拽他
    s.scrollTop = s.scrollHeight
  })
  const target = pick(foot)
  if (target) ro.observe(target)
  return () => ro.disconnect()
}
