package service

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

const lockTTL = 5 * time.Minute

// CollabEvent SSE 推送事件
type CollabEvent struct {
	Type     string      `json:"type"`               // chapter.updated / character.locked / member.joined …
	EntityID uint        `json:"entity_id,omitempty"`
	UserID   uint        `json:"user_id,omitempty"`
	UserName string      `json:"user,omitempty"`
	Summary  string      `json:"summary,omitempty"`
	Data     interface{} `json:"data,omitempty"`
}

func (e CollabEvent) ToSSE() string {
	b, _ := json.Marshal(e)
	return "data: " + string(b) + "\n\n"
}

// NovelMemberDTO 成员信息（含用户资料）
type NovelMemberDTO struct {
	ID       uint       `json:"id"`
	NovelID  uint       `json:"novel_id"`
	UserID   uint       `json:"user_id"`
	Role     string     `json:"role"`
	Status   string     `json:"status"`
	Nickname string     `json:"nickname"`
	Email    string     `json:"email"`
	Avatar   string     `json:"avatar"`
	JoinedAt *time.Time `json:"joined_at"`
}

// CollabService 协作服务
type CollabService struct {
	memberRepo *repository.NovelMemberRepository
	lockRepo   *repository.EditingLockRepository
	userRepo   *repository.UserRepository
	novelRepo  *repository.NovelRepository

	mu         sync.RWMutex
	sseClients map[uint][]chan CollabEvent // novelID → subscriber channels
}

func NewCollabService(
	memberRepo *repository.NovelMemberRepository,
	lockRepo *repository.EditingLockRepository,
	userRepo *repository.UserRepository,
	novelRepo *repository.NovelRepository,
) *CollabService {
	svc := &CollabService{
		memberRepo: memberRepo,
		lockRepo:   lockRepo,
		userRepo:   userRepo,
		novelRepo:  novelRepo,
		sseClients: make(map[uint][]chan CollabEvent),
	}
	// 后台定期清理过期锁
	go func() {
		ticker := time.NewTicker(2 * time.Minute)
		for range ticker.C {
			lockRepo.CleanupExpired()
		}
	}()
	return svc
}

// ─── 成员管理 ──────────────────────────────────────────────────────────────────

func (s *CollabService) GetMemberRole(novelID, userID uint) string {
	m, err := s.memberRepo.GetByNovelAndUser(novelID, userID)
	if err != nil || m.Status != "active" {
		return ""
	}
	return m.Role
}

// EnsureOwner 确保小说所有者在成员表中（首次访问时自动注册）
func (s *CollabService) EnsureOwner(novelID, ownerUserID uint) error {
	if _, err := s.memberRepo.GetByNovelAndUser(novelID, ownerUserID); err == nil {
		return nil // already exists
	}
	now := time.Now()
	return s.memberRepo.Create(&model.NovelMember{
		NovelID:  novelID,
		UserID:   ownerUserID,
		Role:     "owner",
		Status:   "active",
		JoinedAt: &now,
	})
}

func (s *CollabService) ListMembers(novelID uint) ([]*NovelMemberDTO, error) {
	members, err := s.memberRepo.ListByNovel(novelID)
	if err != nil {
		return nil, err
	}
	dtos := make([]*NovelMemberDTO, 0, len(members))
	for _, m := range members {
		dto := &NovelMemberDTO{
			ID:       m.ID,
			NovelID:  m.NovelID,
			UserID:   m.UserID,
			Role:     m.Role,
			Status:   m.Status,
			JoinedAt: m.JoinedAt,
		}
		if u, err := s.userRepo.GetByID(m.UserID); err == nil {
			dto.Nickname = u.Nickname
			if dto.Nickname == "" {
				dto.Nickname = u.Username
			}
			dto.Email = u.Email
			dto.Avatar = u.Avatar
		}
		dtos = append(dtos, dto)
	}
	return dtos, nil
}

// InviteMember 邀请用户（通过 email）。返回邀请链接 token。
func (s *CollabService) InviteMember(novelID, inviterUserID uint, email, role string) (string, error) {
	if role != "editor" && role != "viewer" {
		role = "viewer"
	}
	target, err := s.userRepo.GetByEmail(email)
	if err != nil {
		return "", fmt.Errorf("用户不存在: %s", email)
	}
	// 检查是否已是成员
	if existing, err := s.memberRepo.GetByNovelAndUser(novelID, target.ID); err == nil {
		if existing.Status == "active" {
			return "", fmt.Errorf("该用户已是协作成员")
		}
		// 重新邀请 pending 成员
		token := uuid.New().String()
		existing.InviteToken = token
		existing.Role = role
		existing.Status = "pending"
		existing.InvitedBy = inviterUserID
		s.memberRepo.Update(existing)
		return token, nil
	}
	token := uuid.New().String()
	m := &model.NovelMember{
		NovelID:     novelID,
		UserID:      target.ID,
		Role:        role,
		Status:      "pending",
		InvitedBy:   inviterUserID,
		InviteToken: token,
	}
	if err := s.memberRepo.Create(m); err != nil {
		return "", err
	}
	return token, nil
}

// AcceptInvite 接受邀请
func (s *CollabService) AcceptInvite(token string, userID uint) (uint, error) {
	m, err := s.memberRepo.GetByInviteToken(token)
	if err != nil {
		return 0, fmt.Errorf("邀请链接无效或已过期")
	}
	if m.UserID != userID {
		return 0, fmt.Errorf("邀请链接与当前账号不匹配")
	}
	now := time.Now()
	m.Status = "active"
	m.InviteToken = ""
	m.JoinedAt = &now
	if err := s.memberRepo.Update(m); err != nil {
		return 0, err
	}
	// 获取用户名用于广播
	name := ""
	if u, e := s.userRepo.GetByID(userID); e == nil {
		name = u.Nickname
		if name == "" {
			name = u.Username
		}
	}
	s.Broadcast(m.NovelID, CollabEvent{
		Type:     "member.joined",
		UserID:   userID,
		UserName: name,
		Summary:  name + " 加入了协作",
	})
	return m.NovelID, nil
}

// RemoveMember 移除成员（owner 才能移除，不能移除自己，不能移除其他 owner）
func (s *CollabService) RemoveMember(novelID, requesterUserID, targetUserID uint) error {
	req := s.GetMemberRole(novelID, requesterUserID)
	if req != "owner" {
		return fmt.Errorf("只有所有者可以移除成员")
	}
	if requesterUserID == targetUserID {
		return fmt.Errorf("不能移除自己")
	}
	target := s.GetMemberRole(novelID, targetUserID)
	if target == "owner" {
		return fmt.Errorf("不能移除其他所有者")
	}
	return s.memberRepo.Delete(novelID, targetUserID)
}

// UpdateMemberRole 修改成员角色
func (s *CollabService) UpdateMemberRole(novelID, requesterUserID, targetUserID uint, newRole string) error {
	req := s.GetMemberRole(novelID, requesterUserID)
	if req != "owner" {
		return fmt.Errorf("只有所有者可以修改角色")
	}
	if newRole != "editor" && newRole != "viewer" {
		return fmt.Errorf("无效角色: %s", newRole)
	}
	m, err := s.memberRepo.GetByNovelAndUser(novelID, targetUserID)
	if err != nil {
		return fmt.Errorf("成员不存在")
	}
	if m.Role == "owner" {
		return fmt.Errorf("不能修改所有者角色")
	}
	m.Role = newRole
	return s.memberRepo.Update(m)
}

// ─── 编辑锁 ──────────────────────────────────────────────────────────────────

func (s *CollabService) AcquireLock(entityType string, entityID, userID uint) (*model.EditingLock, bool) {
	name := ""
	if u, err := s.userRepo.GetByID(userID); err == nil {
		name = u.Nickname
		if name == "" {
			name = u.Username
		}
	}
	lock, ok := s.lockRepo.Acquire(entityType, entityID, userID, name, lockTTL)
	if ok {
		logger.Printf("[Collab] lock acquired: %s/%d by user %d", entityType, entityID, userID)
	}
	return lock, ok
}

func (s *CollabService) ReleaseLock(entityType string, entityID, userID uint) error {
	return s.lockRepo.Release(entityType, entityID, userID)
}

func (s *CollabService) RefreshLock(entityType string, entityID, userID uint) error {
	return s.lockRepo.Refresh(entityType, entityID, userID, lockTTL)
}

func (s *CollabService) GetLock(entityType string, entityID uint) (*model.EditingLock, error) {
	return s.lockRepo.Get(entityType, entityID)
}

// BroadcastLock 广播锁事件（附 novelID 上下文）
func (s *CollabService) BroadcastLock(novelID uint, entityType string, entityID, userID uint, eventType string) {
	name := ""
	if u, e := s.userRepo.GetByID(userID); e == nil {
		name = u.Nickname
		if name == "" {
			name = u.Username
		}
	}
	s.Broadcast(novelID, CollabEvent{
		Type:     eventType,
		EntityID: entityID,
		UserID:   userID,
		UserName: name,
		Data:     map[string]string{"entity_type": entityType},
	})
}

// ─── SSE 广播 ────────────────────────────────────────────────────────────────

// Subscribe 订阅某小说的协作事件。返回事件 channel 和取消订阅函数。
func (s *CollabService) Subscribe(novelID uint) (<-chan CollabEvent, func()) {
	ch := make(chan CollabEvent, 16)
	s.mu.Lock()
	s.sseClients[novelID] = append(s.sseClients[novelID], ch)
	s.mu.Unlock()
	unsub := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		list := s.sseClients[novelID]
		for i, c := range list {
			if c == ch {
				s.sseClients[novelID] = append(list[:i], list[i+1:]...)
				close(ch)
				break
			}
		}
	}
	return ch, unsub
}

// Broadcast 向某小说的所有订阅者广播事件。
func (s *CollabService) Broadcast(novelID uint, event CollabEvent) {
	s.mu.RLock()
	list := s.sseClients[novelID]
	s.mu.RUnlock()
	for _, ch := range list {
		select {
		case ch <- event:
		default: // 客户端消费太慢，丢弃
		}
	}
}
