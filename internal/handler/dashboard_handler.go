package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// DashboardHandler 仪表盘统计处理器
type DashboardHandler struct {
	db *gorm.DB
}

func NewDashboardHandler(db *gorm.DB) *DashboardHandler {
	return &DashboardHandler{db: db}
}

// GetStats 获取仪表盘统计数据
// GET /api/v1/dashboard/stats
func (h *DashboardHandler) GetStats(c *gin.Context) {
	tenantID := getTenantID(c)

	// Novel counts
	var novelCount, novelCountMonth int64
	h.db.Model(&model.Novel{}).Where("tenant_id = ?", tenantID).Count(&novelCount)

	monthStart := time.Now().Truncate(24*time.Hour).AddDate(0, 0, -time.Now().Day()+1)
	h.db.Model(&model.Novel{}).
		Where("tenant_id = ? AND created_at >= ?", tenantID, monthStart).
		Count(&novelCountMonth)

	// Chapter counts
	var chapterCount, chapterCountMonth int64
	h.db.Model(&model.Chapter{}).
		Joins("JOIN ink_novel ON ink_chapter.novel_id = ink_novel.id").
		Where("ink_novel.tenant_id = ?", tenantID).
		Count(&chapterCount)
	h.db.Model(&model.Chapter{}).
		Joins("JOIN ink_novel ON ink_chapter.novel_id = ink_novel.id").
		Where("ink_novel.tenant_id = ? AND ink_chapter.created_at >= ?", tenantID, monthStart).
		Count(&chapterCountMonth)

	// Recent 5 chapters (most recently updated)
	var recentChapters []model.Chapter
	h.db.Model(&model.Chapter{}).
		Joins("JOIN ink_novel ON ink_chapter.novel_id = ink_novel.id").
		Where("ink_novel.tenant_id = ?", tenantID).
		Order("ink_chapter.updated_at DESC").
		Limit(5).
		Find(&recentChapters)

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data": gin.H{
			"novel_count":              novelCount,
			"novel_count_this_month":   novelCountMonth,
			"chapter_count":            chapterCount,
			"chapter_count_this_month": chapterCountMonth,
			"token_usage": gin.H{
				"total":       0,
				"by_provider": []interface{}{},
			},
			"recent_chapters":  recentChapters,
			"provider_health":  []interface{}{},
		},
	})
}
