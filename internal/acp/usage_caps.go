package acp

// usageCaps 说明某个 agent 的真实 token 用量从哪儿取。
//
// ACP 标准只定义了 PromptResponse.Usage（轮末一次性上报），但各家实现参差：
//   - 标准实现：轮末 usage 里有 inputTokens/outputTokens（codex、zcode 还带
//     cachedReadTokens/cachedWriteTokens）；
//   - 非标准扩展：轮末什么都不给，只在 session/update 的 usage_update._meta.usage
//     里塞模型原始用量，而且一轮里每次模型调用各报一条（codebuddy-code）；
//   - 两边都没有：usage_update 只有上下文 used/size 和 cost（minimax-code 实测，
//     它的 token 汇总只在内部 /usage 斜杠命令里，没有对应的 ACP 方法）。
//
// 表里只列「需要特殊处理」的 agent；不在表里的按标准路径读 PromptResponse.Usage，
// 没上报就自然没有数据（不猜、不估算）。
//
// 新增 agent 前先实测一轮（起 agent 走 initialize → session/new → session/prompt，
// 看 prompt 响应有没有 usage、usage_update 的 _meta 有没有 usage），不要凭文档推断。
//
// 实测记录（2026-11-10，本机 adapter 版本）：
//
//	codex          codex-acp 2.2.2         prompt.usage ✅（含 cachedReadTokens）
//	zcode          zcode-acp-server        prompt.usage ✅（含 cachedRead/Write）
//	codebuddy-code 2.179.3                 _meta.usage ✅（prompt_tokens/completion_tokens/
//	                                       prompt_cache_hit_tokens，逐模型调用）
//	minimax-code   0.7.4                   ❌ 只有 used/size/cost
//	claude         0.16.1                  ❌ 响应里没有 usage（源码亦无）
//	opencode       opencode acp            ❌ 该版本 acp 是 HTTP 服务，不是 stdio 协议端
//	gemini         gemini --experimental-acp 未验证（需要登录态）
type usageCaps struct {
	// meta 为真时，从 usage_update._meta.usage 里累加真实用量（非标准扩展）。
	meta bool
}

// usageCapsTable 是「用量在非标准位置」的白名单，键是配置里的 agent name。
var usageCapsTable = map[string]usageCaps{
	"codebuddy-code": {meta: true},
}

// usageCapsFor 返回该 agent 的取数方式；不在表里 = 只认标准 PromptResponse.Usage。
func usageCapsFor(agent string) usageCaps {
	return usageCapsTable[agent]
}
