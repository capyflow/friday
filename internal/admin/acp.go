// acp.go 是智能体对话空间的管理后台接口：thread CRUD、SSE 对话流、停止、
// 配置读写与 agent 握手诊断。SSE 帧协议与 /api/chat 完全同形
// （delta/thinking/step/done，另有 status 生命周期占位帧），前端时间线渲染组件两端共用。
package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	vhttp "github.com/capyflow/vortexv3/server/http"

	"github.com/capyflow/friday/internal/acp"
	"github.com/capyflow/friday/internal/agent"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/logx"
	"github.com/capyflow/friday/internal/trace"
)

// handleAcpConfigGet 返回智能体配置（已归一化的默认值一并回显）。
func (s *Server) handleAcpConfigGet(ctx *vhttp.Context) error {
	cfg := config.AcpConfig{}
	if s.cfg != nil {
		cfg = s.cfg.Snapshot().Acp.Normalized()
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"acp": cfg})
}

// handleAcpConfigSet 保存智能体配置：校验 → 写回 config.yaml → Manager 热生效
// （配置变化的 agent 回收进程，上下文由下次打开时 session/load 尝试恢复）。
func (s *Server) handleAcpConfigSet(ctx *vhttp.Context) error {
	s.acpUpdateMu.Lock()
	defer s.acpUpdateMu.Unlock()
	if s.cfg == nil || s.configPath == "" {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "配置不可用"})
	}
	var req struct {
		Acp config.AcpConfig `json:"acp"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	next := req.Acp.Normalized()
	if err := next.Validate(); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	if err := s.cfg.Apply(func(n *config.Config) error {
		n.Acp = next
		return config.UpdateAcp(s.configPath, next)
	}); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	s.acp.Reload(next)
	enabled := "关闭"
	if next.Enabled {
		enabled = "开启"
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"message": "智能体配置已保存（功能" + enabled + "，" + fmt.Sprint(len(next.Agents)) + " 个 agent），并已写回 config.yaml",
		"acp":     next,
	})
}

// handleAcpThreads 返回会话列表（元数据，不含消息）、工作区列表与当前配置。
// 一个请求把页面骨架拉齐：左侧「工作区 → 会话」两级栏靠这一份数据渲染。
// 工作区回带 missingDirs（已被删除/移动的目录）——UI 要能明说哪个源文件夹没了。
func (s *Server) handleAcpThreads(ctx *vhttp.Context) error {
	cfg := config.AcpConfig{}
	if s.cfg != nil {
		cfg = s.cfg.Snapshot().Acp.Normalized()
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"workspaces": s.acp.WorkspaceList(),
		"threads":    s.acp.List(),
		"acp":        cfg,
	})
}

// handleAcpWorkspacesCreate 新建工作区：{"name":"proj-a","dirs":["~/code/a","~/shared"]}。
// dirs 一个都不给就回退到数据目录下的临时工作区目录（acp-sessions/<工作区ID>）。
func (s *Server) handleAcpWorkspacesCreate(ctx *vhttp.Context) error {
	var req struct {
		Name string   `json:"name"`
		Dirs []string `json:"dirs"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	ws, err := s.acp.CreateWorkspace(req.Name, req.Dirs)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"workspace": ws})
}

// handleAcpWorkspacesUpdate 改工作区的展示属性：POST /api/acp/workspaces/:id，
// 字段都可选、给哪个改哪个（缺省即不动，避免"改名字顺手把置顶洗掉"）：
//
//	{"name":"…"}          改名（留空回落主目录名）
//	{"pinned":true}       置顶/取消置顶
//	{"primaryPath":"…"}   把工作区里的某个目录提为主目录（必须是已有目录之一）
//	{"policy":"ask"}      默认审批档位：ask / readonly / auto
func (s *Server) handleAcpWorkspacesUpdate(ctx *vhttp.Context) error {
	var req struct {
		Name        *string `json:"name"`
		Pinned      *bool   `json:"pinned"`
		PrimaryPath *string `json:"primaryPath"`
		Policy      *string `json:"policy"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	id := ctx.GinContext().Param("id")
	if req.Name == nil && req.Pinned == nil && req.PrimaryPath == nil && req.Policy == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest,
			map[string]string{"error": `至少给一个字段：{"name": "..."}、{"pinned": true}、{"primaryPath": "目录"} 或 {"policy": "ask|readonly|auto"}`})
	}
	if req.Pinned != nil {
		if err := s.acp.SetPinned(id, *req.Pinned); err != nil {
			return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
		}
	}
	if req.PrimaryPath != nil {
		if _, err := s.acp.SetPrimaryDir(id, *req.PrimaryPath); err != nil {
			return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
		}
	}
	if req.Policy != nil {
		if err := s.acp.SetPolicy(id, *req.Policy); err != nil {
			return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
		}
	}
	if req.Name != nil {
		if err := s.acp.RenameWorkspace(id, *req.Name); err != nil {
			return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
		}
	}
	ws, _ := s.acp.GetWorkspace(id)
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"workspace": ws})
}

// handleAcpWorkspaceDirAdd 往工作区追加一个目录：{"path":"~/ref/b"}。
func (s *Server) handleAcpWorkspaceDirAdd(ctx *vhttp.Context) error {
	var req struct {
		Path string `json:"path"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil || strings.TrimSpace(req.Path) == "" {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": `请求体须为 {"path": "目录绝对路径"}`})
	}
	ws, err := s.acp.AddWorkspaceDir(ctx.GinContext().Param("id"), req.Path)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"workspace": ws})
}

// handleAcpWorkspaceDirRemove 从工作区移出一个目录（DELETE ?path=…）：只解除分组，
// 磁盘上的目录与文件不动；锚在它上面的会话继续用自己的主目录。
func (s *Server) handleAcpWorkspaceDirRemove(ctx *vhttp.Context) error {
	path := strings.TrimSpace(ctx.GinContext().Query("path"))
	if path == "" {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "缺少参数 path"})
	}
	ws, err := s.acp.RemoveWorkspaceDir(ctx.GinContext().Param("id"), path)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"workspace": ws})
}

// handleAcpWorkspaceDelete 删除工作区（连带它的会话；磁盘目录不动）。
// 回带被删掉的会话 id，前端好收起各自的缓冲与在途请求。
func (s *Server) handleAcpWorkspaceDelete(ctx *vhttp.Context) error {
	id := ctx.GinContext().Param("id")
	ids := s.acp.ThreadsInWorkspace(id)
	if !s.acp.DeleteWorkspace(id) {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "未找到该工作区"})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"message": "已删除该工作区及其会话", "threadIds": ids,
	})
}

// handleAcpThreadGet 返回单个 thread（含投影消息与该会话的目录视图）。
func (s *Server) handleAcpThreadGet(ctx *vhttp.Context) error {
	id := ctx.GinContext().Param("id")
	th, ok := s.acp.Get(id)
	if !ok {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "未找到该对话"})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"thread": th, "session": sessionDirs(s.acp, id),
	})
}

// sessionDirs 把 Manager.SessionDirs 折成前端要的 JSON 形状：主目录、工作区全部
// 目录、实际下发的扩展根，以及 agent 是否声明吃 additionalDirectories
// （known=false 表示进程不在场、能力未知，前端不下结论）。
func sessionDirs(m *acp.Manager, id string) map[string]any {
	cwd, dirs, extra, known, supported := m.SessionDirs(id)
	return map[string]any{"cwd": cwd, "dirs": dirs, "extra": extra,
		"known": known, "additionalDirectories": supported}
}

// handleAcpThreadRename 改会话标题：POST /api/acp/threads/:id {"title":"…"}。
// 手动标题优先；留空 = 回到自动（下一轮若仍为空会按那时的消息重新派生）。
func (s *Server) handleAcpThreadRename(ctx *vhttp.Context) error {
	var req struct {
		Title *string `json:"title"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil || req.Title == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest,
			map[string]string{"error": `请求体须为 {"title": "新标题"}（留空字符串 = 回到自动标题）`})
	}
	th, err := s.acp.RenameThread(ctx.GinContext().Param("id"), *req.Title)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"thread": th})
}

// handleAcpThreadCreate 新建会话：{"workspaceId":"…","agent":"codex","cwd":"可选（默认主目录）","title":"可选"}。
// agent 与主目录在此刻绑定，之后不可切换（D1）；换 = 再建一个会话。
func (s *Server) handleAcpThreadCreate(ctx *vhttp.Context) error {
	var req struct {
		WorkspaceID string `json:"workspaceId"`
		Agent       string `json:"agent"`
		Cwd         string `json:"cwd"`
		Title       string `json:"title"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	if strings.TrimSpace(req.WorkspaceID) == "" {
		return ctx.JsonResponse(vhttp.Codes.BadRequest,
			map[string]string{"error": "请先选一个工作区（左侧「工作区」里新建或点选）"})
	}
	th, err := s.acp.CreateSession(strings.TrimSpace(req.Agent), req.WorkspaceID, req.Cwd, req.Title)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"thread": th})
}

// handleAcpThreadDelete 删除 thread（连同投影记录与存活进程）。
func (s *Server) handleAcpThreadDelete(ctx *vhttp.Context) error {
	if !s.acp.Delete(ctx.GinContext().Param("id")) {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "未找到该对话"})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": "已删除该对话"})
}

// handleAcpStop 取消进行中的一轮（显式 session/cancel；上下文保留，可直接续聊）。
func (s *Server) handleAcpStop(ctx *vhttp.Context) error {
	if !s.acp.Stop(ctx.GinContext().Param("id")) {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "没有进行中的对话"})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": "已发送停止指令"})
}

// handleAcpOptionsGet 返回该 thread 会话的统一能力清单（GET
// /api/acp/threads/:id/options）。清单是归一化产物（configOptions/models/modes
// 三种来源收敛，见 internal/acp/options.go）：agent 不支持的能力直接缺席，前端
// 据此隐藏对应 chip。会话不在时后端会拉起 agent 建会话——清单是 agent 侧的
// 静态配置，进程随后照常空闲回收。
func (s *Server) handleAcpOptionsGet(ctx *vhttp.Context) error {
	id := ctx.GinContext().Param("id")
	caps, err := s.acp.Capabilities(id)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	// 清单这条路径必然已经拉起过进程：顺带回带目录生效情况（扩展根有没有被吃）。
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"capabilities": caps, "session": sessionDirs(s.acp, id),
	})
}

// handleAcpOptionsSet 切换某项能力的取值（POST /api/acp/threads/:id/options，
// {"id":"model","value":"…"}）：后端按能力的 source 路由到 set_config_option /
// set_model / set_mode，返回切换后的全量能力清单。切换即时生效于会话，无需
// 重启对话。
func (s *Server) handleAcpOptionsSet(ctx *vhttp.Context) error {
	var req struct {
		ID    string `json:"id"`
		Value string `json:"value"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil || strings.TrimSpace(req.ID) == "" {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": `请求体须为 {"id": "...", "value": "..."}`})
	}
	caps, err := s.acp.SetCapability(ctx.GinContext().Param("id"), strings.TrimSpace(req.ID), req.Value)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"capabilities": caps})
}

// handleAcpCatalog 返回可安装 agent 目录（内置精选 + 官方注册表合并）与本机
// 状态（设置页「安装 agent」用）。?refresh=1 强制重拉官方目录（默认缓存 TTL 1h，
// 拉取失败回退过期缓存；离线时仅内置目录）。
func (s *Server) handleAcpCatalog(ctx *vhttp.Context) error {
	views := s.acp.Catalog(ctx.Context(), ctx.GinContext().Query("refresh") == "1")
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"catalog": views})
}

// handleAcpDirs 列出某目录下的子目录（新建对话「选择目录」用，浏览器与桌面壳共用）。
// GET /api/acp/dirs?path=~/projects，path 缺省 = HOME。只读、只列目录、逐层懒列举
// （不递归、不读文件内容）；返回绝对路径供 thread 创建用作工作目录。
func (s *Server) handleAcpDirs(ctx *vhttp.Context) error {
	raw := strings.TrimSpace(ctx.GinContext().Query("path"))
	target := raw
	if target == "" || target == "~" || strings.HasPrefix(target, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "解析 HOME 失败"})
		}
		target = filepath.Join(home, strings.TrimPrefix(target, "~"))
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "解析路径失败"})
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "目录不存在或不可读"})
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "读取目录失败: " + err.Error()})
	}
	type dirItem struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	dirs := make([]dirItem, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, dirItem{Name: e.Name(), Path: filepath.Join(abs, e.Name())})
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name < dirs[j].Name })
	parent := filepath.Dir(abs)
	if parent == abs {
		parent = "" // 已到根
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"path": abs, "parent": parent, "dirs": dirs,
	})
}

// handleAcpInstall 执行目录中指定 agent 的官方安装命令（同步，最长 5 分钟），
// 成功后自动注册进智能体清单（写回 config.yaml 并热生效）——安装即入列，无需二次操作。
// 命令为内置目录固定串，不经任何外部输入拼接。
func (s *Server) handleAcpInstall(ctx *vhttp.Context) error {
	s.acpUpdateMu.Lock()
	defer s.acpUpdateMu.Unlock()
	var req struct {
		ID string `json:"id"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	if s.cfg == nil || s.configPath == "" {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "配置不可用"})
	}
	res, err := s.acp.Install(ctx.Context(), req.ID)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error(), "output": res.Output})
	}
	registered := false
	upgraded := false
	if !res.AlreadyRegistered {
		if err := s.cfg.Apply(func(n *config.Config) error {
			n.Acp = n.Acp.Normalized()
			n.Acp.Agents = append(n.Acp.Agents, res.Template)
			return config.UpdateAcp(s.configPath, n.Acp)
		}); err != nil {
			return ctx.JsonResponse(vhttp.Codes.BadRequest,
				map[string]string{"error": "安装成功，但写入清单失败: " + err.Error(), "output": res.Output})
		}
		s.acp.Reload(s.cfg.Snapshot().Acp)
		registered = true
	} else if res.Version != "" {
		// 升级时同步启动命令与参数，避免 binary 仍指向旧版本目录、npx
		// 仍带旧版本包名；用户自定义环境、目录等配置保留。
		if err := s.cfg.Apply(func(n *config.Config) error {
			next := n.Acp.Normalized()
			for i := range next.Agents {
				if next.Agents[i].Name == res.Template.Name {
					next.Agents[i].Version = res.Version
					next.Agents[i].Command = res.Template.Command
					next.Agents[i].Args = res.Template.Args
				}
			}
			n.Acp = next
			return config.UpdateAcp(s.configPath, next)
		}); err != nil {
			return ctx.JsonResponse(vhttp.Codes.BadRequest,
				map[string]string{"error": "安装成功，但更新版本戳失败: " + err.Error(), "output": res.Output})
		}
		s.acp.Reload(s.cfg.Snapshot().Acp)
		upgraded = true
	}
	msg := "安装完成，已加入智能体清单并生效"
	if !registered && !upgraded {
		msg = "安装完成（清单中已有同名 agent，未重复添加）"
	} else if upgraded {
		msg = "安装/升级完成（版本 " + res.Version + " 已登记）"
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"message": msg, "output": res.Output, "registered": registered, "agent": res.Template,
	})
}

// handleAcpTest 对指定 agent 做握手诊断（设置页「测试」按钮）。
func (s *Server) handleAcpTest(ctx *vhttp.Context) error {
	var req struct {
		Name string `json:"name"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	if err := s.acp.Test(strings.TrimSpace(req.Name)); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": "握手成功：agent 可正常拉起并响应 initialize"})
}

// handleAcpThreadFiles 列出会话工作区下的直接子项（文件树懒加载）：
// GET /api/acp/threads/:id/files?path=src。path 是相对主目录的路径，缺省为根。
func (s *Server) handleAcpThreadFiles(ctx *vhttp.Context) error {
	entries, full, truncated, err := s.acp.Files(
		ctx.GinContext().Param("id"), ctx.GinContext().Query("path"))
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"path": full, "entries": entries, "truncated": truncated,
	})
}

// handleAcpThreadChanges 读取某一轮的改动详情（含逐文件 diff）：
// GET /api/acp/threads/:id/changes?turn=t123。
func (s *Server) handleAcpThreadChanges(ctx *vhttp.Context) error {
	turn := strings.TrimSpace(ctx.GinContext().Query("turn"))
	if turn == "" {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "缺少 turn 参数"})
	}
	ch, err := s.acp.Changes(ctx.GinContext().Param("id"), turn)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"changes": ch})
}

// handleAcpThreadChangesRevert 还原本轮改动：POST /api/acp/threads/:id/changes/revert
// body {"turn":"t123"}。还原前会拒绝“本轮之后又被改过”的文件与同目录仍在跑的会话。
func (s *Server) handleAcpThreadChangesRevert(ctx *vhttp.Context) error {
	var req struct {
		Turn string `json:"turn"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil || strings.TrimSpace(req.Turn) == "" {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": `请求体须为 {"turn": "..."}`})
	}
	ch, err := s.acp.Revert(ctx.GinContext().Param("id"), strings.TrimSpace(req.Turn))
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"changes": ch.Summary(), "message": "已还原本轮改动",
	})
}

// handleAcpThreadHandoff 从当前会话生成交接摘要并新开一条同工作区/agent/主目录的
// “续”会话：POST /api/acp/threads/:id/handoff。旧会话保留，摘要随后续首轮下发。
func (s *Server) handleAcpThreadHandoff(ctx *vhttp.Context) error {
	th, err := s.acp.Handoff(ctx.GinContext().Param("id"))
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"thread": th, "message": "已新开续会话：交接摘要会随下一轮发给 agent",
	})
}

// handleAcpWorkspaceMemoryList 列出工作区项目记忆页（不含正文）。
func (s *Server) handleAcpWorkspaceMemoryList(ctx *vhttp.Context) error {
	scope, err := s.workspaceMemoryScope(ctx.GinContext().Param("id"))
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"pages": s.memory.PagesScoped(scope)})
}

// handleAcpWorkspaceMemoryGet 读取一页项目记忆正文。
func (s *Server) handleAcpWorkspaceMemoryGet(ctx *vhttp.Context) error {
	scope, err := s.workspaceMemoryScope(ctx.GinContext().Param("id"))
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	p, ok := s.memory.ReadScoped(scope, ctx.GinContext().Param("page"))
	if !ok {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "未找到这页项目记忆"})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"page": p})
}

// handleAcpWorkspaceMemorySave 新建或更新一页项目记忆：
// POST /api/acp/workspaces/:id/memory {"id?","title","body"}。
func (s *Server) handleAcpWorkspaceMemorySave(ctx *vhttp.Context) error {
	scope, err := s.workspaceMemoryScope(ctx.GinContext().Param("id"))
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	var req struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": `请求体须为 {"title":"…","body":"…"}`})
	}
	p, created, err := s.memory.SaveScoped(scope, req.ID, req.Title, req.Body, "admin")
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	msg := "项目记忆已更新"
	if created {
		msg = "已新建项目记忆"
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"page": p, "created": created, "message": msg})
}

// handleAcpWorkspaceMemoryDelete 删除一页项目记忆。
func (s *Server) handleAcpWorkspaceMemoryDelete(ctx *vhttp.Context) error {
	scope, err := s.workspaceMemoryScope(ctx.GinContext().Param("id"))
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	if !s.memory.DeleteScoped(scope, ctx.GinContext().Param("page")) {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "未找到这页项目记忆"})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"message": "项目记忆已删除"})
}

// workspaceMemoryScope 校验工作区存在且记忆仓库可用，返回 scope（工作区 id）。
func (s *Server) workspaceMemoryScope(workspaceID string) (string, error) {
	if s.acp == nil {
		return "", fmt.Errorf("智能体功能未启用")
	}
	if s.memory == nil {
		return "", fmt.Errorf("长期记忆未启用")
	}
	if _, ok := s.acp.GetWorkspace(workspaceID); !ok {
		return "", fmt.Errorf("未找到工作区 %s", workspaceID)
	}
	return workspaceID, nil
}

// handleAcpChat 处理一次智能体对话：POST /api/acp/threads/:id/chat，
// 请求体 {"message":"...", "attachments":["<上传id>", ...]}，响应为 SSE 流，
// 帧协议与 /api/chat 一致（另有 {"status":"..."} 生命周期占位帧）。
func (s *Server) handleAcpChat(ctx *vhttp.Context) error {
	var req struct {
		Message     string   `json:"message"`
		Attachments []string `json:"attachments"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil ||
		(strings.TrimSpace(req.Message) == "" && len(req.Attachments) == 0) {
		return ctx.JsonResponse(vhttp.Codes.BadRequest,
			map[string]string{"error": `请求体须为 {"message": "...", "attachments": [...]}`})
	}
	if s.chat == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "聊天服务未启用（图片上传链路不可用）"})
	}
	threadID := ctx.GinContext().Param("id")

	// 附件 → 图片内容（复用聊天页的上传链路与目录）。
	var imgs []acp.Image
	for _, id := range req.Attachments {
		path, ok := s.chat.ResolveUpload(id)
		if !ok {
			return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "非法的附件 ID: " + id})
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "附件不存在或已过期"})
		}
		imgs = append(imgs, acp.Image{MIME: detectImageMIME(data), Data: data, ID: id})
	}

	gin := ctx.GinContext()
	gin.Header("Content-Type", "text/event-stream; charset=utf-8")
	gin.Header("Cache-Control", "no-cache")
	gin.Header("X-Accel-Buffering", "no")
	gin.Writer.WriteHeader(http.StatusOK)
	flusher, ok := gin.Writer.(http.Flusher)
	if !ok {
		return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": "连接不支持流式响应"})
	}
	var wmu sync.Mutex
	write := func(v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return false
		}
		wmu.Lock()
		defer wmu.Unlock()
		if _, err := fmt.Fprintf(gin.Writer, "data: %s\n\n", b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	rctx, cancel := context.WithCancel(logx.WithID(ctx.Context(), logx.NextID()))
	defer cancel()
	question := strings.TrimSpace(req.Message)
	trace.Begin(logx.IDFrom(rctx), "agent", "acp:"+threadID, question)
	defer trace.End(logx.IDFrom(rctx), "")

	res, err := s.acp.Send(rctx, threadID, question, imgs, acp.TurnHooks{
		OnText: func(t string) {
			if !write(map[string]string{"delta": t}) {
				cancel() // 客户端已断开，中止对话
			}
		},
		OnThought: func(t string) {
			if !write(map[string]string{"thinking": t}) {
				cancel()
			}
		},
		OnStep: func(st agent.Step) {
			if !write(map[string]any{"step": st}) {
				cancel()
			}
		},
		OnStatus: func(msg string) {
			write(map[string]string{"status": msg})
		},
	})
	if err != nil {
		logx.Printf(rctx, "[智能体] 对话失败: %v", err)
		trace.Record(rctx, "acp", "失败: %v", err)
		write(map[string]any{"done": true, "error": err.Error()})
		return nil
	}
	trace.Record(rctx, "acp", "stopReason=%s，token in=%d out=%d", res.StopReason, res.In, res.Out)
	turn := map[string]any{"in": res.In, "out": res.Out, "ms": res.Ms}
	if res.StopReason != "" && res.StopReason != "end_turn" {
		turn["stopped"] = res.StopReason
	}
	payload := map[string]any{"done": true, "turn": turn}
	if res.Changes != nil {
		// 只带摘要：逐文件 diff 由 /changes?turn= 单独取，避免 SSE 单帧过大。
		payload["changes"] = res.Changes.Summary()
	}
	if th, ok := s.acp.Get(threadID); ok && th.ContextSize > 0 {
		// M-5：上下文水位（agent 自报，仅展示）。
		payload["context"] = map[string]any{
			"used": th.ContextUsed, "size": th.ContextSize, "cost": th.ContextCost,
		}
	}
	write(payload)
	return nil
}

// handleUploadGet 回读一张已上传的图片（智能体对话历史回显原图用）：
// GET /api/upload/:id，仅限 uploads 目录内合法 ID，登录后可读；ID 随机生成
// 不可枚举，内容不可变，允许长缓存。
func (s *Server) handleUploadGet(ctx *vhttp.Context) error {
	if s.chat == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "聊天服务未启用"})
	}
	path, ok := s.chat.ResolveUpload(ctx.GinContext().Param("id"))
	if !ok {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "非法的附件 ID"})
	}
	if _, err := os.Stat(path); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "附件不存在或已过期"})
	}
	gin := ctx.GinContext()
	gin.Header("Cache-Control", "private, max-age=86400")
	gin.File(path)
	return nil
}

// detectImageMIME 嗅探图片类型（与聊天页上传白名单一致：png/jpeg/webp）。
func detectImageMIME(data []byte) string {
	if len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n" {
		return "image/png"
	}
	if len(data) >= 3 && string(data[:3]) == "\xff\xd8\xff" {
		return "image/jpeg"
	}
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return "image/webp"
	}
	return "application/octet-stream"
}
