package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// WebSearchHandler exposes the internal web search endpoint used as the
// default MCP tool endpoint for "web_search".
type WebSearchHandler struct {
	searcher service.WebSearcher
}

func NewWebSearchHandler(searcher service.WebSearcher) *WebSearchHandler {
	return &WebSearchHandler{searcher: searcher}
}

// Search handles POST /api/v1/tools/web-search
// Body:    {"query": "...", "max_results": 3}
// Returns: {"results": [{title, url, content, date, score, site, favicon}]}
func (h *WebSearchHandler) Search(c *gin.Context) {
	var req struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Query == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query is required"})
		return
	}
	if req.MaxResults <= 0 {
		req.MaxResults = 3
	}
	if req.MaxResults > 50 {
		req.MaxResults = 50
	}

	ctx, cancel := requestContext(c, 15*time.Second)
	defer cancel()

	results, err := h.searcher.Search(ctx, req.Query, req.MaxResults)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"results":  results,
		"provider": h.searcher.Name(),
	})
}

// SearchPro handles POST /api/v1/tools/web-search/pro
// 腾讯 WSA SearchPro 完整参数接口；非 tencent-wsa 时降级到 Search。
// Body: {"query":"...","cnt":10,"mode":2,"site":"zhihu.com","from_time":0,"to_time":0,"industry":"news"}
// Returns: {"results":[...],"provider":"tencent-wsa"}
func (h *WebSearchHandler) SearchPro(c *gin.Context) {
	var req struct {
		Query    string `json:"query"`
		Cnt      int    `json:"cnt"`
		Mode     int    `json:"mode"`
		Site     string `json:"site"`
		FromTime int64  `json:"from_time"`
		ToTime   int64  `json:"to_time"`
		Industry string `json:"industry"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Query == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query is required"})
		return
	}
	if req.Cnt <= 0 {
		req.Cnt = 10
	}
	if req.Cnt > 50 {
		req.Cnt = 50
	}

	ctx, cancel := requestContext(c, 20*time.Second)
	defer cancel()

	// 如果底层是腾讯 WSA，使用完整参数
	if wsa, ok := h.searcher.(*service.TencentWSASearcher); ok {
		opts := service.TencentWSAOptions{
			Mode:     req.Mode,
			Site:     req.Site,
			FromTime: req.FromTime,
			ToTime:   req.ToTime,
			Cnt:      req.Cnt,
			Industry: req.Industry,
		}
		results, err := wsa.SearchPro(ctx, req.Query, opts)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"results": results, "provider": "tencent-wsa"})
		return
	}

	// 非腾讯 WSA 时降级到标准接口
	results, err := h.searcher.Search(ctx, req.Query, req.Cnt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"results": results, "provider": h.searcher.Name()})
}
