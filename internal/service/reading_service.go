package service

import (
	"errors"

	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

var ErrChapterCommentPermission = errors.New("permission denied")

// ReadingService 管理章节点赞、章节评论、阅读进度与已读记录
type ReadingService struct {
	chapterLikeRepo    *repository.ChapterLikeRepository
	chapterCommentRepo *repository.ChapterCommentRepository
	readingProgressRepo *repository.ReadingProgressRepository
	chapterReadRepo    *repository.ChapterReadRecordRepository
	chapterRepo        *repository.ChapterRepository
}

func NewReadingService(
	chapterLikeRepo *repository.ChapterLikeRepository,
	chapterCommentRepo *repository.ChapterCommentRepository,
	readingProgressRepo *repository.ReadingProgressRepository,
	chapterReadRepo *repository.ChapterReadRecordRepository,
	chapterRepo *repository.ChapterRepository,
) *ReadingService {
	return &ReadingService{
		chapterLikeRepo:    chapterLikeRepo,
		chapterCommentRepo: chapterCommentRepo,
		readingProgressRepo: readingProgressRepo,
		chapterReadRepo:    chapterReadRepo,
		chapterRepo:        chapterRepo,
	}
}

// ─── Chapter Like ─────────────────────────────────────────────────────────────

func (s *ReadingService) ToggleChapterLike(chapterID, novelID, userID uint) (liked bool, likeCount int, err error) {
	return s.chapterLikeRepo.Toggle(chapterID, novelID, userID)
}

func (s *ReadingService) IsChapterLiked(chapterID, userID uint) bool {
	ok, _ := s.chapterLikeRepo.Exists(chapterID, userID)
	return ok
}

// ─── Chapter Comment ─────────────────────────────────────────────────────────

func (s *ReadingService) ListChapterComments(chapterID uint, page, size int) ([]*model.ChapterComment, int64, error) {
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 100 {
		size = 20
	}
	return s.chapterCommentRepo.ListByChapter(chapterID, page, size)
}

func (s *ReadingService) AddChapterComment(chapterID, novelID, userID uint, nickname, content string, parentID *uint) (*model.ChapterComment, error) {
	c := &model.ChapterComment{
		ChapterID: chapterID,
		NovelID:   novelID,
		UserID:    userID,
		Nickname:  nickname,
		Content:   content,
		ParentID:  parentID,
	}
	if err := s.chapterCommentRepo.Create(c); err != nil {
		return nil, err
	}
	return c, nil
}

func (s *ReadingService) DeleteChapterComment(commentID, userID uint) error {
	c, err := s.chapterCommentRepo.GetByID(commentID)
	if err != nil {
		return err
	}
	if c.UserID != userID {
		return ErrChapterCommentPermission
	}
	return s.chapterCommentRepo.Delete(commentID)
}

// ─── Reading Progress ─────────────────────────────────────────────────────────

// SaveProgress 保存用户阅读进度，同时标记该章节为已读
func (s *ReadingService) SaveProgress(userID, novelID uint, chapterNo int, chapterID uint, scrollPct int) error {
	if err := s.readingProgressRepo.Upsert(userID, novelID, chapterNo, chapterID, scrollPct); err != nil {
		return err
	}
	// 滚动超过 50% 自动标记已读
	if scrollPct >= 50 {
		_ = s.chapterReadRepo.MarkRead(userID, chapterID, novelID)
	}
	return nil
}

// GetProgress 获取用户对某小说的阅读进度（nil = 从未阅读）
func (s *ReadingService) GetProgress(userID, novelID uint) (*model.ReadingProgress, error) {
	return s.readingProgressRepo.Get(userID, novelID)
}

// GetReadHistory 获取用户阅读历史（分页）
func (s *ReadingService) GetReadHistory(userID uint, page, size int) ([]*model.ReadingProgress, int64, error) {
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 50 {
		size = 20
	}
	return s.readingProgressRepo.ListByUser(userID, page, size)
}

// GetReadChapterIDs 获取用户在某小说中已读的章节 ID 集合
func (s *ReadingService) GetReadChapterIDs(userID, novelID uint) ([]uint, error) {
	return s.chapterReadRepo.ListReadChapterIDs(userID, novelID)
}

// MarkChapterRead 主动标记章节为已读（打开章节即调用）
func (s *ReadingService) MarkChapterRead(userID, chapterID, novelID uint) error {
	return s.chapterReadRepo.MarkRead(userID, chapterID, novelID)
}
