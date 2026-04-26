package handler

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// ItemHandler 物品处理器
type ItemHandler struct {
	itemService *service.ItemService
	chapterSvc  *service.ChapterService
}

func NewItemHandler(itemService *service.ItemService, chapterSvc *service.ChapterService) *ItemHandler {
	return &ItemHandler{itemService: itemService, chapterSvc: chapterSvc}
}

// ListItems GET /novels/:id/items
func (h *ItemHandler) ListItems(c *gin.Context) {
	novelID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid novel id")
		return
	}
	items, err := h.itemService.ListItems(uint(novelID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, items)
}

// CreateItem POST /novels/:id/items
func (h *ItemHandler) CreateItem(c *gin.Context) {
	novelID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid novel id")
		return
	}
	var req model.CreateItemRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	item, err := h.itemService.CreateItem(uint(novelID), &req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondCreated(c, item)
}

// GetItem GET /items/:id
func (h *ItemHandler) GetItem(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid item id")
		return
	}
	item, err := h.itemService.GetItem(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "item not found")
		return
	}
	respondOK(c, item)
}

// UpdateItem PUT /items/:id
func (h *ItemHandler) UpdateItem(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid item id")
		return
	}
	var req model.UpdateItemRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	item, err := h.itemService.UpdateItem(uint(id), &req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, item)
}

// DeleteItem DELETE /items/:id
func (h *ItemHandler) DeleteItem(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid item id")
		return
	}
	if err := h.itemService.DeleteItem(uint(id)); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"message": "item deleted"})
}

// GenerateItemImage 生成物品图像（异步任务）
// POST /api/v1/items/:id/images
// 立即返回 202 + task_id，轮询 GET /items/:id/images/:task_id 获取结果
func (h *ItemHandler) GenerateItemImage(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid item id")
		return
	}
	var req struct {
		ReferenceImageURL string `json:"reference_image_url"`
		Provider          string `json:"provider,omitempty"` // 指定图像生成提供者
	}
	// 忽略解析错误（body 可为空）
	_ = c.ShouldBindJSON(&req)

	taskID := newTaskID("img")
	itemID := uint(id)
	refURL, provider := req.ReferenceImageURL, req.Provider

	task := &AsyncTask{TaskID: taskID, Status: taskStatusPending, CreatedAt: time.Now().Unix()}
	itemImageTasks.store(task)

	go func() {
		task.Status = taskStatusRunning
		itemImageTasks.store(task)
		item, err := h.itemService.GenerateItemImage(itemID, refURL, provider)
		if err != nil {
			task.Status = taskStatusFailed
			task.Error = err.Error()
		} else {
			task.Status = taskStatusCompleted
			task.Data = item
		}
		itemImageTasks.store(task)
	}()

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "图像生成任务已提交",
		"data":    gin.H{"task_id": taskID},
	})
}

// GetItemImageTaskStatus 查询物品图像生成任务状态
// GET /api/v1/items/:id/images/:task_id
func (h *ItemHandler) GetItemImageTaskStatus(c *gin.Context) {
	taskID := c.Param("task_id")
	task, ok := itemImageTasks.load(taskID)
	if !ok {
		respondErr(c, http.StatusNotFound, "task not found")
		return
	}
	respondOK(c, task)
}

// ListEffectiveItems GET /novels/:id/chapters/:chapter_no/items
func (h *ItemHandler) ListEffectiveItems(c *gin.Context) {
	novelID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid novel id")
		return
	}
	chapterNo, err := strconv.Atoi(c.Param("chapter_no"))
	if err != nil {
		respondBadRequest(c, "invalid chapter_no")
		return
	}
	chapter, err := h.chapterSvc.GetChapterByNo(uint(novelID), chapterNo)
	if err != nil {
		respondErr(c, http.StatusNotFound, "chapter not found")
		return
	}
	items, err := h.itemService.ListEffectiveItems(uint(novelID), chapter.ID)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, items)
}

// UpsertChapterItem POST /novels/:id/chapters/:chapter_no/items/:item_id
func (h *ItemHandler) UpsertChapterItem(c *gin.Context) {
	novelID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid novel id")
		return
	}
	chapterNo, err := strconv.Atoi(c.Param("chapter_no"))
	if err != nil {
		respondBadRequest(c, "invalid chapter_no")
		return
	}
	itemID, err := strconv.ParseUint(c.Param("item_id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid item id")
		return
	}
	chapter, err := h.chapterSvc.GetChapterByNo(uint(novelID), chapterNo)
	if err != nil {
		respondErr(c, http.StatusNotFound, "chapter not found")
		return
	}
	var req model.UpsertChapterItemRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	ci, err := h.itemService.UpsertChapterItem(uint(novelID), chapter.ID, uint(itemID), &req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, ci)
}

// DeleteChapterItem DELETE /novels/:id/chapters/:chapter_no/items/:item_id
func (h *ItemHandler) DeleteChapterItem(c *gin.Context) {
	novelID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid novel id")
		return
	}
	chapterNo, err := strconv.Atoi(c.Param("chapter_no"))
	if err != nil {
		respondBadRequest(c, "invalid chapter_no")
		return
	}
	itemID, err := strconv.ParseUint(c.Param("item_id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid item id")
		return
	}
	chapter, err := h.chapterSvc.GetChapterByNo(uint(novelID), chapterNo)
	if err != nil {
		respondErr(c, http.StatusNotFound, "chapter not found")
		return
	}
	if err := h.itemService.DeleteChapterItem(chapter.ID, uint(itemID)); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"message": "chapter item deleted"})
}
