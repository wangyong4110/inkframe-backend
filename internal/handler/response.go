package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

func respondOK(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success", "data": data})
}

func respondCreated(c *gin.Context, data interface{}) {
	c.JSON(http.StatusCreated, gin.H{"code": 0, "message": "success", "data": data})
}

func respondBadRequest(c *gin.Context, msg string) {
	c.JSON(http.StatusBadRequest, gin.H{"error": msg})
}

func respondErr(c *gin.Context, status int, msg string) {
	c.JSON(status, gin.H{"error": msg})
}

// PaginationParams holds parsed pagination query parameters.
type PaginationParams struct{ Page, PageSize int }

func parsePagination(c *gin.Context) PaginationParams {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 1
	} else if size > 100 {
		size = 100
	}
	return PaginationParams{page, size}
}
