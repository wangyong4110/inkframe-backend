package handler

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// CollabHandler 协作处理器
type CollabHandler struct {
	collabSvc *service.CollabService
}

func NewCollabHandler(collabSvc *service.CollabService) *CollabHandler {
	return &CollabHandler{collabSvc: collabSvc}
}

func getUserIDFromCtx(c *gin.Context) uint {
	if v, ok := c.Get("user_id"); ok {
		if id, ok := v.(uint); ok {
			return id
		}
	}
	return 0
}

// GET /novels/:id/members
func (h *CollabHandler) ListMembers(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	userID := getUserIDFromCtx(c)
	// 确保 owner 在成员表中
	h.collabSvc.EnsureOwner(novelID, userID)
	members, err := h.collabSvc.ListMembers(novelID)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"members": members})
}

// POST /novels/:id/members/invite  { email, role, ttl_minutes }
func (h *CollabHandler) InviteMember(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	var req struct {
		Email      string `json:"email" binding:"required,email"`
		Role       string `json:"role"`
		TTLMinutes int    `json:"ttl_minutes"` // 0 = default (10 min)
	}
	if !bindJSON(c, &req) {
		return
	}
	userID := getUserIDFromCtx(c)

	token, err := h.collabSvc.InviteMember(novelID, userID, req.Email, req.Role, req.TTLMinutes)
	if err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	respondOK(c, gin.H{
		"invite_token": token,
		"invite_link":  fmt.Sprintf("/collab/accept?token=%s", token),
	})
}

// POST /collab/accept  { token }
func (h *CollabHandler) AcceptInvite(c *gin.Context) {
	var req struct {
		Token string `json:"token" binding:"required"`
	}
	if !bindJSON(c, &req) {
		return
	}
	userID := getUserIDFromCtx(c)
	novelID, err := h.collabSvc.AcceptInvite(req.Token, userID)
	if err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	respondOK(c, gin.H{"novel_id": novelID})
}

// DELETE /novels/:id/members/:uid
func (h *CollabHandler) RemoveMember(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	targetUID, ok2 := parseID(c, "uid")
	if !ok2 {
		return
	}
	userID := getUserIDFromCtx(c)
	if err := h.collabSvc.RemoveMember(novelID, userID, targetUID); err != nil {
		respondErr(c, http.StatusForbidden, err.Error())
		return
	}
	respondOK(c, gin.H{})
}

// PUT /novels/:id/members/:uid  { role }
func (h *CollabHandler) UpdateMemberRole(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	targetUID, ok2 := parseID(c, "uid")
	if !ok2 {
		return
	}
	var req struct {
		Role string `json:"role" binding:"required"`
	}
	if !bindJSON(c, &req) {
		return
	}
	userID := getUserIDFromCtx(c)
	if err := h.collabSvc.UpdateMemberRole(novelID, userID, targetUID, req.Role); err != nil {
		respondErr(c, http.StatusForbidden, err.Error())
		return
	}
	respondOK(c, gin.H{})
}

// DELETE /novels/:id/members/me  — 当前用户主动退出协作
func (h *CollabHandler) LeaveNovel(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	userID := getUserIDFromCtx(c)
	if err := h.collabSvc.LeaveNovel(novelID, userID); err != nil {
		respondErr(c, http.StatusForbidden, err.Error())
		return
	}
	respondOK(c, gin.H{})
}

// POST /editing-locks  { entity_type, entity_id, novel_id }
func (h *CollabHandler) AcquireLock(c *gin.Context) {
	var req struct {
		EntityType string `json:"entity_type" binding:"required"`
		EntityID   uint   `json:"entity_id" binding:"required"`
		NovelID    uint   `json:"novel_id"`
	}
	if !bindJSON(c, &req) {
		return
	}
	userID := getUserIDFromCtx(c)
	lock, ok := h.collabSvc.AcquireLock(req.EntityType, req.EntityID, userID)
	if !ok {
		respondErr(c, http.StatusConflict, fmt.Sprintf("正在被 %s 编辑", lock.LockedByName))
		return
	}
	if req.NovelID > 0 {
		h.collabSvc.BroadcastLock(req.NovelID, req.EntityType, req.EntityID, userID, req.EntityType+".locked")
	}
	respondOK(c, gin.H{"lock": lock})
}

// DELETE /editing-locks/:type/:entity_id?novel_id=xxx
func (h *CollabHandler) ReleaseLock(c *gin.Context) {
	entityType := c.Param("type")
	entityID, ok := parseID(c, "entity_id")
	if !ok {
		return
	}
	userID := getUserIDFromCtx(c)
	h.collabSvc.ReleaseLock(entityType, entityID, userID)
	// broadcast unlock
	if novelIDStr := c.Query("novel_id"); novelIDStr != "" {
		var novelID uint
		fmt.Sscanf(novelIDStr, "%d", &novelID)
		if novelID > 0 {
			h.collabSvc.BroadcastLock(novelID, entityType, entityID, userID, entityType+".unlocked")
		}
	}
	respondOK(c, gin.H{})
}

// PUT /editing-locks/:type/:entity_id/heartbeat
func (h *CollabHandler) HeartbeatLock(c *gin.Context) {
	entityType := c.Param("type")
	entityID, ok := parseID(c, "entity_id")
	if !ok {
		return
	}
	userID := getUserIDFromCtx(c)
	h.collabSvc.RefreshLock(entityType, entityID, userID)
	respondOK(c, gin.H{})
}

// GET /editing-locks/:type/:entity_id
func (h *CollabHandler) GetLock(c *gin.Context) {
	entityType := c.Param("type")
	entityID, ok := parseID(c, "entity_id")
	if !ok {
		return
	}
	lock, err := h.collabSvc.GetLock(entityType, entityID)
	if err != nil {
		respondOK(c, gin.H{"lock": nil})
		return
	}
	respondOK(c, gin.H{"lock": lock})
}

// GET /novels/:id/events  — SSE 长连接
func (h *CollabHandler) SSEStream(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	userID := getUserIDFromCtx(c)

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	events, unsub := h.collabSvc.Subscribe(novelID)
	defer unsub()

	// 推送 connected 事件
	c.Writer.WriteString(service.CollabEvent{Type: "connected", UserID: userID}.ToSSE())
	c.Writer.Flush()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	clientGone := c.Request.Context().Done()
	for {
		select {
		case <-clientGone:
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			c.Writer.WriteString(evt.ToSSE())
			c.Writer.Flush()
		case <-ticker.C:
			c.Writer.WriteString(": ping\n\n")
			c.Writer.Flush()
		}
	}
}
