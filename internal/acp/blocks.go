// blocks.go 是工作区会话的「时间线留档」：一轮里的思考、工具调用、正文除了
// 推给前端，还留一份在投影里，Friday 重启后按它把时间线还原出来。
//
// 为什么必须有：思考与工具调用只经事件总线（内存环形缓冲）推给前端，进程一
// 重启就全没了。投影里原本只留这一轮的回答正文，回放出来是「纯结果」——过程
// 行（N 次思考 · M 次工具调用）与工具明细整段消失，看着像 agent 没干过活。
//
// 形状与主聊天的历史块（internal/admin/ext.go 的 histBlock）刻意保持一致：
// 前端两处用同一个 histBlocks() 映射（web/src/blocks.js），不用各写一套。
package acp

import "strings"

// Block 是投影里的一条时间线内容块。
//
// kind：think（推理全文）| tool（工具调用）| text（回答正文片段）。
// text：think/text 是内容，tool 是参数摘要；out：tool 的结果摘要。
type Block struct {
	Kind   string `json:"kind"`
	Text   string `json:"text,omitempty"`
	Name   string `json:"name,omitempty"`
	Status string `json:"status,omitempty"`
	Out    string `json:"out,omitempty"`
}

// 留档的块类型。与主聊天历史同一套字面量，前端按这两个值分支。
const (
	BlockThink = "think"
	BlockTool  = "tool"
	BlockText  = "text"
)

// 留档的裁剪上限（与主聊天历史同口径：internal/admin/ext.go 的 histThinkMax /
// histTextMax / histToolMax）。留档是给回放看的线索，不是完整归档——思考与
// 正文截断，工具只留参数/结果摘要，别把投影撑成几倍大。
const (
	BlockThinkMax = 4000
	BlockTextMax  = 8000
	BlockToolMax  = 160
)

// blocks 返回本轮留档的时间线块（按到达顺序）。
//
// 收尾时还停在 run 的工具调用按「已执行完」收口（与实时时间线的
// closeRunningTools 同一口径）：done 帧到了就说明这些调用实际都跑过了，
// 不收口的话回放出来是一排永远转圈的圈。
func (s *turnSink) blocks() []Block {
	out := make([]Block, len(s.blocksLog))
	copy(out, s.blocksLog)
	for i := range out {
		if out[i].Kind == BlockTool && out[i].Status == "run" {
			out[i].Status = "ok"
		}
	}
	return out
}

// appendThinkBlock 把一段思考并进留档：连续的思考合成一块（与实时时间线的
// 思考块同形），超长按上限截断（留头，丢尾）。
func (s *turnSink) appendThinkBlock(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if n := len(s.blocksLog); n > 0 && s.blocksLog[n-1].Kind == BlockThink {
		s.blocksLog[n-1].Text = truncateRunes(s.blocksLog[n-1].Text+text, BlockThinkMax)
		return
	}
	s.blocksLog = append(s.blocksLog, Block{Kind: BlockThink, Text: truncateRunes(text, BlockThinkMax)})
}

// appendTextBlock 把一段正文并进留档：连续正文合成一块。
func (s *turnSink) appendTextBlock(text string) {
	if text == "" {
		return
	}
	if n := len(s.blocksLog); n > 0 && s.blocksLog[n-1].Kind == BlockText {
		s.blocksLog[n-1].Text = truncateRunes(s.blocksLog[n-1].Text+text, BlockTextMax)
		return
	}
	s.blocksLog = append(s.blocksLog, Block{Kind: BlockText, Text: truncateRunes(text, BlockTextMax)})
}

// appendToolBlock 把一次工具调用并进留档：同 toolCallId 的后续状态原地更新
// （run → ok/fail），不新起块——与实时时间线「一行 = 一次调用」同口径。
//
// detail 的口径跟实时一致：运行中是参数摘要，收尾是结果摘要（前端 blocks.js
// 的 pushToolStep 同规则：run 写 text，ok/fail 写 out）。
func (s *turnSink) appendToolBlock(id, name, detail, status string) {
	idx := -1
	if id != "" {
		if i, ok := s.toolBlockIdx[id]; ok {
			idx = i
		}
	}
	if idx < 0 {
		s.blocksLog = append(s.blocksLog, Block{Kind: BlockTool, Name: name, Status: status})
		idx = len(s.blocksLog) - 1
		if id != "" {
			s.toolBlockIdx[id] = idx
		}
	}
	b := &s.blocksLog[idx]
	if b.Name == "" {
		b.Name = name
	}
	if status == "run" {
		if detail != "" {
			b.Text = truncateRunes(detail, BlockToolMax)
		}
		if b.Status == "" {
			b.Status = "run"
		}
		return
	}
	if detail != "" {
		b.Out = truncateRunes(detail, BlockToolMax)
	}
	b.Status = status
}
