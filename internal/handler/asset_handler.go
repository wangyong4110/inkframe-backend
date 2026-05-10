package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// AssetHandler handles /api/v1/assets and related routes.
type AssetHandler struct {
	svc *service.AssetService
}

func NewAssetHandler(svc *service.AssetService) *AssetHandler {
	return &AssetHandler{svc: svc}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func callerID(c *gin.Context) uint {
	if v, ok := c.Get("tenant_id"); ok {
		if id, ok := v.(uint); ok {
			return id
		}
	}
	return 0
}

func tenantID(c *gin.Context) uint { return callerID(c) }

// ─── Asset CRUD ───────────────────────────────────────────────────────────────

// POST /assets  (multipart upload)
func (h *AssetHandler) Upload(c *gin.Context) {
	f, header, err := c.Request.FormFile("file")
	if err != nil {
		respondBadRequest(c, "file required")
		return
	}
	defer f.Close()

	title := c.PostForm("title")
	if title == "" {
		title = header.Filename
	}
	assetType := c.PostForm("type")
	if assetType == "" {
		assetType = "image"
	}
	mime := header.Header.Get("Content-Type")
	if mime == "" {
		mime = "application/octet-stream"
	}

	uid := callerID(c)
	tid := tenantID(c)

	asset, err := h.svc.Upload(c.Request.Context(), f, header.Size, service.UploadAssetParams{
		TenantID: tid, CreatorID: uid,
		Title: title, Type: assetType,
		SubType:  c.PostForm("sub_type"),
		MimeType: mime, FileSize: header.Size,
	})
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondCreated(c, asset)
}

// GET /assets/:id
func (h *AssetHandler) GetAsset(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	asset, err := h.svc.GetByID(uint(id), callerID(c))
	if err != nil {
		respondErr(c, http.StatusNotFound, err.Error())
		return
	}
	respondOK(c, asset)
}

// GET /assets
func (h *AssetHandler) SearchAssets(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	params := repository.AssetSearchParams{
		Scope:    c.DefaultQuery("scope", "personal"),
		CallerID: callerID(c), TenantID: tenantID(c),
		Q: c.Query("q"), Type: c.Query("type"),
		SubType: c.Query("sub_type"), Source: c.Query("source"),
		License: c.Query("license"),
		Sort:    c.DefaultQuery("sort", "created_at"),
		Page: page, PageSize: size,
		Status: c.Query("status"),
	}
	if tags := c.QueryArray("tags"); len(tags) > 0 {
		params.Tags = tags
	}

	assets, total, err := h.svc.Search(c.Request.Context(), params)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"items": assets, "total": total, "page": page, "page_size": size})
}

// PUT /assets/:id
func (h *AssetHandler) UpdateAsset(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	var body map[string]interface{}
	if err := c.ShouldBindJSON(&body); err != nil {
		respondBadRequest(c, "invalid body")
		return
	}
	// Only allow safe fields
	allowed := map[string]bool{"title": true, "description": true}
	fields := map[string]interface{}{}
	for k, v := range body {
		if allowed[k] {
			fields[k] = v
		}
	}
	if err := h.svc.Update(uint(id), callerID(c), fields); err != nil {
		respondErr(c, http.StatusForbidden, err.Error())
		return
	}
	respondOK(c, nil)
}

// DELETE /assets/:id  (soft delete)
func (h *AssetHandler) SoftDelete(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	if err := h.svc.SoftDelete(uint(id), callerID(c)); err != nil {
		respondErr(c, http.StatusForbidden, err.Error())
		return
	}
	respondOK(c, nil)
}

// POST /assets/:id/restore
func (h *AssetHandler) Restore(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	if err := h.svc.RestoreFromTrash(uint(id), callerID(c)); err != nil {
		respondErr(c, http.StatusForbidden, err.Error())
		return
	}
	respondOK(c, nil)
}

// DELETE /assets/:id/purge
func (h *AssetHandler) Purge(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	if err := h.svc.PurgeAsset(c.Request.Context(), uint(id), callerID(c)); err != nil {
		respondErr(c, http.StatusForbidden, err.Error())
		return
	}
	respondOK(c, nil)
}

// GET /assets/trash
func (h *AssetHandler) ListTrash(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	assets, total, err := h.svc.ListTrash(callerID(c), page, size)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"items": assets, "total": total})
}

// ─── Share Workflow ───────────────────────────────────────────────────────────

// POST /assets/:id/share-request
func (h *AssetHandler) RequestShare(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	req, err := h.svc.RequestShare(uint(id), callerID(c))
	if err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	respondCreated(c, req)
}

// GET /assets/:id/share-request
func (h *AssetHandler) GetShareRequest(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	req, err := h.svc.GetShareRequest(uint(id), callerID(c))
	if err != nil {
		respondErr(c, http.StatusNotFound, err.Error())
		return
	}
	respondOK(c, req)
}

// DELETE /assets/:id/share-request
func (h *AssetHandler) CancelShareRequest(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	if err := h.svc.CancelShareRequest(uint(id), callerID(c)); err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	respondOK(c, nil)
}

// POST /assets/:id/withdraw
func (h *AssetHandler) WithdrawShare(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	if err := h.svc.WithdrawShare(uint(id), callerID(c)); err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	respondOK(c, nil)
}

// ─── Admin ────────────────────────────────────────────────────────────────────

// GET /admin/share-requests
func (h *AssetHandler) ListPendingShareRequests(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	reqs, total, err := h.svc.ListPendingShareRequests(page, size)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"items": reqs, "total": total})
}

// POST /admin/share-requests/:id/approve
func (h *AssetHandler) ApproveShareRequest(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	var body struct {
		Note string `json:"note"`
	}
	_ = c.ShouldBindJSON(&body)
	if err := h.svc.AdminReview(uint(id), callerID(c), true, body.Note); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, nil)
}

// POST /admin/share-requests/:id/reject
func (h *AssetHandler) RejectShareRequest(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	var body struct {
		Note string `json:"note"`
	}
	_ = c.ShouldBindJSON(&body)
	if err := h.svc.AdminReview(uint(id), callerID(c), false, body.Note); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, nil)
}

// POST /admin/assets/:id/remove
func (h *AssetHandler) AdminRemoveAsset(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	var body struct {
		Note string `json:"note"`
	}
	_ = c.ShouldBindJSON(&body)
	if err := h.svc.AdminRemove(uint(id), callerID(c), body.Note); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, nil)
}

// ─── Versions ─────────────────────────────────────────────────────────────────

// GET /assets/:id/versions
func (h *AssetHandler) ListVersions(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	vs, err := h.svc.ListVersions(uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, vs)
}

// POST /assets/:id/versions
func (h *AssetHandler) CreateVersion(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	f, header, err := c.Request.FormFile("file")
	if err != nil {
		respondBadRequest(c, "file required")
		return
	}
	defer f.Close()
	mime := header.Header.Get("Content-Type")
	if mime == "" {
		mime = "application/octet-stream"
	}
	v, err := h.svc.CreateVersion(c.Request.Context(), uint(id), callerID(c),
		f, header.Size, mime, c.PostForm("note"))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondCreated(c, v)
}

// POST /assets/:id/versions/:v/restore
func (h *AssetHandler) RestoreVersion(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	vNo, _ := strconv.Atoi(c.Param("v"))
	if err := h.svc.RestoreVersion(c.Request.Context(), uint(id), vNo, callerID(c)); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, nil)
}

// ─── Tags ─────────────────────────────────────────────────────────────────────

// GET /assets/tags
func (h *AssetHandler) ListTags(c *gin.Context) {
	tags, err := h.svc.ListTags()
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, tags)
}

// GET /assets/tags/suggest
func (h *AssetHandler) SuggestTags(c *gin.Context) {
	q := c.Query("q")
	tags, err := h.svc.SuggestTags(q)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, tags)
}

// POST /assets/:id/tags
func (h *AssetHandler) AddTags(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	var body struct {
		TagNames []string `json:"tag_names"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || len(body.TagNames) == 0 {
		respondBadRequest(c, "tag_names required")
		return
	}
	tags, err := h.svc.AddTags(uint(id), callerID(c), body.TagNames)
	if err != nil {
		respondErr(c, http.StatusForbidden, err.Error())
		return
	}
	respondOK(c, tags)
}

// DELETE /assets/:id/tags/:tag_id
func (h *AssetHandler) RemoveTag(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	tid, _ := strconv.ParseUint(c.Param("tag_id"), 10, 64)
	if err := h.svc.RemoveTag(uint(id), uint(tid), callerID(c)); err != nil {
		respondErr(c, http.StatusForbidden, err.Error())
		return
	}
	respondOK(c, nil)
}

// POST /assets/:id/auto-tag
func (h *AssetHandler) TriggerAutoTag(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	if err := h.svc.TriggerAutoTag(uint(id), callerID(c)); err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	respondOK(c, gin.H{"message": "auto-tag triggered"})
}

// ─── Public Library Interactions ──────────────────────────────────────────────

// POST /assets/:id/like
func (h *AssetHandler) ToggleLike(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	liked, err := h.svc.ToggleLike(uint(id), callerID(c))
	if err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	respondOK(c, gin.H{"liked": liked})
}

// POST /assets/:id/use
func (h *AssetHandler) UseAsset(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	var body struct {
		UsedByType string `json:"used_by_type"`
		UsedByID   uint   `json:"used_by_id"`
	}
	_ = c.ShouldBindJSON(&body)
	usage := model.AssetUsage{
		UsedByType: body.UsedByType, UsedByID: body.UsedByID,
		TenantID: tenantID(c), UserID: callerID(c),
	}
	url, attribution, err := h.svc.UseAsset(uint(id), usage)
	if err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	respondOK(c, gin.H{"storage_url": url, "attribution_text": attribution})
}

// ─── Batch Operations ─────────────────────────────────────────────────────────

// POST /assets/batch-delete
func (h *AssetHandler) BatchDelete(c *gin.Context) {
	var body struct {
		AssetIDs []uint `json:"asset_ids"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		respondBadRequest(c, "asset_ids required")
		return
	}
	_ = h.svc.BatchDelete(callerID(c), body.AssetIDs)
	respondOK(c, nil)
}

// POST /assets/batch-share-request
func (h *AssetHandler) BatchShareRequest(c *gin.Context) {
	var body struct {
		AssetIDs []uint `json:"asset_ids"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		respondBadRequest(c, "asset_ids required")
		return
	}
	submitted, failed := h.svc.BatchShareRequest(callerID(c), body.AssetIDs)
	respondOK(c, gin.H{"submitted": submitted, "failed": failed})
}

// ─── Collections ─────────────────────────────────────────────────────────────

// GET /asset-collections
func (h *AssetHandler) ListCollections(c *gin.Context) {
	cols, err := h.svc.ListCollections(callerID(c))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, cols)
}

// POST /asset-collections
func (h *AssetHandler) CreateCollection(c *gin.Context) {
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Scope       string `json:"scope"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Name == "" {
		respondBadRequest(c, "name required")
		return
	}
	scope := body.Scope
	if scope == "" {
		scope = "personal"
	}
	col, err := h.svc.CreateCollection(tenantID(c), callerID(c), body.Name, body.Description, scope)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondCreated(c, col)
}

// POST /asset-collections/:id/items
func (h *AssetHandler) AddToCollection(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	var body struct {
		AssetIDs []uint `json:"asset_ids"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		respondBadRequest(c, "asset_ids required")
		return
	}
	if err := h.svc.AddToCollection(uint(id), body.AssetIDs, callerID(c)); err != nil {
		respondErr(c, http.StatusForbidden, err.Error())
		return
	}
	respondOK(c, nil)
}

// DELETE /asset-collections/:id/items
func (h *AssetHandler) RemoveFromCollection(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	var body struct {
		AssetIDs []uint `json:"asset_ids"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		respondBadRequest(c, "asset_ids required")
		return
	}
	if err := h.svc.RemoveFromCollection(uint(id), body.AssetIDs, callerID(c)); err != nil {
		respondErr(c, http.StatusForbidden, err.Error())
		return
	}
	respondOK(c, nil)
}

// GET /asset-collections/:id/items
func (h *AssetHandler) ListCollectionItems(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	assets, err := h.svc.ListCollectionItems(uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, assets)
}

// ─── Share Links ──────────────────────────────────────────────────────────────

// POST /share-links
func (h *AssetHandler) CreateShareLink(c *gin.Context) {
	var body struct {
		AssetID         *uint `json:"asset_id"`
		CollectionID    *uint `json:"collection_id"`
		ExpiresIn       int   `json:"expires_in"` // hours
		DownloadAllowed bool  `json:"download_allowed"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		respondBadRequest(c, "invalid body")
		return
	}
	sl, err := h.svc.CreateShareLink(callerID(c), service.ShareLinkOptions{
		AssetID: body.AssetID, CollectionID: body.CollectionID,
		ExpiresIn: body.ExpiresIn, DownloadAllowed: body.DownloadAllowed,
	})
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondCreated(c, sl)
}

// GET /share-links
func (h *AssetHandler) ListShareLinks(c *gin.Context) {
	sls, err := h.svc.ListShareLinks(callerID(c))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, sls)
}

// DELETE /share-links/:token
func (h *AssetHandler) RevokeShareLink(c *gin.Context) {
	token := c.Param("token")
	if err := h.svc.RevokeShareLink(token, callerID(c)); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, nil)
}

// GET /share/:token  (public, no auth)
func (h *AssetHandler) PublicSharePage(c *gin.Context) {
	token := c.Param("token")
	sl, err := h.svc.ValidateShareLink(token)
	if err != nil {
		respondErr(c, http.StatusNotFound, err.Error())
		return
	}
	respondOK(c, sl)
}

// ─── Comments ─────────────────────────────────────────────────────────────────

// GET /assets/:id/comments
func (h *AssetHandler) ListComments(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	cs, err := h.svc.ListComments(uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, cs)
}

// POST /assets/:id/comments
func (h *AssetHandler) AddComment(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	var body struct {
		Content  string   `json:"content"`
		ParentID *uint    `json:"parent_id"`
		XRatio   *float64 `json:"x_ratio"`
		YRatio   *float64 `json:"y_ratio"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Content == "" {
		respondBadRequest(c, "content required")
		return
	}
	comment, err := h.svc.AddComment(uint(id), callerID(c), body.Content, body.ParentID, body.XRatio, body.YRatio)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondCreated(c, comment)
}

// DELETE /assets/:id/comments/:cid
func (h *AssetHandler) DeleteComment(c *gin.Context) {
	cid, _ := strconv.ParseUint(c.Param("cid"), 10, 64)
	if err := h.svc.DeleteComment(uint(cid), callerID(c)); err != nil {
		respondErr(c, http.StatusForbidden, err.Error())
		return
	}
	respondOK(c, nil)
}

// ─── Analytics ────────────────────────────────────────────────────────────────

// GET /account/quota
func (h *AssetHandler) GetQuota(c *gin.Context) {
	q, err := h.svc.GetQuota(tenantID(c))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, q)
}

// GET /assets/stats/ranking
func (h *AssetHandler) GetValueRanking(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	assets, err := h.svc.GetValueRanking(limit)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, assets)
}

// GET /assets/stats/search-gaps
func (h *AssetHandler) GetSearchGaps(c *gin.Context) {
	gaps, err := h.svc.GetSearchGaps(c.DefaultQuery("scope", "public"))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gaps)
}

// ─── Crawl Jobs ───────────────────────────────────────────────────────────────

// GET /crawl-jobs
func (h *AssetHandler) ListCrawlJobs(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	jobs, total, err := h.svc.ListCrawlJobs(page, size)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"items": jobs, "total": total})
}

// POST /crawl-jobs
func (h *AssetHandler) CreateCrawlJob(c *gin.Context) {
	var body struct {
		Source    string `json:"source"`
		Query     string `json:"query"`
		AssetType string `json:"asset_type"`
		License   string `json:"license"`
		Limit     int    `json:"limit"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Source == "" {
		respondBadRequest(c, "source and query required")
		return
	}
	if body.Limit == 0 {
		body.Limit = 20
	}
	job, err := h.svc.CreateCrawlJob(body.Source, body.Query, body.AssetType, body.License, body.Limit, callerID(c))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondCreated(c, job)
}

// GET /crawl-jobs/:id
func (h *AssetHandler) GetCrawlJob(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	job, err := h.svc.GetCrawlJob(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, err.Error())
		return
	}
	respondOK(c, job)
}
