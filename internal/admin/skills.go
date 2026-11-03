// skills.go 提供技能的管理后台接口：列表、原文编辑、启用开关、删除与重载。
// 技能文件本身就是真相，所有写操作都只落到 SKILL.md 并按次生效——
// 不重建 Agent、不占用配置热更新链路。
package admin

import (
	"net/http"

	vhttp "github.com/capyflow/vortexv3/server/http"
)

// registerSkillRoutes 挂载技能接口（技能层关闭时不挂载）。
func (s *Server) registerSkillRoutes(root *vhttp.VortexHttpRouterGroup) {
	if s.skills == nil {
		return
	}
	root.AddRouter([]string{http.MethodGet}, "/api/skills", s.wrap(s.handleSkillsList))
	root.AddRouter([]string{http.MethodPost}, "/api/skills/reload", s.wrap(s.handleSkillsReload))
	root.AddRouter([]string{http.MethodGet}, "/api/skills/:id", s.wrap(s.handleSkillGet))
	root.AddRouter([]string{http.MethodPost}, "/api/skills", s.wrap(s.handleSkillCreate))
	root.AddRouter([]string{http.MethodPut}, "/api/skills/:id", s.wrap(s.handleSkillUpdate))
	root.AddRouter([]string{http.MethodPost}, "/api/skills/:id/enabled", s.wrap(s.handleSkillEnabled))
	root.AddRouter([]string{http.MethodDelete}, "/api/skills/:id", s.wrap(s.handleSkillDelete))
}

func (s *Server) handleSkillsList(ctx *vhttp.Context) error {
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"skills": s.skills.List(),
		"dir":    s.skills.Dir(),
		// roots：全部发现根（含只读的约定地址）。多根之后"这条从哪儿来"必须看得见。
		"roots": s.skills.Roots(),
		"error": errText(s.skills.Err()),
	})
}

func (s *Server) handleSkillsReload(ctx *vhttp.Context) error {
	s.skillUpdateMu.Lock()
	defer s.skillUpdateMu.Unlock()
	list := s.skills.Reload()
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"message": "技能目录已重新扫描",
		"skills":  list,
		"error":   errText(s.skills.Err()),
	})
}

func (s *Server) handleSkillGet(ctx *vhttp.Context) error {
	id := ctx.GinContext().Param("id")
	summary, ok := s.skills.Get(id)
	if !ok {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "技能不存在: " + id})
	}
	raw, err := s.skills.Raw(id)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"skill": summary, "raw": raw})
}

// skillWriteBody 是新建/编辑的请求体：整份 SKILL.md 文本（后台直接编辑 markdown）。
type skillWriteBody struct {
	ID  string `json:"id"`
	Raw string `json:"raw"`
}

func (s *Server) handleSkillCreate(ctx *vhttp.Context) error {
	var body skillWriteBody
	if err := ctx.UnmarshalBody(&body); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	s.skillUpdateMu.Lock()
	defer s.skillUpdateMu.Unlock()
	summary, err := s.skills.Save(body.ID, body.Raw)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"message": "技能已保存并生效（下一轮对话即可用，无需重启）",
		"skill":   summary,
	})
}

func (s *Server) handleSkillUpdate(ctx *vhttp.Context) error {
	id := ctx.GinContext().Param("id")
	var body skillWriteBody
	if err := ctx.UnmarshalBody(&body); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	s.skillUpdateMu.Lock()
	defer s.skillUpdateMu.Unlock()
	summary, err := s.skills.Save(id, body.Raw)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"message": "技能已更新并生效",
		"skill":   summary,
	})
}

func (s *Server) handleSkillEnabled(ctx *vhttp.Context) error {
	id := ctx.GinContext().Param("id")
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := ctx.UnmarshalBody(&body); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 {enabled: bool}"})
	}
	s.skillUpdateMu.Lock()
	defer s.skillUpdateMu.Unlock()
	summary, err := s.skills.SetEnabled(id, body.Enabled)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	msg := "技能已启用"
	if !body.Enabled {
		msg = "技能已停用（文件保留，随时可再启用）"
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"message": msg, "skill": summary})
}

func (s *Server) handleSkillDelete(ctx *vhttp.Context) error {
	id := ctx.GinContext().Param("id")
	s.skillUpdateMu.Lock()
	defer s.skillUpdateMu.Unlock()
	if err := s.skills.Delete(id); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"message": "技能已删除"})
}
