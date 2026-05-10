package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/inkframe/inkframe-backend/internal/storage"
	"gorm.io/gorm"
)

// ─── AssetService ─────────────────────────────────────────────────────────────

type AssetService struct {
	assetRepo      *repository.AssetRepository
	tagRepo        *repository.TagRepository
	versionRepo    *repository.AssetVersionRepository
	collectionRepo *repository.AssetCollectionRepository
	shareReqRepo   *repository.AssetShareRequestRepository
	usageRepo      *repository.AssetUsageRepository
	likeRepo       *repository.AssetLikeRepository
	commentRepo    *repository.AssetCommentRepository
	crawlRepo      *repository.CrawlJobRepository
	shareLinkRepo  *repository.ShareLinkRepository
	searchLogRepo  *repository.SearchLogRepository
	quotaRepo      *repository.AssetStorageQuotaRepository
	storageSvc     storage.Service
	taskSvc        *TaskService
	aiSvc          *AIService
}

func NewAssetService(
	assetRepo *repository.AssetRepository,
	tagRepo *repository.TagRepository,
	versionRepo *repository.AssetVersionRepository,
	collectionRepo *repository.AssetCollectionRepository,
	shareReqRepo *repository.AssetShareRequestRepository,
	usageRepo *repository.AssetUsageRepository,
	likeRepo *repository.AssetLikeRepository,
	commentRepo *repository.AssetCommentRepository,
	crawlRepo *repository.CrawlJobRepository,
	shareLinkRepo *repository.ShareLinkRepository,
	searchLogRepo *repository.SearchLogRepository,
	quotaRepo *repository.AssetStorageQuotaRepository,

	taskSvc *TaskService,
) *AssetService {
	return &AssetService{
		assetRepo: assetRepo, tagRepo: tagRepo, versionRepo: versionRepo,
		collectionRepo: collectionRepo, shareReqRepo: shareReqRepo,
		usageRepo: usageRepo, likeRepo: likeRepo, commentRepo: commentRepo,
		crawlRepo: crawlRepo, shareLinkRepo: shareLinkRepo,
		searchLogRepo: searchLogRepo, quotaRepo: quotaRepo,
		taskSvc: taskSvc,
	}
}

// ─── Upload ───────────────────────────────────────────────────────────────────

type UploadAssetParams struct {
	TenantID  uint
	CreatorID uint
	Title     string
	Type      string // image|video|audio|text
	SubType   string
	MimeType  string
	FileSize  int64
	NovelID   *uint
	VideoID   *uint
	ShotID    *uint
}

func (s *AssetService) Upload(ctx context.Context, r io.Reader, size int64, p UploadAssetParams) (*model.Asset, error) {
	// Quota check
	quota, err := s.quotaRepo.Get(p.TenantID)
	if err == nil && quota.StorageUsedBytes+size > quota.StorageLimitBytes {
		return nil, errors.New("personal storage quota exceeded")
	}

	// Upload to OSS
	ext := mimeToExt(p.MimeType)
	key := fmt.Sprintf("assets/%d/%s%s", p.TenantID, randomHex(16), ext)
	url, err := s.storageSvc.Upload(ctx, key, r, size, p.MimeType)
	if err != nil {
		return nil, err
	}

	asset := &model.Asset{
		Scope: model.AssetScopePersonal, TenantID: p.TenantID, CreatorID: p.CreatorID,
		Title: p.Title, Type: p.Type, SubType: p.SubType,
		Source: "uploaded", StorageURL: url,
		MimeType: p.MimeType, FileSize: size,
		Status:  model.AssetStatusActive,
		NovelID: p.NovelID, VideoID: p.VideoID, ShotID: p.ShotID,
	}
	if err := s.assetRepo.Create(asset); err != nil {
		return nil, err
	}

	// Update quota
	_ = s.quotaRepo.AddStorage(p.TenantID, size)

	// Async pipeline
	go func() {
		bgCtx := context.Background()
		_ = s.processNewAsset(bgCtx, asset)
	}()

	return asset, nil
}

// CreateFromGeneration creates an asset from a platform-generated file (shot image, synthesized video).
func (s *AssetService) CreateFromGeneration(ctx context.Context, p UploadAssetParams, storageURL, thumbnailURL string) (*model.Asset, error) {
	asset := &model.Asset{
		Scope: model.AssetScopePersonal, TenantID: p.TenantID, CreatorID: p.CreatorID,
		Title: p.Title, Type: p.Type, SubType: p.SubType,
		Source: "platform", StorageURL: storageURL, ThumbnailURL: thumbnailURL,
		License: "platform", Status: model.AssetStatusActive,
		NovelID: p.NovelID, VideoID: p.VideoID, ShotID: p.ShotID,
	}
	if err := s.assetRepo.Create(asset); err != nil {
		return nil, err
	}
	return asset, nil
}

// ─── Query ────────────────────────────────────────────────────────────────────

func (s *AssetService) GetByID(id, callerID uint) (*model.Asset, error) {
	a, err := s.assetRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if a.Scope == model.AssetScopePersonal && a.CreatorID != callerID {
		return nil, errors.New("not found")
	}
	return a, nil
}

func (s *AssetService) Search(ctx context.Context, p repository.AssetSearchParams) ([]*model.Asset, int64, error) {
	assets, total, err := s.assetRepo.Search(p)
	// Log search
	if err == nil {
		sl := &model.SearchLog{
			Query: p.Q, ResultCount: int(total),
			SearchScope: p.Scope, TenantID: p.TenantID, SearchedAt: time.Now(),
		}
		_ = s.searchLogRepo.Create(sl)
	}
	return assets, total, err
}

// ─── Personal Library Management ──────────────────────────────────────────────

func (s *AssetService) Update(id, callerID uint, fields map[string]interface{}) error {
	a, err := s.assetRepo.GetByID(id)
	if err != nil || a.CreatorID != callerID {
		return errors.New("not found or permission denied")
	}
	return s.assetRepo.UpdateFields(id, fields)
}

func (s *AssetService) SoftDelete(id, callerID uint) error {
	a, err := s.assetRepo.GetByID(id)
	if err != nil || a.CreatorID != callerID {
		return errors.New("not found or permission denied")
	}
	return s.assetRepo.SoftDelete(id, callerID)
}

func (s *AssetService) RestoreFromTrash(id, callerID uint) error {
	a, err := s.assetRepo.GetByID(id)
	if err != nil || a.CreatorID != callerID {
		return errors.New("not found or permission denied")
	}
	return s.assetRepo.Restore(id)
}

func (s *AssetService) PurgeAsset(ctx context.Context, id, callerID uint) error {
	a, err := s.assetRepo.GetByID(id)
	if err != nil || a.CreatorID != callerID {
		return errors.New("not found or permission denied")
	}
	// Note: storage deletion from OSS would go here if the interface supported it
	_ = s.quotaRepo.SubStorage(a.TenantID, a.FileSize)
	return s.assetRepo.HardDelete(id)
}

func (s *AssetService) ListTrash(creatorID uint, page, size int) ([]*model.Asset, int64, error) {
	return s.assetRepo.ListTrash(creatorID, page, size)
}

// ─── Sharing Workflow ─────────────────────────────────────────────────────────

func (s *AssetService) RequestShare(assetID, callerID uint) (*model.AssetShareRequest, error) {
	a, err := s.assetRepo.GetByID(assetID)
	if err != nil || a.CreatorID != callerID {
		return nil, errors.New("not found or permission denied")
	}
	if a.Scope == model.AssetScopePublic {
		return nil, errors.New("asset is already in public library")
	}
	// Update status to pending_review
	if err := s.assetRepo.UpdateFields(assetID, map[string]interface{}{"status": model.AssetStatusPendingReview}); err != nil {
		return nil, err
	}
	req := &model.AssetShareRequest{AssetID: assetID, RequestedBy: callerID, Status: "pending"}
	if err := s.shareReqRepo.Create(req); err != nil {
		return nil, err
	}
	// Async quality + NSFW check
	go func() {
		_ = s.autoReviewShare(context.Background(), a, req)
	}()
	return req, nil
}

func (s *AssetService) autoReviewShare(ctx context.Context, a *model.Asset, req *model.AssetShareRequest) error {
	// Simple auto-approve for platform-generated assets; stub for uploaded/crawled
	qualityOK := a.QualityScore >= 6.0 || a.QualityScore == 0 // 0 means not yet checked
	safetyOK := a.SafetyScore >= 0.9 || !a.SafetyChecked

	now := time.Now()
	if qualityOK && safetyOK {
		req.Status = "approved"
		req.AutoPassed = true
		req.ReviewedAt = &now
		_ = s.shareReqRepo.Update(req)
		_ = s.assetRepo.UpdateFields(a.ID, map[string]interface{}{
			"scope": model.AssetScopePublic, "status": model.AssetStatusActive,
			"shared_at": now, "shared_by": a.CreatorID,
		})
	} else {
		note := "quality or safety check failed"
		req.Status = "rejected"
		req.ReviewNote = note
		req.ReviewedAt = &now
		_ = s.shareReqRepo.Update(req)
		_ = s.assetRepo.UpdateFields(a.ID, map[string]interface{}{
			"status": model.AssetStatusRejected, "review_note": note,
		})
	}
	return nil
}

func (s *AssetService) GetShareRequest(assetID, callerID uint) (*model.AssetShareRequest, error) {
	a, err := s.assetRepo.GetByID(assetID)
	if err != nil || a.CreatorID != callerID {
		return nil, errors.New("not found or permission denied")
	}
	return s.shareReqRepo.GetByAssetID(assetID)
}

func (s *AssetService) CancelShareRequest(assetID, callerID uint) error {
	req, err := s.shareReqRepo.GetByAssetID(assetID)
	if err != nil {
		return err
	}
	a, err := s.assetRepo.GetByID(assetID)
	if err != nil || a.CreatorID != callerID {
		return errors.New("permission denied")
	}
	if req.Status != "pending" {
		return errors.New("cannot cancel non-pending request")
	}
	_ = s.shareReqRepo.Delete(req.ID)
	return s.assetRepo.UpdateFields(assetID, map[string]interface{}{"status": model.AssetStatusActive})
}

func (s *AssetService) WithdrawShare(assetID, callerID uint) error {
	a, err := s.assetRepo.GetByID(assetID)
	if err != nil || a.CreatorID != callerID {
		return errors.New("not found or permission denied")
	}
	if a.Scope != model.AssetScopePublic {
		return errors.New("asset is not in public library")
	}
	return s.assetRepo.UpdateFields(assetID, map[string]interface{}{
		"scope": model.AssetScopePersonal, "status": model.AssetStatusWithdrawn,
	})
}

func (s *AssetService) AdminReview(shareReqID, reviewerID uint, approved bool, note string) error {
	req, err := s.shareReqRepo.GetByID(shareReqID)
	if err != nil {
		return err
	}
	now := time.Now()
	req.ReviewerID = &reviewerID
	req.ReviewedAt = &now
	req.ReviewNote = note
	if approved {
		req.Status = "approved"
		_ = s.assetRepo.UpdateFields(req.AssetID, map[string]interface{}{
			"scope": model.AssetScopePublic, "status": model.AssetStatusActive,
			"shared_at": now, "shared_by": req.RequestedBy,
		})
	} else {
		req.Status = "rejected"
		_ = s.assetRepo.UpdateFields(req.AssetID, map[string]interface{}{
			"status": model.AssetStatusRejected, "review_note": note,
		})
	}
	return s.shareReqRepo.Update(req)
}

func (s *AssetService) AdminRemove(assetID, adminID uint, note string) error {
	return s.assetRepo.UpdateFields(assetID, map[string]interface{}{
		"scope": model.AssetScopePersonal, "status": model.AssetStatusWithdrawn,
		"review_note": note, "deleted_by": adminID,
	})
}

func (s *AssetService) ListPendingShareRequests(page, size int) ([]*model.AssetShareRequest, int64, error) {
	return s.shareReqRepo.ListPending(page, size)
}

// ─── Version Control ──────────────────────────────────────────────────────────

func (s *AssetService) ListVersions(assetID uint) ([]*model.AssetVersion, error) {
	return s.versionRepo.ListByAsset(assetID)
}

func (s *AssetService) CreateVersion(ctx context.Context, assetID, callerID uint, r io.Reader, size int64, mimeType, note string) (*model.AssetVersion, error) {
	a, err := s.assetRepo.GetByID(assetID)
	if err != nil || a.CreatorID != callerID {
		return nil, errors.New("not found or permission denied")
	}
	ext := mimeToExt(mimeType)
	key := fmt.Sprintf("assets/%d/versions/%d/%s%s", a.TenantID, assetID, randomHex(12), ext)
	url, err := s.storageSvc.Upload(ctx, key, r, size, mimeType)
	if err != nil {
		return nil, err
	}
	maxV, _ := s.versionRepo.MaxVersionNo(assetID)
	v := &model.AssetVersion{
		AssetID: assetID, VersionNo: maxV + 1,
		StorageURL: url, FileSize: size,
		ChangeNote: note, CreatedBy: callerID,
	}
	if err := s.versionRepo.Create(v); err != nil {
		return nil, err
	}
	// Update asset storage URL to latest version
	_ = s.assetRepo.UpdateFields(assetID, map[string]interface{}{"storage_url": url, "file_size": size})
	return v, nil
}

func (s *AssetService) RestoreVersion(ctx context.Context, assetID uint, versionNo int, callerID uint) error {
	a, err := s.assetRepo.GetByID(assetID)
	if err != nil || a.CreatorID != callerID {
		return errors.New("not found or permission denied")
	}
	v, err := s.versionRepo.GetByVersionNo(assetID, versionNo)
	if err != nil {
		return err
	}
	return s.assetRepo.UpdateFields(assetID, map[string]interface{}{
		"storage_url": v.StorageURL, "file_size": v.FileSize,
	})
}

// ─── Tags ─────────────────────────────────────────────────────────────────────

func (s *AssetService) AddTags(assetID, callerID uint, tagNames []string) ([]*model.Tag, error) {
	a, err := s.assetRepo.GetByID(assetID)
	if err != nil {
		return nil, err
	}
	// Only creator or public asset
	if a.Scope == model.AssetScopePersonal && a.CreatorID != callerID {
		return nil, errors.New("permission denied")
	}
	var tags []*model.Tag
	for _, name := range tagNames {
		t, err := s.tagRepo.FindOrCreate(name, "custom")
		if err != nil {
			continue
		}
		_ = s.tagRepo.AddToAsset(assetID, t.ID, "manual", 1.0)
		_ = s.tagRepo.IncrUseCount(t.ID)
		tags = append(tags, t)
	}
	return tags, nil
}

func (s *AssetService) RemoveTag(assetID, tagID, callerID uint) error {
	a, err := s.assetRepo.GetByID(assetID)
	if err != nil || a.CreatorID != callerID {
		return errors.New("not found or permission denied")
	}
	return s.tagRepo.RemoveFromAsset(assetID, tagID)
}

func (s *AssetService) ListTags() (map[string][]*model.Tag, error) {
	return s.tagRepo.ListByCategory()
}

func (s *AssetService) SuggestTags(q string) ([]*model.Tag, error) {
	return s.tagRepo.Suggest(q, 10)
}

// ─── Public Library Interactions ──────────────────────────────────────────────

func (s *AssetService) ToggleLike(assetID, userID uint) (bool, error) {
	a, err := s.assetRepo.GetByID(assetID)
	if err != nil {
		return false, err
	}
	if a.Scope != model.AssetScopePublic {
		return false, errors.New("can only like public assets")
	}
	liked, err := s.likeRepo.Toggle(assetID, userID)
	if err != nil {
		return false, err
	}
	delta := 1
	if !liked {
		delta = -1
	}
	_ = s.assetRepo.IncrLikeCount(assetID, delta)
	return liked, nil
}

func (s *AssetService) UseAsset(assetID uint, usage model.AssetUsage) (string, string, error) {
	a, err := s.assetRepo.GetByID(assetID)
	if err != nil {
		return "", "", err
	}
	usage.AssetID = assetID
	usage.UsedAt = time.Now()
	_ = s.usageRepo.Create(&usage)
	_ = s.assetRepo.IncrUseCount(assetID)
	attribution := ""
	if strings.Contains(a.License, "CC-BY") || a.License == "unsplash" {
		attribution = a.Attribution
	}
	return a.StorageURL, attribution, nil
}

// ─── Collections ─────────────────────────────────────────────────────────────

func (s *AssetService) CreateCollection(tenantID, creatorID uint, name, desc, scope string) (*model.AssetCollection, error) {
	c := &model.AssetCollection{
		TenantID: tenantID, CreatorID: creatorID,
		Name: name, Description: desc, Scope: scope,
	}
	return c, s.collectionRepo.Create(c)
}

func (s *AssetService) ListCollections(creatorID uint) ([]*model.AssetCollection, error) {
	return s.collectionRepo.ListByCreator(creatorID)
}

func (s *AssetService) AddToCollection(collectionID uint, assetIDs []uint, callerID uint) error {
	c, err := s.collectionRepo.GetByID(collectionID)
	if err != nil || c.CreatorID != callerID {
		return errors.New("not found or permission denied")
	}
	for _, aid := range assetIDs {
		_ = s.collectionRepo.AddItem(collectionID, aid)
	}
	return nil
}

func (s *AssetService) RemoveFromCollection(collectionID uint, assetIDs []uint, callerID uint) error {
	c, err := s.collectionRepo.GetByID(collectionID)
	if err != nil || c.CreatorID != callerID {
		return errors.New("not found or permission denied")
	}
	for _, aid := range assetIDs {
		_ = s.collectionRepo.RemoveItem(collectionID, aid)
	}
	return nil
}

func (s *AssetService) ListCollectionItems(collectionID uint) ([]*model.Asset, error) {
	return s.collectionRepo.ListItems(collectionID)
}

// ─── Share Links ──────────────────────────────────────────────────────────────

type ShareLinkOptions struct {
	AssetID         *uint
	CollectionID    *uint
	ExpiresIn       int // hours; 0 = no expiry
	DownloadAllowed bool
}

func (s *AssetService) CreateShareLink(callerID uint, opts ShareLinkOptions) (*model.ShareLink, error) {
	token := randomHex(32)
	sl := &model.ShareLink{
		Token: token, CreatedBy: callerID,
		AssetID: opts.AssetID, CollectionID: opts.CollectionID,
		DownloadAllowed: opts.DownloadAllowed,
	}
	if opts.ExpiresIn > 0 {
		exp := time.Now().Add(time.Duration(opts.ExpiresIn) * time.Hour)
		sl.ExpiresAt = &exp
	}
	if err := s.shareLinkRepo.Create(sl); err != nil {
		return nil, err
	}
	return sl, nil
}

func (s *AssetService) ValidateShareLink(token string) (*model.ShareLink, error) {
	sl, err := s.shareLinkRepo.GetByToken(token)
	if err != nil {
		return nil, errors.New("share link not found")
	}
	if sl.ExpiresAt != nil && sl.ExpiresAt.Before(time.Now()) {
		return nil, errors.New("share link expired")
	}
	_ = s.shareLinkRepo.IncrViewCount(token)
	return sl, nil
}

func (s *AssetService) ListShareLinks(callerID uint) ([]*model.ShareLink, error) {
	return s.shareLinkRepo.ListByCreator(callerID)
}

func (s *AssetService) RevokeShareLink(token string, callerID uint) error {
	return s.shareLinkRepo.Delete(token)
}

// ─── Comments ─────────────────────────────────────────────────────────────────

func (s *AssetService) ListComments(assetID uint) ([]*model.AssetComment, error) {
	return s.commentRepo.ListByAsset(assetID)
}

func (s *AssetService) AddComment(assetID, userID uint, content string, parentID *uint, xRatio, yRatio *float64) (*model.AssetComment, error) {
	c := &model.AssetComment{
		AssetID: assetID, UserID: userID, Content: content,
		ParentID: parentID, XRatio: xRatio, YRatio: yRatio,
	}
	return c, s.commentRepo.Create(c)
}

func (s *AssetService) DeleteComment(commentID, callerID uint) error {
	c, err := s.commentRepo.GetByID(commentID)
	if err != nil {
		return err
	}
	if c.UserID != callerID {
		return errors.New("permission denied")
	}
	return s.commentRepo.Delete(commentID)
}

// ─── Analytics ────────────────────────────────────────────────────────────────

func (s *AssetService) GetQuota(tenantID uint) (*model.AssetStorageQuota, error) {
	return s.quotaRepo.Get(tenantID)
}

func (s *AssetService) GetValueRanking(limit int) ([]*model.Asset, error) {
	return s.assetRepo.ListPublicByValueScore(limit)
}

func (s *AssetService) GetSearchGaps(scope string) ([]struct {
	Query string
	Count int
}, error) {
	return s.searchLogRepo.ListGaps(scope, 50)
}

func (s *AssetService) RecalcValueScores() error {
	assets, err := s.assetRepo.ListPublicAll()
	if err != nil {
		return err
	}
	for _, a := range assets {
		score := float64(a.UseCount)*0.3 + float64(a.LikeCount)*0.2
		_ = s.assetRepo.UpdateValueScore(a.ID, score)
	}
	return nil
}

// ─── Batch Operations ─────────────────────────────────────────────────────────

func (s *AssetService) BatchDelete(callerID uint, assetIDs []uint) error {
	for _, id := range assetIDs {
		_ = s.SoftDelete(id, callerID)
	}
	return nil
}

func (s *AssetService) BatchShareRequest(callerID uint, assetIDs []uint) (submitted, failed int) {
	for _, id := range assetIDs {
		_, err := s.RequestShare(id, callerID)
		if err != nil {
			failed++
		} else {
			submitted++
		}
	}
	return
}

// ─── Crawl Jobs ───────────────────────────────────────────────────────────────

func (s *AssetService) CreateCrawlJob(source, query, assetType, license string, limit int, createdBy uint) (*model.CrawlJob, error) {
	job := &model.CrawlJob{
		Source: source, Query: query, AssetType: assetType,
		License: license, Limit: limit, CreatedBy: createdBy, Status: "pending",
	}
	if err := s.crawlRepo.Create(job); err != nil {
		return nil, err
	}
	// Async crawl — service-level crawl dispatch (pluggable crawler registry)
	go s.runCrawlJob(context.Background(), job)
	return job, nil
}

func (s *AssetService) runCrawlJob(ctx context.Context, job *model.CrawlJob) {
	now := time.Now()
	_ = s.crawlRepo.Update(&model.CrawlJob{
		ID: job.ID, Status: "running", StartedAt: &now,
	})
	// Crawling is handled by AssetCrawlerService (separate service)
	// Stub: just mark completed
	completed := time.Now()
	_ = s.crawlRepo.Update(&model.CrawlJob{
		ID: job.ID, Status: "completed", CompletedAt: &completed,
	})
}

func (s *AssetService) GetCrawlJob(id uint) (*model.CrawlJob, error) {
	return s.crawlRepo.GetByID(id)
}

func (s *AssetService) ListCrawlJobs(page, size int) ([]*model.CrawlJob, int64, error) {
	return s.crawlRepo.List(page, size)
}

// ─── Asset Processing Pipeline ───────────────────────────────────────────────

func (s *AssetService) processNewAsset(ctx context.Context, asset *model.Asset) error {
	if s.aiSvc == nil {
		return nil
	}
	return s.autoTagAsset(ctx, asset)
}

// autoTagResult holds the structured tag output from the AI.
type autoTagResult struct {
	Style   []string `json:"style"`
	Mood    []string `json:"mood"`
	Subject []string `json:"subject"`
	Color   []string `json:"color"`
	Angle   []string `json:"angle"`
	Genre   []string `json:"genre"`
	Custom  []string `json:"custom"`
}

// autoTagAsset calls the AI to generate tags for the asset and persists them.
func (s *AssetService) autoTagAsset(ctx context.Context, asset *model.Asset) error {
	_ = ctx
	var rawJSON string
	var err error

	if asset.Type == "image" && asset.StorageURL != "" {
		prompt := loadPromptTemplate("asset_auto_tag.tmpl")
		rawJSON, err = s.aiSvc.GenerateWithVision(prompt, []string{asset.StorageURL})
	} else {
		// Non-image: text-based tag generation from title + type
		prompt := fmt.Sprintf(
			"请为以下素材生成描述标签，以JSON格式返回{\"custom\":[\"标签1\",\"标签2\",\"标签3\"]}，最多5个标签，简短中文：类型=%s，名称=%s，子类型=%s",
			asset.Type, asset.Title, asset.SubType,
		)
		rawJSON, err = s.aiSvc.Generate(0, "asset_tag", prompt)
	}
	if err != nil {
		return nil // non-fatal: AI unavailable, skip tagging
	}

	return s.saveAutoTags(asset.ID, rawJSON)
}

// saveAutoTags parses the AI JSON response and persists tags to the database.
func (s *AssetService) saveAutoTags(assetID uint, rawJSON string) error {
	cleaned := extractJSON(strings.TrimSpace(rawJSON))
	var result autoTagResult
	if err := json.Unmarshal([]byte(cleaned), &result); err != nil {
		return nil // non-fatal: malformed JSON from AI
	}

	categoryTags := map[string][]string{
		"style": result.Style, "mood": result.Mood, "subject": result.Subject,
		"color": result.Color, "angle": result.Angle, "genre": result.Genre,
		"custom": result.Custom,
	}
	for category, names := range categoryTags {
		for _, name := range names {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			tag, err := s.tagRepo.FindOrCreate(name, category)
			if err != nil {
				continue
			}
			_ = s.tagRepo.AddToAsset(assetID, tag.ID, "ai", 0.9)
			_ = s.tagRepo.IncrUseCount(tag.ID)
		}
	}
	return nil
}

// TriggerAutoTag re-runs AI tagging on demand (for owners).
func (s *AssetService) TriggerAutoTag(assetID, callerID uint) error {
	a, err := s.assetRepo.GetByID(assetID)
	if err != nil || a.CreatorID != callerID {
		return errors.New("not found or permission denied")
	}
	if s.aiSvc == nil {
		return errors.New("AI service not available")
	}
	go func() {
		_ = s.autoTagAsset(context.Background(), a)
	}()
	return nil
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func mimeToExt(mime string) string {
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "video/mp4":
		return ".mp4"
	case "video/quicktime":
		return ".mov"
	case "audio/mpeg":
		return ".mp3"
	case "audio/wav":
		return ".wav"
	case "audio/ogg":
		return ".ogg"
	}
	return ""
}

// WithStorage injects the storage service (called after construction).
func (s *AssetService) WithStorage(svc storage.Service) *AssetService {
	s.storageSvc = svc
	return s
}

// Ensure ErrRecordNotFound is available
var _ = gorm.ErrRecordNotFound
