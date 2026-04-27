package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
	"github.com/inkframe/inkframe-backend/internal/storage"
)

// ItemHandler 物品处理器
type ItemHandler struct {
	itemService *service.ItemService
	chapterSvc  *service.ChapterService
	storageSvc  storage.Service
	taskSvc     *service.TaskService
}

func NewItemHandler(itemService *service.ItemService, chapterSvc *service.ChapterService) *ItemHandler {
	return &ItemHandler{itemService: itemService, chapterSvc: chapterSvc}
}

func (h *ItemHandler) WithStorage(svc storage.Service) *ItemHandler {
	h.storageSvc = svc
	return h
}

func (h *ItemHandler) WithTaskService(svc *service.TaskService) *ItemHandler {
	h.taskSvc = svc
	return h
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

	itemID := uint(id)
	refURL, provider := req.ReferenceImageURL, req.Provider
	tenantID := getTenantID(c)

	task, err := h.taskSvc.Create(tenantID, service.TaskTypeImageGen, "物品图像生成", "item", itemID)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}

	go func(taskID string) {
		h.taskSvc.SetRunning(taskID) //nolint:errcheck
		item, err := h.itemService.GenerateItemImage(tenantID, itemID, refURL, provider)
		if err != nil {
			h.taskSvc.Fail(taskID, err.Error()) //nolint:errcheck
		} else {
			h.taskSvc.Complete(taskID, item) //nolint:errcheck
		}
	}(task.TaskID)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "图像生成任务已提交",
		"data":    gin.H{"task_id": task.TaskID},
	})
}

// GetItemImageTaskStatus 查询物品图像生成任务状态
// GET /api/v1/items/:id/images/:task_id
func (h *ItemHandler) GetItemImageTaskStatus(c *gin.Context) {
	taskID := c.Param("task_id")
	task, err := h.taskSvc.Get(taskID)
	if err != nil {
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

// UploadItemImage 上传物品图片到 OSS，保存 URL 到 item.ImageURL
// POST /api/v1/items/:id/image/upload
func (h *ItemHandler) UploadItemImage(c *gin.Context) {
	if h.storageSvc == nil {
		respondErr(c, http.StatusServiceUnavailable, "storage not configured")
		return
	}
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid item id")
		return
	}
	imgURL, ok := receiveAndUpload(c, "item-images", h.storageSvc)
	if !ok {
		return
	}
	item, err := h.itemService.UpdateItem(uint(id), &model.UpdateItemRequest{ImageURL: imgURL})
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to save image url")
		return
	}
	respondOK(c, gin.H{"url": imgURL, "item": item})
}

// UploadItemReference 上传物品参考图到 OSS，保存 URL 到 item.ReferenceImageURL
// POST /api/v1/items/:id/reference/upload
func (h *ItemHandler) UploadItemReference(c *gin.Context) {
	if h.storageSvc == nil {
		respondErr(c, http.StatusServiceUnavailable, "storage not configured")
		return
	}
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid item id")
		return
	}
	refURL, ok := receiveAndUpload(c, "item-references", h.storageSvc)
	if !ok {
		return
	}
	item, err := h.itemService.UpdateItem(uint(id), &model.UpdateItemRequest{ReferenceImageURL: refURL})
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to save reference image url")
		return
	}
	respondOK(c, gin.H{"url": refURL, "item": item})
}
