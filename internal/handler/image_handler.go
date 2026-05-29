package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// ImageHandler handles generic image operations.
type ImageHandler struct {
	aiSvc *service.AIService
}

func NewImageHandler(aiSvc *service.AIService) *ImageHandler {
	return &ImageHandler{aiSvc: aiSvc}
}

// EditImage POST /images/edit
// Accepts { image_url, instruction, novel_id? } and returns { image_url } with the edited image.
// Uses SeedEditV3 with low scale to preserve the original image while applying the instruction.
func (h *ImageHandler) EditImage(c *gin.Context) {
	var body struct {
		ImageURL    string `json:"image_url" binding:"required"`
		Instruction string `json:"instruction" binding:"required"`
		NovelID     uint   `json:"novel_id"`
	}
	if !bindJSON(c, &body) {
		return
	}

	newURL, err := h.aiSvc.EditImageWithInstruction(
		c.Request.Context(), getTenantID(c), body.ImageURL, body.Instruction,
	)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to edit image: "+err.Error())
		return
	}
	respondOK(c, gin.H{"image_url": newURL})
}
