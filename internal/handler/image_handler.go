package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// ImageHandler handles generic image operations.
type ImageHandler struct {
	aiSvc     *service.AIService
	novelRepo *repository.NovelRepository
}

func NewImageHandler(aiSvc *service.AIService, novelRepo ...*repository.NovelRepository) *ImageHandler {
	h := &ImageHandler{aiSvc: aiSvc}
	if len(novelRepo) > 0 {
		h.novelRepo = novelRepo[0]
	}
	return h
}

// EditImage POST /images/edit
// Accepts { image_url, instruction, novel_id? } and returns { image_url } with the edited image.
// novel_id is optional; when provided the handler uses the novel's image_style for consistency.
func (h *ImageHandler) EditImage(c *gin.Context) {
	var body struct {
		ImageURL    string `json:"image_url" binding:"required"`
		Instruction string `json:"instruction" binding:"required"`
		NovelID     uint   `json:"novel_id"`
	}
	if !bindJSON(c, &body) {
		return
	}

	imageStyle := ""
	if body.NovelID > 0 && h.novelRepo != nil {
		if novel, err := h.novelRepo.GetByID(body.NovelID); err == nil {
			imageStyle = novel.ImageStyle
		}
	}

	// consistencyWeight=0.4 (<0.7) routes to SeedEditV3 for instruction-based editing
	newURL, err := h.aiSvc.GenerateCharacterThreeViewMulti(
		c.Request.Context(), getTenantID(c), "",
		body.Instruction,
		[]string{body.ImageURL},
		imageStyle, "", "", 0.4,
	)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to edit image")
		return
	}
	respondOK(c, gin.H{"image_url": newURL})
}
