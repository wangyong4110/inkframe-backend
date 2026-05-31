package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/inkframe/inkframe-backend/internal/storage"
	"golang.org/x/crypto/bcrypt"
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
	storageSvc    storage.Service
	taskSvc       *TaskService
	aiSvc         *AIService
	crawlProxyURL string
	unsplashKey   string
	freesoundKey  string
	pixabayKey    string
	// crawlMu guards the ExistsByExternalID+Create sequence for crawled assets
	crawlMu sync.Map
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

// crawlUpsert atomically checks whether an asset with the given externalID already exists
// and, if not, creates it. Returns (true, nil) on insert; (false, nil) if already exists.
func (s *AssetService) crawlUpsert(externalID string, create func() error) (bool, error) {
	mu, _ := s.crawlMu.LoadOrStore(externalID, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	exists, err := s.assetRepo.ExistsByExternalID(externalID)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	return true, create()
}

func (s *AssetService) WithCrawlProxy(proxyURL string) *AssetService {
	s.crawlProxyURL = proxyURL
	return s
}

func (s *AssetService) WithUnsplashKey(key string) *AssetService {
	s.unsplashKey = key
	return s
}

func (s *AssetService) WithFreesoundKey(key string) *AssetService {
	s.freesoundKey = key
	return s
}

func (s *AssetService) WithPixabayKey(key string) *AssetService {
	s.pixabayKey = key
	return s
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
	// Best-effort storage deletion; errors are non-fatal
	if s.storageSvc != nil && a.StorageURL != "" {
		_ = s.storageSvc.Delete(ctx, a.StorageURL)
	}
	if s.storageSvc != nil && a.ThumbnailURL != "" {
		_ = s.storageSvc.Delete(ctx, a.ThumbnailURL)
	}
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
	Password        string // plain-text; will be bcrypt-hashed before storage
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
	if opts.Password != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(opts.Password), bcrypt.DefaultCost)
		if err != nil {
			return nil, fmt.Errorf("hash password: %w", err)
		}
		sl.Password = string(hash)
	}
	if err := s.shareLinkRepo.Create(sl); err != nil {
		return nil, err
	}
	return sl, nil
}

func (s *AssetService) ValidateShareLink(token, password string) (*model.ShareLink, error) {
	sl, err := s.shareLinkRepo.GetByToken(token)
	if err != nil {
		return nil, errors.New("share link not found")
	}
	if sl.ExpiresAt != nil && sl.ExpiresAt.Before(time.Now()) {
		return nil, errors.New("share link expired")
	}
	if sl.Password != "" {
		if password == "" {
			return nil, errors.New("share link requires a password")
		}
		if err := bcrypt.CompareHashAndPassword([]byte(sl.Password), []byte(password)); err != nil {
			return nil, errors.New("incorrect password")
		}
	}
	_ = s.shareLinkRepo.IncrViewCount(token)
	return sl, nil
}

func (s *AssetService) ListShareLinks(callerID uint) ([]*model.ShareLink, error) {
	return s.shareLinkRepo.ListByCreator(callerID)
}

func (s *AssetService) RevokeShareLink(token string, callerID uint) error {
	sl, err := s.shareLinkRepo.GetByToken(token)
	if err != nil {
		return errors.New("share link not found")
	}
	if sl.CreatedBy != callerID {
		return errors.New("permission denied")
	}
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

func (s *AssetService) CreateCrawlJob(tenantID uint, source, query, assetType, license string, limit int, createdBy uint) (*model.CrawlJob, error) {
	job := &model.CrawlJob{
		TenantID: tenantID, Source: source, Query: query, AssetType: assetType,
		License: license, Limit: limit, CreatedBy: createdBy, Status: "pending",
	}
	if err := s.crawlRepo.Create(job); err != nil {
		return nil, err
	}
	task, err := s.taskSvc.Create(tenantID, TaskTypeCrawlJob, source+": "+query, "crawl_job", job.ID)
	if err != nil {
		return nil, err
	}
	job.TaskID = task.TaskID
	_ = s.crawlRepo.SetTaskID(job.ID, task.TaskID)

	ctx, cancel := context.WithCancel(context.Background())
	s.taskSvc.RegisterCancel(task.TaskID, cancel)
	go s.runCrawlJob(ctx, job)
	return job, nil
}

func (s *AssetService) RetryCrawlJob(id uint) (*model.CrawlJob, error) {
	job, err := s.crawlRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if job.Status != "failed" && job.Status != "cancelled" {
		return nil, errors.New("only failed or cancelled jobs can be retried")
	}
	if err := s.crawlRepo.Reset(id); err != nil {
		return nil, err
	}
	job.Status = "pending"
	job.Imported, job.Skipped, job.Failed, job.TotalFound = 0, 0, 0, 0
	job.ErrorMsg = ""
	job.StartedAt, job.CompletedAt = nil, nil

	task, err := s.taskSvc.Create(job.TenantID, TaskTypeCrawlJob, job.Source+": "+job.Query, "crawl_job", job.ID)
	if err != nil {
		return nil, err
	}
	job.TaskID = task.TaskID
	_ = s.crawlRepo.SetTaskID(job.ID, task.TaskID)

	ctx, cancel := context.WithCancel(context.Background())
	s.taskSvc.RegisterCancel(task.TaskID, cancel)
	go s.runCrawlJob(ctx, job)
	return job, nil
}

func (s *AssetService) CancelCrawlJob(id uint) error {
	job, err := s.crawlRepo.GetByID(id)
	if err != nil {
		return err
	}
	if job.TaskID != "" {
		return s.taskSvc.Cancel(job.TaskID)
	}
	// Legacy job without task_id: mark cancelled directly
	return s.crawlRepo.UpdateFinal(id, "cancelled", 0, "manually cancelled", nil)
}

// RecoverOrphanedCrawlJobs marks jobs stuck in running/pending as failed.
// Should be called once at server startup.
func (s *AssetService) RecoverOrphanedCrawlJobs() {
	_ = s.crawlRepo.MarkRunningAsFailed()
}

func (s *AssetService) runCrawlJob(ctx context.Context, job *model.CrawlJob) {
	defer s.taskSvc.DeregisterCancel(job.TaskID)
	_ = s.taskSvc.SetRunning(job.TaskID)

	now := time.Now()
	_ = s.crawlRepo.UpdateFinal(job.ID, "running", 0, "", &now)

	var imported, skipped, failed, totalFound int
	var errMsg string

	switch job.Source {
	case "bbc-sfx":
		imported, skipped, failed, totalFound, errMsg = s.crawlBBCSFX(ctx, job)
	case "wikimedia":
		imported, skipped, failed, totalFound, errMsg = s.crawlWikimedia(ctx, job)
	case "unsplash":
		imported, skipped, failed, totalFound, errMsg = s.crawlUnsplash(ctx, job)
	case "freesound":
		imported, skipped, failed, totalFound, errMsg = s.crawlFreesound(ctx, job)
	case "pixabay":
		imported, skipped, failed, totalFound, errMsg = s.crawlPixabay(ctx, job)
	default:
		errMsg = "unsupported crawl source: " + job.Source
	}

	completed := time.Now()
	status := "completed"
	if ctx.Err() != nil {
		status = "cancelled"
		errMsg = ""
	} else if errMsg != "" && imported == 0 {
		status = "failed"
	}
	_ = s.crawlRepo.UpdateProgress(job.ID, imported, skipped, failed)
	_ = s.crawlRepo.UpdateFinal(job.ID, status, totalFound, errMsg, &completed)

	result := map[string]int{"imported": imported, "skipped": skipped, "failed": failed, "total_found": totalFound}
	switch status {
	case "completed":
		_ = s.taskSvc.Complete(job.TaskID, result)
	case "failed":
		_ = s.taskSvc.Fail(job.TaskID, errMsg)
	}
}


// crawlBBCSFX fetches audio assets from BBC Sound Effects and saves them to the public library.
// API: https://sound-effects.bbcrewind.co.uk/api/sfx/search?q=<query>&limit=<n>&from=<offset>
// License: BBC RemArc Licence (free for personal, educational, and research use).
func (s *AssetService) crawlBBCSFX(ctx context.Context, job *model.CrawlJob) (imported, skipped, failed, totalFound int, errMsg string) {
	limit := job.Limit
	if limit <= 0 {
		limit = 20
	}

	httpClient := buildCrawlHTTPClient(s.crawlProxyURL, 10*time.Second)
	pageSize := 20
	from := 0

	for imported+skipped+failed < limit {
		if err := ctx.Err(); err != nil {
			errMsg = "context cancelled"
			return
		}

		need := limit - (imported + skipped + failed)
		batchSize := pageSize
		if need < batchSize {
			batchSize = need
		}

		apiURL := fmt.Sprintf(
			"https://sound-effects.bbcrewind.co.uk/api/sfx/search?q=%s&limit=%d&from=%d",
			url.QueryEscape(job.Query), batchSize, from,
		)
		var body []byte
		var fetchErr error
		for attempt := 0; attempt <= 3; attempt++ {
			if attempt > 0 {
				select {
				case <-ctx.Done():
					errMsg = "context cancelled"
					return
				case <-time.After(time.Duration(attempt) * time.Second):
				}
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
			if err != nil {
				errMsg = err.Error()
				return
			}
			req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; InkFrame/1.0; +https://inkframe.io)")
			req.Header.Set("Accept", "application/json")

			resp, err := httpClient.Do(req)
			if err != nil {
				fetchErr = err
				continue // retry on network error
			}
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
			resp.Body.Close()
			if resp.StatusCode >= 500 {
				fetchErr = fmt.Errorf("BBC SFX HTTP %d: %.200s", resp.StatusCode, b)
				continue // retry on server error
			}
			if resp.StatusCode != http.StatusOK {
				errMsg = fmt.Sprintf("BBC SFX HTTP %d: %.200s", resp.StatusCode, b)
				return // don't retry on 4xx (permanent error)
			}
			ct := resp.Header.Get("Content-Type")
			if ct != "" && !strings.HasPrefix(ct, "application/json") {
				errMsg = fmt.Sprintf("BBC SFX unexpected content-type %q: %.200s", ct, b)
				return
			}
			body = b
			fetchErr = nil
			break
		}
		if fetchErr != nil {
			errMsg = fetchErr.Error()
			return
		}

		var result struct {
			Count   int `json:"count"`
			Results []struct {
				ID          string  `json:"id"`
				Description string  `json:"description"`
				Duration    float64 `json:"duration"`
				Formats     struct {
					MP3 string `json:"mp3"`
				} `json:"formats"`
			} `json:"results"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			errMsg = fmt.Sprintf("parse error: %v — body: %.200s", err, body)
			return
		}

		if from == 0 {
			totalFound = result.Count
		}
		if len(result.Results) == 0 {
			break
		}

		for _, r := range result.Results {
			mp3 := r.Formats.MP3
			if mp3 == "" && r.ID != "" {
				mp3 = fmt.Sprintf("https://sound-effects-media.bbcrewind.co.uk/mp3/%s.mp3", r.ID)
			}
			if mp3 == "" {
				skipped++
				continue
			}

			externalID := "bbc-sfx:" + r.ID
			asset := &model.Asset{
				Scope:      model.AssetScopePublic,
				Title:      r.Description,
				Type:       "audio",
				SubType:    "sfx",
				Source:     "crawled",
				StorageURL: mp3,
				SourceURL:  fmt.Sprintf("https://sound-effects.bbcrewind.co.uk/#%s", r.ID),
				ExternalID: externalID,
				License:    "bbc-remarc",
				Duration:   r.Duration,
				Status:     model.AssetStatusActive,
			}
			created, err := s.crawlUpsert(externalID, func() error { return s.assetRepo.Create(asset) })
			if err != nil {
				failed++
			} else if !created {
				skipped++
			} else {
				imported++
			}
		}

		from += len(result.Results)
		if from >= result.Count || len(result.Results) < batchSize {
			break
		}
	}
	return
}

// crawlFreesound fetches audio assets from Freesound via the official API.
// Requires FREESOUND_API_KEY. Supports license filter: CC0 → "Creative Commons 0", CC-BY → "Attribution".
// API docs: https://freesound.org/docs/api/
func (s *AssetService) crawlFreesound(ctx context.Context, job *model.CrawlJob) (imported, skipped, failed, totalFound int, errMsg string) {
	if s.freesoundKey == "" {
		errMsg = "Freesound API key not configured (set FREESOUND_API_KEY)"
		return
	}
	limit := job.Limit
	if limit <= 0 {
		limit = 20
	}

	// Map job.License to Freesound filter value
	licenseFilter := ""
	switch strings.ToUpper(job.License) {
	case "CC0":
		licenseFilter = `license:"Creative Commons 0"`
	case "CC-BY":
		licenseFilter = `license:"Attribution"`
	}

	httpClient := buildCrawlHTTPClient(s.crawlProxyURL, 15*time.Second)
	pageSize := 20
	page := 1

	for imported+skipped+failed < limit {
		if err := ctx.Err(); err != nil {
			return
		}

		need := limit - (imported + skipped + failed)
		batchSize := pageSize
		if need < batchSize {
			batchSize = need
		}

		apiURL := fmt.Sprintf(
			"https://freesound.org/apiv2/search/text/?query=%s&fields=id,name,previews,duration,username,url,license&sort=score&page_size=%d&page=%d&token=%s",
			url.QueryEscape(job.Query), batchSize, page, s.freesoundKey,
		)
		if licenseFilter != "" {
			apiURL += "&filter=" + url.QueryEscape(licenseFilter)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
		if err != nil {
			errMsg = err.Error()
			return
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			errMsg = err.Error()
			return
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			errMsg = "Freesound: invalid API key (401)"
			return
		}
		if resp.StatusCode != http.StatusOK {
			errMsg = fmt.Sprintf("Freesound HTTP %d: %.200s", resp.StatusCode, b)
			return
		}

		var result struct {
			Count   int    `json:"count"`
			Next    string `json:"next"`
			Results []struct {
				ID       int     `json:"id"`
				Name     string  `json:"name"`
				Duration float64 `json:"duration"`
				Username string  `json:"username"`
				URL      string  `json:"url"`
				License  string  `json:"license"`
				Previews struct {
					HQ string `json:"preview-hq-mp3"`
					LQ string `json:"preview-lq-mp3"`
				} `json:"previews"`
			} `json:"results"`
		}
		if err := json.Unmarshal(b, &result); err != nil {
			errMsg = fmt.Sprintf("parse error: %v — body: %.200s", err, b)
			return
		}
		if page == 1 {
			totalFound = result.Count
		}
		if len(result.Results) == 0 {
			break
		}

		for _, r := range result.Results {
			mp3 := r.Previews.HQ
			if mp3 == "" {
				mp3 = r.Previews.LQ
			}
			if mp3 == "" {
				skipped++
				continue
			}

			externalID := fmt.Sprintf("freesound:%d", r.ID)

			// Normalize license string to short form
			license := r.License
			switch {
			case strings.Contains(license, "Creative Commons 0"):
				license = "CC0"
			case strings.Contains(license, "Attribution Noncommercial"):
				license = "CC-BY-NC"
			case strings.Contains(license, "Attribution"):
				license = "CC-BY"
			}

			asset := &model.Asset{
				Scope:      model.AssetScopePublic,
				Title:      r.Name,
				Type:       "audio",
				SubType:    "sfx",
				Source:     "crawled",
				StorageURL: mp3,
				SourceURL:  r.URL,
				ExternalID: externalID,
				License:    license,
				Duration:   r.Duration,
				Status:     model.AssetStatusActive,
			}
			created, err := s.crawlUpsert(externalID, func() error { return s.assetRepo.Create(asset) })
			if err != nil {
				failed++
			} else if !created {
				skipped++
			} else {
				imported++
			}
		}

		if result.Next == "" {
			break
		}
		page++
	}
	return
}

// crawlPixabay fetches audio assets from Pixabay via the official API.
// Requires PIXABAY_API_KEY. All Pixabay audio is free for commercial use (Pixabay License).
// API docs: https://pixabay.com/api/docs/
func (s *AssetService) crawlPixabay(ctx context.Context, job *model.CrawlJob) (imported, skipped, failed, totalFound int, errMsg string) {
	if s.pixabayKey == "" {
		errMsg = "Pixabay API key not configured (set PIXABAY_API_KEY)"
		return
	}
	limit := job.Limit
	if limit <= 0 {
		limit = 20
	}

	httpClient := buildCrawlHTTPClient(s.crawlProxyURL, 15*time.Second)
	pageSize := 20 // Pixabay max per_page = 200, but keep batches small
	page := 1

	for imported+skipped+failed < limit {
		if err := ctx.Err(); err != nil {
			return
		}

		need := limit - (imported + skipped + failed)
		batchSize := pageSize
		if need < batchSize {
			batchSize = need
		}

		apiURL := fmt.Sprintf(
			"https://pixabay.com/api/?key=%s&q=%s&media_type=music&per_page=%d&page=%d",
			s.pixabayKey, url.QueryEscape(job.Query), batchSize, page,
		)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
		if err != nil {
			errMsg = err.Error()
			return
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			errMsg = err.Error()
			return
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			errMsg = fmt.Sprintf("Pixabay: invalid API key (%d)", resp.StatusCode)
			return
		}
		if resp.StatusCode != http.StatusOK {
			errMsg = fmt.Sprintf("Pixabay HTTP %d: %.200s", resp.StatusCode, b)
			return
		}

		var result struct {
			Total     int `json:"total"`
			TotalHits int `json:"totalHits"`
			Hits      []struct {
				ID       int     `json:"id"`
				Tags     string  `json:"tags"`
				Audio    string  `json:"audio"`
				Duration float64 `json:"duration"`
				PageURL  string  `json:"pageURL"`
			} `json:"hits"`
		}
		if err := json.Unmarshal(b, &result); err != nil {
			errMsg = fmt.Sprintf("parse error: %v — body: %.200s", err, b)
			return
		}
		if page == 1 {
			totalFound = result.TotalHits
		}
		if len(result.Hits) == 0 {
			break
		}

		for _, h := range result.Hits {
			if h.Audio == "" {
				skipped++
				continue
			}

			externalID := fmt.Sprintf("pixabay:%d", h.ID)

			// Use first tag as title, fall back to "Pixabay audio {id}"
			title := strings.SplitN(h.Tags, ",", 2)[0]
			title = strings.TrimSpace(title)
			if title == "" {
				title = fmt.Sprintf("Pixabay audio %d", h.ID)
			}

			asset := &model.Asset{
				Scope:      model.AssetScopePublic,
				Title:      title,
				Type:       "audio",
				SubType:    "sfx",
				Source:     "crawled",
				StorageURL: h.Audio,
				SourceURL:  h.PageURL,
				ExternalID: externalID,
				License:    "pixabay",
				Duration:   h.Duration,
				Status:     model.AssetStatusActive,
			}
			created, err := s.crawlUpsert(externalID, func() error { return s.assetRepo.Create(asset) })
			if err != nil {
				failed++
			} else if !created {
				skipped++
			} else {
				imported++
			}
		}

		if imported+skipped+failed >= limit || len(result.Hits) < batchSize {
			break
		}
		page++
	}
	return
}

// crawlWikimedia fetches assets from Wikimedia Commons via the public MediaWiki API.
// API docs: https://www.mediawiki.org/wiki/API:Search
// No API key required; User-Agent identification is mandatory per Wikimedia policy.
func (s *AssetService) crawlWikimedia(ctx context.Context, job *model.CrawlJob) (imported, skipped, failed, totalFound int, errMsg string) {
	limit := job.Limit
	if limit <= 0 {
		limit = 20
	}

	httpClient := buildCrawlHTTPClient(s.crawlProxyURL, 15*time.Second)
	pageSize := 20
	offset := 0

	for imported+skipped+failed < limit {
		if err := ctx.Err(); err != nil {
			return
		}

		need := limit - (imported + skipped + failed)
		batchSize := pageSize
		if need < batchSize {
			batchSize = need
		}

		apiURL := fmt.Sprintf(
			"https://commons.wikimedia.org/w/api.php?action=query&generator=search&gsrsearch=%s&gsrnamespace=6&gsrlimit=%d&gsroffset=%d&prop=imageinfo&iiprop=url%%7Cmime%%7Cextmetadata&format=json&formatversion=2",
			url.QueryEscape(job.Query), batchSize, offset,
		)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
		if err != nil {
			errMsg = err.Error()
			return
		}
		req.Header.Set("User-Agent", "InkFrame/1.0 (https://inkframe.io; contact@inkframe.io) Go-http-client")
		req.Header.Set("Accept", "application/json")

		resp, err := httpClient.Do(req)
		if err != nil {
			errMsg = err.Error()
			return
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			errMsg = fmt.Sprintf("Wikimedia HTTP %d: %.200s", resp.StatusCode, b)
			return
		}

		var result struct {
			Continue *struct {
				GsrOffset int `json:"gsroffset"`
			} `json:"continue"`
			Query *struct {
				Pages []struct {
					PageID    int    `json:"pageid"`
					Title     string `json:"title"`
					ImageInfo []struct {
						URL  string `json:"url"`
						Mime string `json:"mime"`
						Extmetadata *struct {
							LicenseShortName *struct{ Value string `json:"value"` } `json:"LicenseShortName"`
						} `json:"extmetadata"`
					} `json:"imageinfo"`
				} `json:"pages"`
			} `json:"query"`
		}
		if err := json.Unmarshal(b, &result); err != nil {
			errMsg = fmt.Sprintf("parse error: %v — body: %.200s", err, b)
			return
		}
		if result.Query == nil || len(result.Query.Pages) == 0 {
			break
		}

		// Rough total estimate on first page
		if totalFound == 0 {
			totalFound = offset + len(result.Query.Pages)
			if result.Continue != nil {
				totalFound += 50 // there are more pages
			}
		}

		for _, page := range result.Query.Pages {
			if len(page.ImageInfo) == 0 {
				skipped++
				continue
			}
			info := page.ImageInfo[0]
			if info.URL == "" {
				skipped++
				continue
			}

			// Determine asset type from MIME
			assetType := "image"
			if strings.HasPrefix(info.Mime, "video/") {
				assetType = "video"
			} else if strings.HasPrefix(info.Mime, "audio/") {
				assetType = "audio"
			} else if !strings.HasPrefix(info.Mime, "image/") {
				skipped++ // skip SVG documents, PDFs, etc. that aren't plain images
				continue
			}
			if job.AssetType != "" && job.AssetType != assetType {
				skipped++
				continue
			}

			// Extract license
			license := ""
			if info.Extmetadata != nil && info.Extmetadata.LicenseShortName != nil {
				license = info.Extmetadata.LicenseShortName.Value
			}
			if job.License != "" && !strings.EqualFold(license, job.License) {
				skipped++
				continue
			}

			externalID := fmt.Sprintf("wikimedia:%d", page.PageID)

			// Clean title: strip "File:" prefix, extension, replace underscores
			title := strings.TrimPrefix(page.Title, "File:")
			if idx := strings.LastIndex(title, "."); idx > 0 {
				title = title[:idx]
			}
			title = strings.ReplaceAll(title, "_", " ")

			asset := &model.Asset{
				Scope:      model.AssetScopePublic,
				Title:      title,
				Type:       assetType,
				Source:     "crawled",
				StorageURL: info.URL,
				SourceURL:  fmt.Sprintf("https://commons.wikimedia.org/wiki/%s", url.PathEscape(page.Title)),
				ExternalID: externalID,
				License:    license,
				Status:     model.AssetStatusActive,
			}
			created, err := s.crawlUpsert(externalID, func() error { return s.assetRepo.Create(asset) })
			if err != nil {
				failed++
			} else if !created {
				skipped++
			} else {
				imported++
			}
		}

		if result.Continue == nil {
			break
		}
		offset = result.Continue.GsrOffset
	}
	return
}

// crawlUnsplash fetches images from Unsplash via the official API.
// Requires an Unsplash Access Key (set UNSPLASH_ACCESS_KEY env var).
// API docs: https://unsplash.com/documentation#search-photos
// Unsplash License: free for commercial/non-commercial use, attribution appreciated.
func (s *AssetService) crawlUnsplash(ctx context.Context, job *model.CrawlJob) (imported, skipped, failed, totalFound int, errMsg string) {
	if s.unsplashKey == "" {
		errMsg = "Unsplash access key not configured (set UNSPLASH_ACCESS_KEY)"
		return
	}
	limit := job.Limit
	if limit <= 0 {
		limit = 20
	}

	httpClient := buildCrawlHTTPClient(s.crawlProxyURL, 15*time.Second)
	pageSize := 30 // Unsplash max per_page = 30
	page := 1

	for imported+skipped+failed < limit {
		if err := ctx.Err(); err != nil {
			return
		}

		need := limit - (imported + skipped + failed)
		batchSize := pageSize
		if need < batchSize {
			batchSize = need
		}

		apiURL := fmt.Sprintf(
			"https://api.unsplash.com/search/photos?query=%s&per_page=%d&page=%d",
			url.QueryEscape(job.Query), batchSize, page,
		)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
		if err != nil {
			errMsg = err.Error()
			return
		}
		req.Header.Set("Authorization", "Client-ID "+s.unsplashKey)
		req.Header.Set("Accept-Version", "v1")

		resp, err := httpClient.Do(req)
		if err != nil {
			errMsg = err.Error()
			return
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			errMsg = "Unsplash: invalid access key (401)"
			return
		}
		if resp.StatusCode == 429 {
			errMsg = "Unsplash: rate limit exceeded (50 requests/hour on free plan)"
			return
		}
		if resp.StatusCode != http.StatusOK {
			errMsg = fmt.Sprintf("Unsplash HTTP %d: %.200s", resp.StatusCode, b)
			return
		}

		var result struct {
			Total      int `json:"total"`
			TotalPages int `json:"total_pages"`
			Results    []struct {
				ID              string `json:"id"`
				Description     string `json:"description"`
				AltDescription  string `json:"alt_description"`
				Width           int    `json:"width"`
				Height          int    `json:"height"`
				Urls            struct {
					Raw     string `json:"raw"`
					Full    string `json:"full"`
					Regular string `json:"regular"`
				} `json:"urls"`
				Links struct {
					HTML             string `json:"html"`
					DownloadLocation string `json:"download_location"`
				} `json:"links"`
				User struct {
					Name     string `json:"name"`
					Username string `json:"username"`
				} `json:"user"`
			} `json:"results"`
		}
		if err := json.Unmarshal(b, &result); err != nil {
			errMsg = fmt.Sprintf("parse error: %v — body: %.200s", err, b)
			return
		}
		if page == 1 {
			totalFound = result.Total
		}
		if len(result.Results) == 0 {
			break
		}

		for _, photo := range result.Results {
			imgURL := photo.Urls.Regular
			if imgURL == "" {
				imgURL = photo.Urls.Full
			}
			if imgURL == "" {
				skipped++
				continue
			}

			externalID := "unsplash:" + photo.ID

			title := photo.Description
			if title == "" {
				title = photo.AltDescription
			}
			if title == "" {
				title = "Unsplash photo by " + photo.User.Name
			}

			asset := &model.Asset{
				Scope:      model.AssetScopePublic,
				Title:      title,
				Type:       "image",
				Source:     "crawled",
				StorageURL: imgURL,
				SourceURL:  photo.Links.HTML,
				ExternalID: externalID,
				License:    "unsplash",
				Status:     model.AssetStatusActive,
			}
			created, err := s.crawlUpsert(externalID, func() error { return s.assetRepo.Create(asset) })
			if err != nil {
				failed++
				continue
			}
			if !created {
				skipped++
				continue
			}
			imported++

			// Unsplash API terms: trigger download tracking after each import
			if photo.Links.DownloadLocation != "" {
				go func(dlURL string) {
					dlReq, _ := http.NewRequest(http.MethodGet, dlURL, nil)
					if dlReq != nil {
						dlReq.Header.Set("Authorization", "Client-ID "+s.unsplashKey)
						dlReq.Header.Set("Accept-Version", "v1")
						resp, _ := httpClient.Do(dlReq)
						if resp != nil {
							resp.Body.Close()
						}
					}
				}(photo.Links.DownloadLocation)
			}
		}

		if page >= result.TotalPages {
			break
		}
		page++
	}
	return
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
		var prompt string
		prompt, err = renderPrompt("asset_auto_tag", nil)
		if err == nil {
			rawJSON, err = s.aiSvc.GenerateWithVision(prompt, []string{asset.StorageURL})
		}
	} else {
		// Non-image: text-based tag generation from title + type
		var prompt string
		prompt, err = renderPrompt("asset_text_tag", map[string]interface{}{
			"Type":    asset.Type,
			"Title":   asset.Title,
			"SubType": asset.SubType,
		})
		if err == nil {
			rawJSON, err = s.aiSvc.Generate(0, "asset_tag", prompt)
		}
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
