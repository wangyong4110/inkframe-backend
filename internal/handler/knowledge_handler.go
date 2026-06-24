package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// KnowledgeHandler 知识库处理器
type KnowledgeHandler struct {
	knowledgeSvc *service.KnowledgeService
	novelSvc     *service.NovelService
}

func NewKnowledgeHandler(knowledgeSvc *service.KnowledgeService) *KnowledgeHandler {
	return &KnowledgeHandler{knowledgeSvc: knowledgeSvc}
}

// WithNovelService 注入小说服务（用于租户校验）
func (h *KnowledgeHandler) WithNovelService(svc *service.NovelService) *KnowledgeHandler {
	h.novelSvc = svc
	return h
}

// checkNovelOwnership 校验小说归属当前租户。返回 false 时已写入错误响应。
func (h *KnowledgeHandler) checkNovelOwnership(c *gin.Context, novelID uint) bool {
	if h.novelSvc == nil {
		return true
	}
	if _, err := h.novelSvc.GetNovel(novelID, getTenantID(c), getUserID(c)); err != nil {
		respondErr(c, http.StatusNotFound, "novel not found")
		return false
	}
	return true
}

// ListKnowledge 获取小说知识库列表（支持分页）
// GET /novels/:id/knowledge?page=1&page_size=20
func (h *KnowledgeHandler) ListKnowledge(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if !h.checkNovelOwnership(c, novelID) {
		return
	}
	pageStr := c.DefaultQuery("page", "1")
	pageSizeStr := c.DefaultQuery("page_size", "20")
	pageNum, err := strconv.Atoi(pageStr)
	if err != nil || pageNum < 1 {
		pageNum = 1
	}
	pageSizeNum, err := strconv.Atoi(pageSizeStr)
	if err != nil || pageSizeNum < 1 || pageSizeNum > 100 {
		pageSizeNum = 20
	}
	items, total, err := h.knowledgeSvc.GetByNovelPaged(c.Request.Context(), novelID, pageNum, pageSizeNum)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"items":     items,
			"total":     total,
			"page":      pageNum,
			"page_size": pageSizeNum,
		},
	})
}

// BulkImport 批量导入知识条目
// POST /novels/:id/knowledge/bulk-import
// Body: {"items": [...]}
func (h *KnowledgeHandler) BulkImport(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if !requireNovelEditorRole(c, h.novelSvc, novelID) {
		return
	}
	var req struct {
		Items []service.KnowledgeImportItem `json:"items" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	if len(req.Items) > 500 {
		respondBadRequest(c, "max 500 items per import")
		return
	}
	count, err := h.knowledgeSvc.BulkImport(c.Request.Context(), novelID, req.Items)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "import failed")
		return
	}
	respondOK(c, gin.H{"imported": count})
}

// CreateKnowledge 创建知识条目
// POST /novels/:id/knowledge
func (h *KnowledgeHandler) CreateKnowledge(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if !requireNovelEditorRole(c, h.novelSvc, novelID) {
		return
	}
	var req struct {
		Type    string `json:"type"`
		Title   string `json:"title" binding:"required"`
		Content string `json:"content"`
		Tags    string `json:"tags"`
	}
	if !bindJSON(c, &req) {
		return
	}
	kb := &model.KnowledgeBase{
		Type:    req.Type,
		Title:   req.Title,
		Content: req.Content,
		Tags:    req.Tags,
		NovelID: &novelID,
	}
	if err := h.knowledgeSvc.StoreKnowledge(c.Request.Context(), kb); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondCreated(c, kb)
}

// UpdateKnowledge 更新知识条目
// PUT /novels/:id/knowledge/:kb_id
func (h *KnowledgeHandler) UpdateKnowledge(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if !requireNovelEditorRole(c, h.novelSvc, novelID) {
		return
	}
	kbID, ok := parseID(c, "kb_id")
	if !ok {
		return
	}
	var req struct {
		Title   string `json:"title"`
		Content string `json:"content"`
		Tags    string `json:"tags"`
	}
	if !bindJSON(c, &req) {
		return
	}
	novelIDPtr := uint(novelID)
	kb, err := h.knowledgeSvc.UpdateKnowledge(c.Request.Context(), uint(kbID), &novelIDPtr, req.Title, req.Content, req.Tags)
	if err != nil {
		respondErr(c, http.StatusNotFound, err.Error())
		return
	}
	respondOK(c, kb)
}

// DeleteKnowledge 删除知识条目
// DELETE /novels/:id/knowledge/:kb_id
func (h *KnowledgeHandler) DeleteKnowledge(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if !requireNovelEditorRole(c, h.novelSvc, novelID) {
		return
	}
	kbID, ok := parseID(c, "kb_id")
	if !ok {
		return
	}
	novelIDPtr := uint(novelID)
	if err := h.knowledgeSvc.DeleteKnowledge(c.Request.Context(), uint(kbID), &novelIDPtr); err != nil {
		respondErr(c, http.StatusNotFound, err.Error())
		return
	}
	respondOK(c, nil)
}

// SearchKnowledge 搜索知识库
// GET /novels/:id/knowledge/search?q=query&limit=10
func (h *KnowledgeHandler) SearchKnowledge(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if !h.checkNovelOwnership(c, novelID) {
		return
	}
	query := c.Query("q")
	if query == "" {
		respondBadRequest(c, "q parameter required")
		return
	}
	limit := 10
	if ls := c.Query("limit"); ls != "" {
		if v, err := strconv.Atoi(ls); err == nil && v > 0 && v <= 100 {
			limit = v
		}
	}
	results, err := h.knowledgeSvc.SearchKnowledge(c.Request.Context(), query, limit, &novelID)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, results)
}
