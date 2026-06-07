package repository

import (
	"strings"
	"time"

	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// ─── AssetSearchParams ────────────────────────────────────────────────────────

type AssetSearchParams struct {
	Scope          string // "personal"|"public"|"all"
	CallerID       uint
	TenantID       uint
	Q              string
	Type           string
	SubType        string
	Source         string
	Tags           []string // AND
	TagsOr         []string // OR
	License        string
	DominantColor  string
	ColorTolerance int
	Status         string // filter by status; empty = active only
	Sort           string // created_at|use_count|like_count|value_score
	Page           int
	PageSize       int
}

// ─── AssetRepository ─────────────────────────────────────────────────────────

type AssetRepository struct{ db *gorm.DB }

func NewAssetRepository(db *gorm.DB) *AssetRepository { return &AssetRepository{db: db} }

func (r *AssetRepository) Create(a *model.Asset) error { return r.db.Create(a).Error }

func (r *AssetRepository) GetByID(id uint) (*model.Asset, error) {
	var a model.Asset
	err := r.db.Preload("Tags").Where("id = ? AND (deleted_at IS NULL OR status != ?)", id, model.AssetStatusTrash).First(&a).Error
	return &a, err
}

func (r *AssetRepository) Update(a *model.Asset) error { return r.db.Save(a).Error }

func (r *AssetRepository) UpdateFields(id uint, fields map[string]interface{}) error {
	return r.db.Model(&model.Asset{}).Where("id = ?", id).Updates(fields).Error
}

func (r *AssetRepository) SoftDelete(id, deletedBy uint) error {
	now := time.Now()
	return r.db.Model(&model.Asset{}).Where("id = ?", id).Updates(map[string]interface{}{
		"status": model.AssetStatusTrash, "deleted_at": now, "deleted_by": deletedBy,
	}).Error
}

func (r *AssetRepository) Restore(id uint) error {
	return r.db.Model(&model.Asset{}).Where("id = ?", id).Updates(map[string]interface{}{
		"status": model.AssetStatusActive, "deleted_at": nil, "deleted_by": nil,
	}).Error
}

func (r *AssetRepository) HardDelete(id uint) error {
	return r.db.Unscoped().Delete(&model.Asset{}, id).Error
}

// SoftDeleteAllByCreator soft-deletes all active personal-scope assets owned by creatorID.
// Returns the number of rows affected.
func (r *AssetRepository) SoftDeleteAllByCreator(creatorID uint) (int64, error) {
	now := time.Now()
	tx := r.db.Model(&model.Asset{}).
		Where("creator_id = ? AND scope = ? AND status = ?", creatorID, model.AssetScopePersonal, model.AssetStatusActive).
		Updates(map[string]interface{}{
			"status": model.AssetStatusTrash, "deleted_at": now, "deleted_by": creatorID,
		})
	return tx.RowsAffected, tx.Error
}

// HardDeleteAllTrash permanently removes every trash item belonging to creatorID.
// Returns the number of rows deleted.
func (r *AssetRepository) HardDeleteAllTrash(creatorID uint) (int64, error) {
	tx := r.db.Unscoped().Where("creator_id = ? AND status = ?", creatorID, model.AssetStatusTrash).
		Delete(&model.Asset{})
	return tx.RowsAffected, tx.Error
}

func (r *AssetRepository) ListTrash(creatorID uint, page, size int) ([]*model.Asset, int64, error) {
	var assets []*model.Asset
	var total int64
	q := r.db.Model(&model.Asset{}).Where("creator_id = ? AND status = ?", creatorID, model.AssetStatusTrash)
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	err := q.Order("deleted_at DESC").Offset((page - 1) * size).Limit(size).Find(&assets).Error
	return assets, total, err
}

func (r *AssetRepository) Search(p AssetSearchParams) ([]*model.Asset, int64, error) {
	q := r.db.Model(&model.Asset{})

	// Scope filter
	switch p.Scope {
	case model.AssetScopePublic:
		q = q.Where("scope = ?", model.AssetScopePublic)
	case model.AssetScopePersonal:
		q = q.Where("scope = ? AND creator_id = ?", model.AssetScopePersonal, p.CallerID)
	default: // "all" or empty
		q = q.Where("scope = ? OR (scope = ? AND creator_id = ?)",
			model.AssetScopePublic, model.AssetScopePersonal, p.CallerID)
	}

	// Status filter
	if p.Status != "" {
		q = q.Where("status = ?", p.Status)
	} else {
		q = q.Where("status = ?", model.AssetStatusActive)
	}

	if p.Type != "" {
		q = q.Where("type = ?", p.Type)
	}
	if p.SubType != "" {
		q = q.Where("sub_type = ?", p.SubType)
	}
	if p.Source != "" {
		q = q.Where("source = ?", p.Source)
	}
	if p.License != "" {
		q = q.Where("license = ?", p.License)
	}
	if p.Q != "" {
		like := "%" + p.Q + "%"
		q = q.Where("title LIKE ? OR description LIKE ?", like, like)
	}
	if p.DominantColor != "" {
		q = q.Where("dominant_color = ?", p.DominantColor)
	}

	// AND-tag filter
	if len(p.Tags) > 0 {
		tagIDs := r.resolveTagSlugs(p.Tags)
		if len(tagIDs) > 0 {
			q = q.Where("id IN (?)",
				r.db.Table("ink_asset_tag_map").
					Select("asset_id").
					Where("tag_id IN ?", tagIDs).
					Group("asset_id").
					Having("COUNT(DISTINCT tag_id) = ?", len(tagIDs)))
		}
	}
	// OR-tag filter
	if len(p.TagsOr) > 0 {
		tagIDs := r.resolveTagSlugs(p.TagsOr)
		if len(tagIDs) > 0 {
			q = q.Where("id IN (?)",
				r.db.Table("ink_asset_tag_map").Select("DISTINCT asset_id").Where("tag_id IN ?", tagIDs))
		}
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	// Sort
	sortField := "created_at"
	if p.Sort == "use_count" || p.Sort == "like_count" || p.Sort == "value_score" {
		sortField = p.Sort
	}
	q = q.Order(sortField + " DESC")

	if p.Page < 1 {
		p.Page = 1
	}
	if p.PageSize < 1 {
		p.PageSize = 20
	}

	var assets []*model.Asset
	err := q.Preload("Tags").Offset((p.Page - 1) * p.PageSize).Limit(p.PageSize).Find(&assets).Error
	return assets, total, err
}

func (r *AssetRepository) resolveTagSlugs(names []string) []uint {
	slugs := make([]string, len(names))
	for i, n := range names {
		slugs[i] = strings.ToLower(strings.ReplaceAll(n, " ", "-"))
	}
	var tags []model.Tag
	r.db.Where("slug IN ? OR name IN ?", slugs, names).Find(&tags)
	ids := make([]uint, len(tags))
	for i, t := range tags {
		ids[i] = t.ID
	}
	return ids
}

func (r *AssetRepository) FindByPHashSimilar(phash string, _ int) ([]*model.Asset, error) {
	// Simple exact-match; Hamming distance filtering is done in-memory by the service.
	var assets []*model.Asset
	err := r.db.Where("phash != '' AND phash = ?", phash).Limit(10).Find(&assets).Error
	return assets, err
}

func (r *AssetRepository) IncrUseCount(id uint) error {
	return r.db.Model(&model.Asset{}).Where("id = ?", id).
		UpdateColumn("use_count", gorm.Expr("use_count + 1")).Error
}

func (r *AssetRepository) IncrLikeCount(id uint, delta int) error {
	return r.db.Model(&model.Asset{}).Where("id = ?", id).
		UpdateColumn("like_count", gorm.Expr("like_count + ?", delta)).Error
}

func (r *AssetRepository) ExistsByExternalID(externalID string) (bool, error) {
	var count int64
	err := r.db.Model(&model.Asset{}).Where("external_id = ?", externalID).Count(&count).Error
	return count > 0, err
}

func (r *AssetRepository) UpdateValueScore(id uint, score float64) error {
	return r.db.Model(&model.Asset{}).Where("id = ?", id).
		UpdateColumn("value_score", score).Error
}

func (r *AssetRepository) ListPublicByValueScore(limit int) ([]*model.Asset, error) {
	var assets []*model.Asset
	err := r.db.Where("scope = ? AND status = ?", model.AssetScopePublic, model.AssetStatusActive).
		Order("value_score DESC").Limit(limit).Find(&assets).Error
	return assets, err
}

func (r *AssetRepository) ListPublicAll() ([]*model.Asset, error) {
	var assets []*model.Asset
	err := r.db.Where("scope = ? AND status = ?", model.AssetScopePublic, model.AssetStatusActive).
		Find(&assets).Error
	return assets, err
}

func (r *AssetRepository) SearchByColor(hexColor string, _ int, scope string, callerID uint, page, size int) ([]*model.Asset, int64, error) {
	q := r.db.Model(&model.Asset{}).Where("dominant_color = ? AND status = ?", hexColor, model.AssetStatusActive)
	if scope == model.AssetScopePublic {
		q = q.Where("scope = ?", model.AssetScopePublic)
	} else if scope == model.AssetScopePersonal {
		q = q.Where("scope = ? AND creator_id = ?", model.AssetScopePersonal, callerID)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var assets []*model.Asset
	err := q.Offset((page - 1) * size).Limit(size).Find(&assets).Error
	return assets, total, err
}

// ─── TagRepository ────────────────────────────────────────────────────────────

type TagRepository struct{ db *gorm.DB }

func NewTagRepository(db *gorm.DB) *TagRepository { return &TagRepository{db: db} }

func (r *TagRepository) FindOrCreate(name, category string) (*model.Tag, error) {
	slug := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), " ", "-"))
	var tag model.Tag
	err := r.db.Where("slug = ?", slug).First(&tag).Error
	if err == gorm.ErrRecordNotFound {
		tag = model.Tag{Name: name, Slug: slug, Category: category}
		err = r.db.Create(&tag).Error
	}
	return &tag, err
}

func (r *TagRepository) ListByCategory() (map[string][]*model.Tag, error) {
	var tags []*model.Tag
	if err := r.db.Order("use_count DESC").Find(&tags).Error; err != nil {
		return nil, err
	}
	result := map[string][]*model.Tag{}
	for _, t := range tags {
		result[t.Category] = append(result[t.Category], t)
	}
	return result, nil
}

func (r *TagRepository) Suggest(q string, limit int) ([]*model.Tag, error) {
	var tags []*model.Tag
	err := r.db.Where("name LIKE ?", "%"+q+"%").Order("use_count DESC").Limit(limit).Find(&tags).Error
	return tags, err
}

func (r *TagRepository) AddToAsset(assetID, tagID uint, source string, confidence float64) error {
	m := model.AssetTagMap{AssetID: assetID, TagID: tagID, Source: source, Confidence: confidence, CreatedAt: time.Now()}
	return r.db.Where("asset_id = ? AND tag_id = ?", assetID, tagID).FirstOrCreate(&m).Error
}

func (r *TagRepository) RemoveFromAsset(assetID, tagID uint) error {
	return r.db.Where("asset_id = ? AND tag_id = ?", assetID, tagID).Delete(&model.AssetTagMap{}).Error
}

func (r *TagRepository) ListByAsset(assetID uint) ([]*model.Tag, error) {
	var tags []*model.Tag
	err := r.db.Joins("JOIN ink_asset_tag_map m ON m.tag_id = ink_tag.id").
		Where("m.asset_id = ?", assetID).Find(&tags).Error
	return tags, err
}

func (r *TagRepository) IncrUseCount(tagID uint) error {
	return r.db.Model(&model.Tag{}).Where("id = ?", tagID).
		UpdateColumn("use_count", gorm.Expr("use_count + 1")).Error
}

func (r *TagRepository) RecalcUseCounts() error {
	return r.db.Exec(`UPDATE ink_tag t SET use_count =
		(SELECT COUNT(*) FROM ink_asset_tag_map m WHERE m.tag_id = t.id)`).Error
}

// ─── AssetVersionRepository ───────────────────────────────────────────────────

type AssetVersionRepository struct{ db *gorm.DB }

func NewAssetVersionRepository(db *gorm.DB) *AssetVersionRepository {
	return &AssetVersionRepository{db: db}
}

func (r *AssetVersionRepository) Create(v *model.AssetVersion) error { return r.db.Create(v).Error }

func (r *AssetVersionRepository) ListByAsset(assetID uint) ([]*model.AssetVersion, error) {
	var vs []*model.AssetVersion
	err := r.db.Where("asset_id = ?", assetID).Order("version_no DESC").Find(&vs).Error
	return vs, err
}

func (r *AssetVersionRepository) GetByVersionNo(assetID uint, versionNo int) (*model.AssetVersion, error) {
	var v model.AssetVersion
	err := r.db.Where("asset_id = ? AND version_no = ?", assetID, versionNo).First(&v).Error
	return &v, err
}

func (r *AssetVersionRepository) MaxVersionNo(assetID uint) (int, error) {
	var max int
	err := r.db.Model(&model.AssetVersion{}).Where("asset_id = ?", assetID).
		Select("COALESCE(MAX(version_no), 0)").Scan(&max).Error
	return max, err
}

// ─── AssetCollectionRepository ───────────────────────────────────────────────

type AssetCollectionRepository struct{ db *gorm.DB }

func NewAssetCollectionRepository(db *gorm.DB) *AssetCollectionRepository {
	return &AssetCollectionRepository{db: db}
}

func (r *AssetCollectionRepository) Create(c *model.AssetCollection) error {
	return r.db.Create(c).Error
}

func (r *AssetCollectionRepository) GetByID(id uint) (*model.AssetCollection, error) {
	var c model.AssetCollection
	return &c, r.db.First(&c, id).Error
}

func (r *AssetCollectionRepository) Update(c *model.AssetCollection) error { return r.db.Save(c).Error }

func (r *AssetCollectionRepository) Delete(id uint) error {
	return r.db.Delete(&model.AssetCollection{}, id).Error
}

func (r *AssetCollectionRepository) ListByCreator(creatorID uint) ([]*model.AssetCollection, error) {
	var cs []*model.AssetCollection
	err := r.db.Where("creator_id = ?", creatorID).Order("created_at DESC").Find(&cs).Error
	return cs, err
}

func (r *AssetCollectionRepository) AddItem(collectionID, assetID uint) error {
	item := model.AssetCollectionItem{CollectionID: collectionID, AssetID: assetID, AddedAt: time.Now()}
	if err := r.db.Where("collection_id = ? AND asset_id = ?", collectionID, assetID).
		FirstOrCreate(&item).Error; err != nil {
		return err
	}
	return r.db.Model(&model.AssetCollection{}).Where("id = ?", collectionID).
		UpdateColumn("asset_count", gorm.Expr("asset_count + 1")).Error
}

func (r *AssetCollectionRepository) RemoveItem(collectionID, assetID uint) error {
	if err := r.db.Where("collection_id = ? AND asset_id = ?", collectionID, assetID).
		Delete(&model.AssetCollectionItem{}).Error; err != nil {
		return err
	}
	return r.db.Model(&model.AssetCollection{}).Where("id = ?", collectionID).
		UpdateColumn("asset_count", gorm.Expr("GREATEST(asset_count - 1, 0)")).Error
}

func (r *AssetCollectionRepository) ListItems(collectionID uint) ([]*model.Asset, error) {
	var assets []*model.Asset
	err := r.db.Joins("JOIN ink_asset_collection_item ci ON ci.asset_id = ink_asset.id").
		Where("ci.collection_id = ?", collectionID).
		Order("ci.sort_order ASC, ci.added_at DESC").Find(&assets).Error
	return assets, err
}

// ─── AssetShareRequestRepository ─────────────────────────────────────────────

type AssetShareRequestRepository struct{ db *gorm.DB }

func NewAssetShareRequestRepository(db *gorm.DB) *AssetShareRequestRepository {
	return &AssetShareRequestRepository{db: db}
}

func (r *AssetShareRequestRepository) Create(req *model.AssetShareRequest) error {
	return r.db.Create(req).Error
}

func (r *AssetShareRequestRepository) GetByAssetID(assetID uint) (*model.AssetShareRequest, error) {
	var req model.AssetShareRequest
	err := r.db.Where("asset_id = ?", assetID).First(&req).Error
	return &req, err
}

func (r *AssetShareRequestRepository) GetByID(id uint) (*model.AssetShareRequest, error) {
	var req model.AssetShareRequest
	err := r.db.First(&req, id).Error
	return &req, err
}

func (r *AssetShareRequestRepository) Update(req *model.AssetShareRequest) error {
	return r.db.Save(req).Error
}

func (r *AssetShareRequestRepository) Delete(id uint) error {
	return r.db.Delete(&model.AssetShareRequest{}, id).Error
}

func (r *AssetShareRequestRepository) ListPending(page, size int) ([]*model.AssetShareRequest, int64, error) {
	var reqs []*model.AssetShareRequest
	var total int64
	q := r.db.Model(&model.AssetShareRequest{}).Where("status = ?", "pending")
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	err := q.Order("created_at ASC").Offset((page - 1) * size).Limit(size).Find(&reqs).Error
	return reqs, total, err
}

// ─── AssetUsageRepository ─────────────────────────────────────────────────────

type AssetUsageRepository struct{ db *gorm.DB }

func NewAssetUsageRepository(db *gorm.DB) *AssetUsageRepository {
	return &AssetUsageRepository{db: db}
}

func (r *AssetUsageRepository) Create(u *model.AssetUsage) error { return r.db.Create(u).Error }

func (r *AssetUsageRepository) ListByAsset(assetID uint) ([]*model.AssetUsage, error) {
	var us []*model.AssetUsage
	err := r.db.Where("asset_id = ?", assetID).Order("used_at DESC").Limit(50).Find(&us).Error
	return us, err
}

// ─── CrawlJobRepository ───────────────────────────────────────────────────────

type CrawlJobRepository struct{ db *gorm.DB }

func NewCrawlJobRepository(db *gorm.DB) *CrawlJobRepository { return &CrawlJobRepository{db: db} }

func (r *CrawlJobRepository) Create(j *model.CrawlJob) error { return r.db.Create(j).Error }

func (r *CrawlJobRepository) GetByID(id uint) (*model.CrawlJob, error) {
	var j model.CrawlJob
	return &j, r.db.First(&j, id).Error
}

func (r *CrawlJobRepository) Update(j *model.CrawlJob) error { return r.db.Save(j).Error }

func (r *CrawlJobRepository) UpdateProgress(id uint, imported, skipped, failed int) error {
	return r.db.Model(&model.CrawlJob{}).Where("id = ?", id).Updates(map[string]interface{}{
		"imported": imported, "skipped": skipped, "failed": failed,
	}).Error
}

func (r *CrawlJobRepository) Reset(id uint) error {
	return r.db.Model(&model.CrawlJob{}).Where("id = ?", id).Updates(map[string]interface{}{
		"status": "pending", "error_msg": "", "imported": 0, "skipped": 0,
		"failed": 0, "total_found": 0, "started_at": nil, "completed_at": nil,
	}).Error
}

func (r *CrawlJobRepository) UpdateFinal(id uint, status string, totalFound int, errMsg string, ts *time.Time) error {
	fields := map[string]interface{}{"status": status}
	if totalFound > 0 {
		fields["total_found"] = totalFound
	}
	if errMsg != "" {
		fields["error_msg"] = errMsg
	}
	if ts != nil {
		switch status {
		case "running":
			fields["started_at"] = ts
		default:
			fields["completed_at"] = ts
		}
	}
	return r.db.Model(&model.CrawlJob{}).Where("id = ?", id).Updates(fields).Error
}

func (r *CrawlJobRepository) List(page, size int) ([]*model.CrawlJob, int64, error) {
	var jobs []*model.CrawlJob
	var total int64
	if err := r.db.Model(&model.CrawlJob{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	err := r.db.Order("created_at DESC").Offset((page - 1) * size).Limit(size).Find(&jobs).Error
	return jobs, total, err
}

func (r *CrawlJobRepository) SetTaskID(id uint, taskID string) error {
	return r.db.Model(&model.CrawlJob{}).Where("id = ?", id).Update("task_id", taskID).Error
}

// MarkRunningAsFailed marks jobs stuck in "running" or "pending" as "failed".
// Called on server startup to recover from unclean shutdowns.
func (r *CrawlJobRepository) MarkRunningAsFailed() error {
	return r.db.Model(&model.CrawlJob{}).
		Where("status IN ?", []string{"running", "pending"}).
		Updates(map[string]interface{}{"status": "failed", "error_msg": "server restarted"}).Error
}

// ─── ShareLinkRepository ──────────────────────────────────────────────────────

type ShareLinkRepository struct{ db *gorm.DB }

func NewShareLinkRepository(db *gorm.DB) *ShareLinkRepository {
	return &ShareLinkRepository{db: db}
}

func (r *ShareLinkRepository) Create(sl *model.ShareLink) error { return r.db.Create(sl).Error }

func (r *ShareLinkRepository) GetByToken(token string) (*model.ShareLink, error) {
	var sl model.ShareLink
	return &sl, r.db.Where("token = ?", token).First(&sl).Error
}

func (r *ShareLinkRepository) ListByCreator(creatorID uint) ([]*model.ShareLink, error) {
	var sls []*model.ShareLink
	err := r.db.Where("created_by = ?", creatorID).Order("created_at DESC").Find(&sls).Error
	return sls, err
}

func (r *ShareLinkRepository) Delete(token string) error {
	return r.db.Where("token = ?", token).Delete(&model.ShareLink{}).Error
}

func (r *ShareLinkRepository) IncrViewCount(token string) error {
	return r.db.Model(&model.ShareLink{}).Where("token = ?", token).
		UpdateColumn("view_count", gorm.Expr("view_count + 1")).Error
}

// ─── SearchLogRepository ──────────────────────────────────────────────────────

type SearchLogRepository struct{ db *gorm.DB }

func NewSearchLogRepository(db *gorm.DB) *SearchLogRepository {
	return &SearchLogRepository{db: db}
}

func (r *SearchLogRepository) Create(sl *model.SearchLog) error { return r.db.Create(sl).Error }

func (r *SearchLogRepository) ListGaps(scope string, limit int) ([]struct {
	Query string
	Count int
}, error) {
	type row struct {
		Query string
		Count int
	}
	var rows []row
	q := r.db.Table("ink_search_log").
		Select("query, COUNT(*) as count").
		Where("result_count = 0").
		Group("query").Order("count DESC").Limit(limit)
	if scope != "" {
		q = q.Where("search_scope = ?", scope)
	}
	err := q.Scan(&rows).Error
	result := make([]struct {
		Query string
		Count int
	}, len(rows))
	for i, r := range rows {
		result[i] = struct {
			Query string
			Count int
		}{r.Query, r.Count}
	}
	return result, err
}

func (r *SearchLogRepository) CleanOlderThan(days int) error {
	cutoff := time.Now().AddDate(0, 0, -days)
	return r.db.Where("searched_at < ?", cutoff).Delete(&model.SearchLog{}).Error
}

// ─── TenantQuotaRepository ────────────────────────────────────────────────────

type AssetStorageQuotaRepository struct{ db *gorm.DB }

func NewAssetStorageQuotaRepository(db *gorm.DB) *AssetStorageQuotaRepository {
	return &AssetStorageQuotaRepository{db: db}
}

func (r *AssetStorageQuotaRepository) Get(tenantID uint) (*model.AssetStorageQuota, error) {
	var q model.AssetStorageQuota
	err := r.db.Where("tenant_id = ?", tenantID).First(&q).Error
	if err == gorm.ErrRecordNotFound {
		q = model.AssetStorageQuota{TenantID: tenantID}
		err = r.db.Create(&q).Error
	}
	return &q, err
}

func (r *AssetStorageQuotaRepository) AddStorage(tenantID uint, bytes int64) error {
	return r.db.Model(&model.AssetStorageQuota{}).Where("tenant_id = ?", tenantID).
		UpdateColumn("storage_used_bytes", gorm.Expr("storage_used_bytes + ?", bytes)).Error
}

func (r *AssetStorageQuotaRepository) SubStorage(tenantID uint, bytes int64) error {
	return r.db.Model(&model.AssetStorageQuota{}).Where("tenant_id = ?", tenantID).
		UpdateColumn("storage_used_bytes", gorm.Expr("GREATEST(storage_used_bytes - ?, 0)", bytes)).Error
}

func (r *AssetStorageQuotaRepository) ResetMonthlyCrawl() error {
	return r.db.Model(&model.AssetStorageQuota{}).Where("1=1").
		UpdateColumn("crawl_used_this_month", 0).Error
}

// ─── AssetLikeRepository ──────────────────────────────────────────────────────

type AssetLikeRepository struct{ db *gorm.DB }

func NewAssetLikeRepository(db *gorm.DB) *AssetLikeRepository {
	return &AssetLikeRepository{db: db}
}

func (r *AssetLikeRepository) Toggle(assetID, userID uint) (liked bool, err error) {
	var like model.AssetLike
	err = r.db.Where("asset_id = ? AND user_id = ?", assetID, userID).First(&like).Error
	if err == gorm.ErrRecordNotFound {
		err = r.db.Create(&model.AssetLike{AssetID: assetID, UserID: userID, CreatedAt: time.Now()}).Error
		return true, err
	}
	if err != nil {
		return false, err
	}
	return false, r.db.Where("asset_id = ? AND user_id = ?", assetID, userID).Delete(&model.AssetLike{}).Error
}

// ─── AssetCommentRepository ───────────────────────────────────────────────────

type AssetCommentRepository struct{ db *gorm.DB }

func NewAssetCommentRepository(db *gorm.DB) *AssetCommentRepository {
	return &AssetCommentRepository{db: db}
}

func (r *AssetCommentRepository) Create(c *model.AssetComment) error { return r.db.Create(c).Error }

func (r *AssetCommentRepository) GetByID(id uint) (*model.AssetComment, error) {
	var c model.AssetComment
	return &c, r.db.First(&c, id).Error
}

func (r *AssetCommentRepository) ListByAsset(assetID uint) ([]*model.AssetComment, error) {
	var cs []*model.AssetComment
	err := r.db.Where("asset_id = ?", assetID).Order("created_at ASC").Find(&cs).Error
	return cs, err
}

func (r *AssetCommentRepository) Delete(id uint) error {
	return r.db.Delete(&model.AssetComment{}, id).Error
}
