package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/inkframe/inkframe-backend/internal/storage"
	"github.com/redis/go-redis/v9"
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
	pexelsKey     string
	// crawlMu guards the ExistsByExternalID+Create sequence for crawled assets (local fallback)
	crawlMu         sync.Map
	cache           *redis.Client // optional: cross-instance crawl dedup lock
	onDeleteSFXHook func(tag string)      // fired when an SFX audio asset is deleted
	onDeleteBGMHook func(filename string) // fired when a BGM audio asset is deleted
}

// OnDeleteSFX registers a callback fired when an SFX audio asset is soft- or hard-deleted.
// The callback receives the asset title (tag name) for cache invalidation.
func (s *AssetService) OnDeleteSFX(fn func(tag string)) {
	s.onDeleteSFXHook = fn
}

// OnDeleteBGM registers a callback fired when a BGM audio asset is soft- or hard-deleted.
// The callback receives the filename extracted from the asset's URL for cache invalidation.
func (s *AssetService) OnDeleteBGM(fn func(filename string)) {
	s.onDeleteBGMHook = fn
}

func (s *AssetService) fireDeleteHooks(a *model.Asset) {
	if a.Type == "audio" {
		switch a.SubType {
		case "sfx":
			if s.onDeleteSFXHook != nil {
				s.onDeleteSFXHook(a.Title)
			}
		case "bgm":
			if s.onDeleteBGMHook != nil {
				// Derive filename from the OSS URL path (e.g. ".../bgm/local/happy.mp3" → "happy.mp3")
				filename := path.Base(a.StorageURL)
				if filename != "" && filename != "." {
					s.onDeleteBGMHook(filename)
				}
			}
		}
	}
}

// WithRedis injects a Redis client for cross-instance crawl deduplication.
func (s *AssetService) WithRedis(c *redis.Client) *AssetService {
	s.cache = c
	return s
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
// Redis SETNX provides cross-instance mutual exclusion; local sync.Map is the fallback.
func (s *AssetService) crawlUpsert(externalID string, create func() error) (bool, error) {
	if s.cache != nil {
		// Use a short hash so the lock key stays within Redis key size limits.
		h := sha256.Sum256([]byte(externalID))
		redisKey := "lock:asset:crawl:" + hex.EncodeToString(h[:16])
		ok, err := s.cache.SetNX(context.Background(), redisKey, "1", 30*time.Second).Result()
		if err == nil {
			if !ok {
				return false, nil // another instance is already creating this asset
			}
			defer s.cache.Del(context.Background(), redisKey) //nolint:errcheck
			exists, err := s.assetRepo.ExistsByExternalID(externalID)
			if err != nil {
				return false, err
			}
			if exists {
				return false, nil
			}
			return true, create()
		}
		// Redis unavailable — fall through to local mutex
	}
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

func (s *AssetService) WithPexelsKey(key string) *AssetService {
	s.pexelsKey = key
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
	if s.storageSvc == nil {
		return nil, fmt.Errorf("storage service not configured")
	}

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
	if err := s.assetRepo.SoftDelete(id, callerID); err != nil {
		return err
	}
	s.fireDeleteHooks(a)
	return nil
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
	if err := s.assetRepo.HardDelete(id); err != nil {
		return err
	}
	s.fireDeleteHooks(a)
	return nil
}

func (s *AssetService) ListTrash(creatorID uint, page, size int) ([]*model.Asset, int64, error) {
	return s.assetRepo.ListTrash(creatorID, page, size)
}

// ClearPersonalAssets soft-deletes all active personal-scope assets for the caller.
func (s *AssetService) ClearPersonalAssets(callerID uint) (int64, error) {
	return s.assetRepo.SoftDeleteAllByCreator(callerID)
}

// EmptyTrash permanently purges all trashed assets for the caller.
func (s *AssetService) EmptyTrash(callerID uint) (int64, error) {
	return s.assetRepo.HardDeleteAllTrash(callerID)
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
	if s.storageSvc == nil {
		return nil, fmt.Errorf("storage service not configured")
	}
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
	v := &model.AssetVersion{
		AssetID:    assetID,
		StorageURL: url, FileSize: size,
		ChangeNote: note, CreatedBy: callerID,
	}
	if err := s.versionRepo.CreateVersionAtomic(v); err != nil {
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
	if (opts.AssetID == nil) == (opts.CollectionID == nil) {
		return nil, errors.New("exactly one of asset_id or collection_id must be set")
	}
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

func (s *AssetService) CreateCrawlJob(tenantID uint, source, query, assetType, license string, limit, crawlDepth int, urlPattern string, createdBy uint) (*model.CrawlJob, error) {
	job := &model.CrawlJob{
		TenantID: tenantID, Source: source, Query: query, AssetType: assetType,
		License: license, Limit: limit, CrawlDepth: crawlDepth, URLPattern: urlPattern,
		CreatedBy: createdBy, Status: "pending",
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
	case "pexels":
		imported, skipped, failed, totalFound, errMsg = s.crawlPexels(ctx, job)
	case "nasa":
		imported, skipped, failed, totalFound, errMsg = s.crawlNASA(ctx, job)
	case "webpage":
		imported, skipped, failed, totalFound, errMsg = s.crawlWebpage(ctx, job)
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

// crawlPixabay fetches image, video, or audio assets from Pixabay via the official API.
// Requires PIXABAY_API_KEY. All Pixabay content is free for commercial use (Pixabay License).
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

		var apiURL string
		switch job.AssetType {
		case "video":
			apiURL = fmt.Sprintf(
				"https://pixabay.com/api/videos/?key=%s&q=%s&per_page=%d&page=%d",
				s.pixabayKey, url.QueryEscape(job.Query), batchSize, page,
			)
		case "audio":
			apiURL = fmt.Sprintf(
				"https://pixabay.com/api/?key=%s&q=%s&media_type=music&per_page=%d&page=%d",
				s.pixabayKey, url.QueryEscape(job.Query), batchSize, page,
			)
		default: // image
			apiURL = fmt.Sprintf(
				"https://pixabay.com/api/?key=%s&q=%s&image_type=photo&per_page=%d&page=%d",
				s.pixabayKey, url.QueryEscape(job.Query), batchSize, page,
			)
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
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			errMsg = fmt.Sprintf("Pixabay: invalid API key (%d)", resp.StatusCode)
			return
		}
		if resp.StatusCode != http.StatusOK {
			errMsg = fmt.Sprintf("Pixabay HTTP %d: %.200s", resp.StatusCode, b)
			return
		}

		switch job.AssetType {
		case "video":
			var result struct {
				Total     int `json:"total"`
				TotalHits int `json:"totalHits"`
				Hits      []struct {
					ID       int     `json:"id"`
					Tags     string  `json:"tags"`
					PageURL  string  `json:"pageURL"`
					Duration int     `json:"duration"`
					Videos   struct {
						Large  struct{ URL string `json:"url"`; Width int `json:"width"`; Height int `json:"height"` } `json:"large"`
						Medium struct{ URL string `json:"url"`; Width int `json:"width"`; Height int `json:"height"` } `json:"medium"`
						Small  struct{ URL string `json:"url"`; Width int `json:"width"`; Height int `json:"height"` } `json:"small"`
					} `json:"videos"`
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
				videoURL := h.Videos.Large.URL
				w, ht := h.Videos.Large.Width, h.Videos.Large.Height
				if videoURL == "" {
					videoURL = h.Videos.Medium.URL
					w, ht = h.Videos.Medium.Width, h.Videos.Medium.Height
				}
				if videoURL == "" {
					skipped++
					continue
				}
				externalID := fmt.Sprintf("pixabay-video:%d", h.ID)
				title := strings.TrimSpace(strings.SplitN(h.Tags, ",", 2)[0])
				if title == "" {
					title = fmt.Sprintf("Pixabay video %d", h.ID)
				}
				asset := &model.Asset{
					Scope:       model.AssetScopePublic,
					Title:       title,
					Type:        "video",
					Source:      "crawled",
					StorageURL:  videoURL,
					SourceURL:   h.PageURL,
					ExternalID:  externalID,
					License:     "pixabay",
					Duration:    float64(h.Duration),
					Width:       w,
					Height:      ht,
					AspectRatio: calcAspectRatio(w, ht),
					Status:      model.AssetStatusActive,
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

		case "audio":
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
				externalID := fmt.Sprintf("pixabay-audio:%d", h.ID)
				title := strings.TrimSpace(strings.SplitN(h.Tags, ",", 2)[0])
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

		default: // image
			var result struct {
				Total     int `json:"total"`
				TotalHits int `json:"totalHits"`
				Hits      []struct {
					ID             int    `json:"id"`
					Tags           string `json:"tags"`
					PageURL        string `json:"pageURL"`
					PreviewURL     string `json:"previewURL"`
					WebformatURL   string `json:"webformatURL"`
					LargeImageURL  string `json:"largeImageURL"`
					ImageWidth     int    `json:"imageWidth"`
					ImageHeight    int    `json:"imageHeight"`
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
				imgURL := h.LargeImageURL
				if imgURL == "" {
					imgURL = h.WebformatURL
				}
				if imgURL == "" {
					skipped++
					continue
				}
				externalID := fmt.Sprintf("pixabay:%d", h.ID)
				title := strings.TrimSpace(strings.SplitN(h.Tags, ",", 2)[0])
				if title == "" {
					title = fmt.Sprintf("Pixabay image %d", h.ID)
				}
				asset := &model.Asset{
					Scope:        model.AssetScopePublic,
					Title:        title,
					Type:         "image",
					Source:       "crawled",
					StorageURL:   imgURL,
					ThumbnailURL: h.PreviewURL,
					SourceURL:    h.PageURL,
					ExternalID:   externalID,
					License:      "pixabay",
					Width:        h.ImageWidth,
					Height:       h.ImageHeight,
					AspectRatio:  calcAspectRatio(h.ImageWidth, h.ImageHeight),
					Status:       model.AssetStatusActive,
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
		}

		if imported+skipped+failed >= limit {
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
			"https://commons.wikimedia.org/w/api.php?action=query&generator=search&gsrsearch=%s&gsrnamespace=6&gsrlimit=%d&gsroffset=%d&prop=imageinfo&iiprop=url%%7Cmime%%7Cextmetadata%%7Cdimensions%%7Cthumburl&iiurlwidth=400&format=json&formatversion=2",
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
						URL      string `json:"url"`
						ThumbURL string `json:"thumburl"`
						Mime     string `json:"mime"`
						Width    int    `json:"width"`
						Height   int    `json:"height"`
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
				Scope:        model.AssetScopePublic,
				Title:        title,
				Type:         assetType,
				Source:       "crawled",
				StorageURL:   info.URL,
				ThumbnailURL: info.ThumbURL,
				SourceURL:    fmt.Sprintf("https://commons.wikimedia.org/wiki/%s", url.PathEscape(page.Title)),
				ExternalID:   externalID,
				License:      license,
				Width:        info.Width,
				Height:       info.Height,
				AspectRatio:  calcAspectRatio(info.Width, info.Height),
				Status:       model.AssetStatusActive,
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

// calcAspectRatio returns a simplified aspect ratio string like "16:9", "4:3", or "1:1".
func calcAspectRatio(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	gcd := func(a, b int) int {
		for b != 0 {
			a, b = b, a%b
		}
		return a
	}
	d := gcd(w, h)
	return fmt.Sprintf("%d:%d", w/d, h/d)
}

// crawlPexels fetches image or video assets from Pexels via the official API.
// Requires PEXELS_API_KEY. All Pexels content is free for commercial use (Pexels License).
// API docs: https://www.pexels.com/api/documentation/
func (s *AssetService) crawlPexels(ctx context.Context, job *model.CrawlJob) (imported, skipped, failed, totalFound int, errMsg string) {
	if s.pexelsKey == "" {
		errMsg = "Pexels API key not configured (set PEXELS_API_KEY)"
		return
	}
	limit := job.Limit
	if limit <= 0 {
		limit = 20
	}

	httpClient := buildCrawlHTTPClient(s.crawlProxyURL, 15*time.Second)
	pageSize := 20
	page := 1
	isVideo := job.AssetType == "video"

	for imported+skipped+failed < limit {
		if err := ctx.Err(); err != nil {
			return
		}

		need := limit - (imported + skipped + failed)
		batchSize := pageSize
		if need < batchSize {
			batchSize = need
		}

		var apiURL string
		if isVideo {
			apiURL = fmt.Sprintf(
				"https://api.pexels.com/videos/search?query=%s&per_page=%d&page=%d",
				url.QueryEscape(job.Query), batchSize, page,
			)
		} else {
			apiURL = fmt.Sprintf(
				"https://api.pexels.com/v1/search?query=%s&per_page=%d&page=%d",
				url.QueryEscape(job.Query), batchSize, page,
			)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
		if err != nil {
			errMsg = err.Error()
			return
		}
		req.Header.Set("Authorization", s.pexelsKey)

		resp, err := httpClient.Do(req)
		if err != nil {
			errMsg = err.Error()
			return
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			errMsg = "Pexels: invalid API key (401)"
			return
		}
		if resp.StatusCode == 429 {
			errMsg = "Pexels: rate limit exceeded"
			return
		}
		if resp.StatusCode != http.StatusOK {
			errMsg = fmt.Sprintf("Pexels HTTP %d: %.200s", resp.StatusCode, b)
			return
		}

		if isVideo {
			var result struct {
				TotalResults int `json:"total_results"`
				Videos       []struct {
					ID     int    `json:"id"`
					Width  int    `json:"width"`
					Height int    `json:"height"`
					URL    string `json:"url"`
					Image  string `json:"image"`
					User   struct {
						Name string `json:"name"`
					} `json:"user"`
					VideoFiles []struct {
						Quality  string `json:"quality"`
						FileType string `json:"file_type"`
						Width    int    `json:"width"`
						Height   int    `json:"height"`
						Link     string `json:"link"`
					} `json:"video_files"`
				} `json:"videos"`
			}
			if err := json.Unmarshal(b, &result); err != nil {
				errMsg = fmt.Sprintf("parse error: %v — body: %.200s", err, b)
				return
			}
			if page == 1 {
				totalFound = result.TotalResults
			}
			if len(result.Videos) == 0 {
				break
			}
			for _, v := range result.Videos {
				var bestURL string
				var bw, bh int
				for _, f := range v.VideoFiles {
					if strings.EqualFold(f.Quality, "hd") {
						bestURL, bw, bh = f.Link, f.Width, f.Height
						break
					}
				}
				if bestURL == "" {
					for _, f := range v.VideoFiles {
						if strings.EqualFold(f.Quality, "sd") {
							bestURL, bw, bh = f.Link, f.Width, f.Height
							break
						}
					}
				}
				if bestURL == "" && len(v.VideoFiles) > 0 {
					bestURL = v.VideoFiles[0].Link
					bw, bh = v.VideoFiles[0].Width, v.VideoFiles[0].Height
				}
				if bestURL == "" {
					skipped++
					continue
				}
				externalID := fmt.Sprintf("pexels-video:%d", v.ID)
				asset := &model.Asset{
					Scope:        model.AssetScopePublic,
					Title:        fmt.Sprintf("Pexels video by %s", v.User.Name),
					Type:         "video",
					Source:       "crawled",
					StorageURL:   bestURL,
					ThumbnailURL: v.Image,
					SourceURL:    v.URL,
					ExternalID:   externalID,
					License:      "pexels",
					Attribution:  fmt.Sprintf("Video by %s on Pexels", v.User.Name),
					Width:        bw,
					Height:       bh,
					AspectRatio:  calcAspectRatio(bw, bh),
					Status:       model.AssetStatusActive,
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
		} else {
			var result struct {
				TotalResults int `json:"total_results"`
				TotalPages   int `json:"total_pages"`
				Photos       []struct {
					ID           int    `json:"id"`
					Width        int    `json:"width"`
					Height       int    `json:"height"`
					URL          string `json:"url"`
					Alt          string `json:"alt"`
					Photographer string `json:"photographer"`
					Src          struct {
						Large2x string `json:"large2x"`
						Large   string `json:"large"`
						Medium  string `json:"medium"`
					} `json:"src"`
				} `json:"photos"`
			}
			if err := json.Unmarshal(b, &result); err != nil {
				errMsg = fmt.Sprintf("parse error: %v — body: %.200s", err, b)
				return
			}
			if page == 1 {
				totalFound = result.TotalResults
			}
			if len(result.Photos) == 0 {
				break
			}
			for _, p := range result.Photos {
				imgURL := p.Src.Large2x
				if imgURL == "" {
					imgURL = p.Src.Large
				}
				if imgURL == "" {
					skipped++
					continue
				}
				externalID := fmt.Sprintf("pexels:%d", p.ID)
				title := p.Alt
				if title == "" {
					title = fmt.Sprintf("Pexels photo by %s", p.Photographer)
				}
				asset := &model.Asset{
					Scope:        model.AssetScopePublic,
					Title:        title,
					Type:         "image",
					Source:       "crawled",
					StorageURL:   imgURL,
					ThumbnailURL: p.Src.Medium,
					SourceURL:    p.URL,
					ExternalID:   externalID,
					License:      "pexels",
					Attribution:  fmt.Sprintf("Photo by %s on Pexels", p.Photographer),
					Width:        p.Width,
					Height:       p.Height,
					AspectRatio:  calcAspectRatio(p.Width, p.Height),
					Status:       model.AssetStatusActive,
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
			if page >= result.TotalPages {
				break
			}
		}
		page++
	}
	return
}

// crawlNASA fetches image and video assets from the NASA Image and Video Library.
// No API key required. All NASA content is in the public domain (US Government works).
// API docs: https://images.nasa.gov/docs/images.nasa.gov_api_docs.pdf
func (s *AssetService) crawlNASA(ctx context.Context, job *model.CrawlJob) (imported, skipped, failed, totalFound int, errMsg string) {
	limit := job.Limit
	if limit <= 0 {
		limit = 20
	}

	mediaType := job.AssetType
	if mediaType == "" || mediaType == "audio" {
		mediaType = "image"
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
			"https://images-api.nasa.gov/search?q=%s&media_type=%s&page_size=%d&page=%d",
			url.QueryEscape(job.Query), mediaType, batchSize, page,
		)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
		if err != nil {
			errMsg = err.Error()
			return
		}
		req.Header.Set("User-Agent", "InkFrame/1.0 (https://inkframe.io; contact@inkframe.io) Go-http-client")

		resp, err := httpClient.Do(req)
		if err != nil {
			errMsg = err.Error()
			return
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
		resp.Body.Close()
		if resp.StatusCode == 404 {
			// NASA API returns 404 for empty result pages
			break
		}
		if resp.StatusCode != http.StatusOK {
			errMsg = fmt.Sprintf("NASA HTTP %d: %.200s", resp.StatusCode, b)
			return
		}

		var result struct {
			Collection struct {
				Metadata struct {
					TotalHits int `json:"total_hits"`
				} `json:"metadata"`
				Items []struct {
					Data []struct {
						NasaID      string `json:"nasa_id"`
						Title       string `json:"title"`
						Description string `json:"description"`
						MediaType   string `json:"media_type"`
						DateCreated string `json:"date_created"`
						Center      string `json:"center"`
					} `json:"data"`
					Links []struct {
						Href   string `json:"href"`
						Rel    string `json:"rel"`
						Render string `json:"render"`
					} `json:"links"`
				} `json:"items"`
			} `json:"collection"`
		}
		if err := json.Unmarshal(b, &result); err != nil {
			errMsg = fmt.Sprintf("parse error: %v — body: %.200s", err, b)
			return
		}
		if page == 1 {
			totalFound = result.Collection.Metadata.TotalHits
		}
		if len(result.Collection.Items) == 0 {
			break
		}

		for _, item := range result.Collection.Items {
			if len(item.Data) == 0 {
				skipped++
				continue
			}
			d := item.Data[0]
			if d.NasaID == "" {
				skipped++
				continue
			}

			// Find the preview link (thumbnail/preview image)
			var thumbURL string
			for _, lnk := range item.Links {
				if lnk.Rel == "preview" {
					thumbURL = lnk.Href
					break
				}
			}
			if thumbURL == "" && len(item.Links) > 0 {
				thumbURL = item.Links[0].Href
			}
			if thumbURL == "" {
				skipped++
				continue
			}

			// For images use the ~orig or ~large URL; derive from thumb URL by replacing ~thumb suffix
			storageURL := thumbURL
			if strings.Contains(thumbURL, "~thumb.") {
				storageURL = strings.Replace(thumbURL, "~thumb.", "~orig.", 1)
			} else if strings.Contains(thumbURL, "~small.") {
				storageURL = strings.Replace(thumbURL, "~small.", "~orig.", 1)
			}

			externalID := "nasa:" + d.NasaID

			assetType := d.MediaType
			if assetType == "" {
				assetType = mediaType
			}
			if assetType != "image" && assetType != "video" {
				skipped++
				continue
			}

			asset := &model.Asset{
				Scope:        model.AssetScopePublic,
				Title:        d.Title,
				Description:  d.Description,
				Type:         assetType,
				Source:       "crawled",
				StorageURL:   storageURL,
				ThumbnailURL: thumbURL,
				SourceURL:    fmt.Sprintf("https://images.nasa.gov/details/%s", url.PathEscape(d.NasaID)),
				ExternalID:   externalID,
				License:      "PD",
				Attribution:  fmt.Sprintf("NASA/%s", d.Center),
				Status:       model.AssetStatusActive,
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

		if imported+skipped+failed >= limit {
			break
		}
		page++
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
				ID             string `json:"id"`
				Description    string `json:"description"`
				AltDescription string `json:"alt_description"`
				Width          int    `json:"width"`
				Height         int    `json:"height"`
				Urls           struct {
					Raw     string `json:"raw"`
					Full    string `json:"full"`
					Regular string `json:"regular"`
					Small   string `json:"small"`
					Thumb   string `json:"thumb"`
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

			thumbURL := photo.Urls.Small
			if thumbURL == "" {
				thumbURL = photo.Urls.Thumb
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
				Scope:        model.AssetScopePublic,
				Title:        title,
				Type:         "image",
				Source:       "crawled",
				StorageURL:   imgURL,
				ThumbnailURL: thumbURL,
				SourceURL:    photo.Links.HTML,
				ExternalID:   externalID,
				License:      "unsplash",
				Attribution:  fmt.Sprintf("Photo by %s (@%s) on Unsplash", photo.User.Name, photo.User.Username),
				Width:        photo.Width,
				Height:       photo.Height,
				AspectRatio:  calcAspectRatio(photo.Width, photo.Height),
				Status:       model.AssetStatusActive,
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

// ─── Webpage Crawl ────────────────────────────────────────────────────────────

// crawlWebpage fetches a web page (or a small site tree) and imports linked media assets.
// job.Query = starting URL
// job.CrawlDepth = 0: only the given page; 1: also follow same-domain links on that page
// job.URLPattern = optional regex filter for links to follow (overrides same-domain check)
// job.AssetType  = "image"|"video"|"audio"|"" (empty = all)
func (s *AssetService) crawlWebpage(ctx context.Context, job *model.CrawlJob) (imported, skipped, failed, totalFound int, errMsg string) {
	startURL := strings.TrimSpace(job.Query)
	if startURL == "" {
		errMsg = "URL is required for webpage crawl (set query to the target URL)"
		return
	}
	parsedStart, err := url.Parse(startURL)
	if err != nil || parsedStart.Host == "" {
		errMsg = "invalid URL: " + startURL
		return
	}

	limit := job.Limit
	if limit <= 0 {
		limit = 50
	}

	var urlRe *regexp.Regexp
	if job.URLPattern != "" {
		urlRe, err = regexp.Compile(job.URLPattern)
		if err != nil {
			errMsg = "invalid url_pattern regexp: " + err.Error()
			return
		}
	}

	httpClient := buildCrawlHTTPClient(s.crawlProxyURL, 20*time.Second)

	visited := make(map[string]bool)
	toVisit := []string{startURL}
	const maxPages = 30 // hard cap on pages crawled per job

	for len(toVisit) > 0 && (imported+skipped+failed) < limit && len(visited) < maxPages {
		if ctx.Err() != nil {
			return
		}

		pageURL := toVisit[0]
		toVisit = toVisit[1:]
		if visited[pageURL] {
			continue
		}
		visited[pageURL] = true

		assets, links, extractErr := s.extractPageAssets(ctx, httpClient, pageURL, job.AssetType)
		if extractErr != nil {
			// non-fatal: log and continue to next page
			continue
		}

		totalFound += len(assets)

		for _, a := range assets {
			if (imported + skipped + failed) >= limit {
				break
			}
			eid := a.ExternalID
			created, createErr := s.crawlUpsert(eid, func() error { return s.assetRepo.Create(a) })
			if createErr != nil {
				failed++
			} else if !created {
				skipped++
			} else {
				imported++
			}
		}

		// Follow links only when depth > 0
		if job.CrawlDepth > 0 {
			for _, lnk := range links {
				if visited[lnk] {
					continue
				}
				lParsed, parseErr := url.Parse(lnk)
				if parseErr != nil {
					continue
				}
				if urlRe != nil {
					if !urlRe.MatchString(lnk) {
						continue
					}
				} else if lParsed.Host != parsedStart.Host {
					continue
				}
				toVisit = append(toVisit, lnk)
			}
		}
	}
	return
}

// extractPageAssets fetches a single HTML page and returns discovered media assets and
// navigable links (for depth-following).
func (s *AssetService) extractPageAssets(ctx context.Context, client *http.Client, pageURL, assetTypeFilter string) ([]*model.Asset, []string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; InkFrame/1.0; +https://inkframe.io)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	doc, err := goquery.NewDocumentFromReader(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		return nil, nil, err
	}

	base, _ := url.Parse(pageURL)
	pageTitle := strings.TrimSpace(doc.Find("title").First().Text())

	seen := make(map[string]bool) // per-page dedup
	var assets []*model.Asset

	addAsset := func(rawURL, assetType, title string) {
		abs := webpageResolveURL(base, rawURL)
		if abs == "" || seen[abs] {
			return
		}
		if assetTypeFilter != "" && assetTypeFilter != assetType {
			return
		}
		seen[abs] = true

		if title == "" {
			if p, parseErr := url.Parse(abs); parseErr == nil {
				title = path.Base(p.Path)
				if title == "." || title == "/" || title == "" {
					title = pageTitle
				}
			}
		}

		// externalID = "webpage:" + first 24 hex chars of sha256(URL)
		h := sha256.Sum256([]byte(abs))
		externalID := "webpage:" + hex.EncodeToString(h[:12])

		assets = append(assets, &model.Asset{
			Scope:      model.AssetScopePublic,
			Title:      title,
			Type:       assetType,
			Source:     "crawled",
			StorageURL: abs,
			SourceURL:  pageURL,
			ExternalID: externalID,
			License:    "unknown",
			Status:     model.AssetStatusActive,
		})
	}

	// <img src / data-src / data-original / data-lazy>
	doc.Find("img").Each(func(_ int, sel *goquery.Selection) {
		alt, _ := sel.Attr("alt")
		title, _ := sel.Attr("title")
		if title == "" {
			title = alt
		}
		for _, attr := range []string{"src", "data-src", "data-original", "data-lazy"} {
			if src, ok := sel.Attr(attr); ok && src != "" && !strings.HasPrefix(src, "data:") {
				addAsset(src, "image", title)
				break
			}
		}
		// srcset
		if srcset, ok := sel.Attr("srcset"); ok && srcset != "" {
			for _, part := range strings.Split(srcset, ",") {
				if fields := strings.Fields(strings.TrimSpace(part)); len(fields) > 0 {
					addAsset(fields[0], "image", alt)
				}
			}
		}
	})

	// <picture><source srcset>
	doc.Find("picture source[srcset]").Each(func(_ int, sel *goquery.Selection) {
		srcset, _ := sel.Attr("srcset")
		for _, part := range strings.Split(srcset, ",") {
			if fields := strings.Fields(strings.TrimSpace(part)); len(fields) > 0 {
				addAsset(fields[0], "image", "")
			}
		}
	})

	// og:image / twitter:image
	doc.Find(`meta[property="og:image"], meta[name="twitter:image"]`).Each(func(_ int, sel *goquery.Selection) {
		if content, ok := sel.Attr("content"); ok && content != "" {
			addAsset(content, "image", pageTitle)
		}
	})

	// <link rel="preload" as="image">
	doc.Find(`link[rel="preload"][as="image"]`).Each(func(_ int, sel *goquery.Selection) {
		if href, ok := sel.Attr("href"); ok {
			addAsset(href, "image", "")
		}
	})

	// <video src> and <video><source src>
	doc.Find("video[src]").Each(func(_ int, sel *goquery.Selection) {
		src, _ := sel.Attr("src")
		title, _ := sel.Attr("title")
		addAsset(src, "video", title)
	})
	doc.Find("video source[src]").Each(func(_ int, sel *goquery.Selection) {
		src, _ := sel.Attr("src")
		addAsset(src, "video", "")
	})

	// <audio src> and <audio><source src>
	doc.Find("audio[src]").Each(func(_ int, sel *goquery.Selection) {
		src, _ := sel.Attr("src")
		title, _ := sel.Attr("title")
		addAsset(src, "audio", title)
	})
	doc.Find("audio source[src]").Each(func(_ int, sel *goquery.Selection) {
		src, _ := sel.Attr("src")
		addAsset(src, "audio", "")
	})

	// <a href> pointing to media files (by extension)
	doc.Find("a[href]").Each(func(_ int, sel *goquery.Selection) {
		href, _ := sel.Attr("href")
		if href == "" {
			return
		}
		ext := strings.ToLower(path.Ext(strings.SplitN(href, "?", 2)[0]))
		var assetType string
		switch ext {
		case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp", ".avif":
			assetType = "image"
		case ".mp4", ".mov", ".avi", ".mkv", ".webm":
			assetType = "video"
		case ".mp3", ".wav", ".ogg", ".flac", ".aac", ".m4a", ".opus":
			assetType = "audio"
		}
		if assetType != "" {
			linkText := strings.TrimSpace(sel.Text())
			addAsset(href, assetType, linkText)
		}
	})

	// Collect navigable links for depth-following (HTML pages only)
	var links []string
	doc.Find("a[href]").Each(func(_ int, sel *goquery.Selection) {
		href, _ := sel.Attr("href")
		if href == "" || strings.HasPrefix(href, "#") ||
			strings.HasPrefix(href, "javascript:") || strings.HasPrefix(href, "mailto:") {
			return
		}
		abs := webpageResolveURL(base, href)
		if abs == "" {
			return
		}
		p, parseErr := url.Parse(abs)
		if parseErr != nil {
			return
		}
		ext := strings.ToLower(path.Ext(p.Path))
		// Only follow HTML-like URLs
		if ext == "" || ext == ".html" || ext == ".htm" || ext == ".php" || ext == ".asp" || ext == ".aspx" {
			links = append(links, abs)
		}
	})

	return assets, links, nil
}

// webpageResolveURL resolves rawURL against base, returning "" for data URIs or parse errors.
func webpageResolveURL(base *url.URL, rawURL string) string {
	if rawURL == "" || strings.HasPrefix(rawURL, "data:") {
		return ""
	}
	ref, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	resolved := base.ResolveReference(ref)
	// Strip fragment
	resolved.Fragment = ""
	return resolved.String()
}

// WithStorage injects the storage service (called after construction).
func (s *AssetService) WithStorage(svc storage.Service) *AssetService {
	s.storageSvc = svc
	return s
}

// Ensure ErrRecordNotFound is available
var _ = gorm.ErrRecordNotFound
