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

const lockTTL = 90 * time.Second

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
	ID              uint       `json:"id"`
	NovelID         uint       `json:"novel_id"`
	UserID          uint       `json:"user_id"`
	Role            string     `json:"role"`
	Status          string     `json:"status"`
	Nickname        string     `json:"nickname"`
	Email           string     `json:"email"`
	Avatar          string     `json:"avatar"`
	JoinedAt        *time.Time `json:"joined_at"`
	InviteExpiresAt *time.Time `json:"invite_expires_at,omitempty"`
}

// CollabService 协作服务
type CollabService struct {
	memberRepo     *repository.NovelMemberRepository
	lockRepo       *repository.EditingLockRepository
	userRepo       *repository.UserRepository
	novelRepo      *repository.NovelRepository
	tenantUserRepo *repository.TenantUserRepository // 用于查询被邀请者的 TenantID
	notifSvc       *NotificationService              // 站内信发送

	mu         sync.RWMutex
	sseClients map[uint][]chan CollabEvent // novelID → subscriber channels
	stopCh     chan struct{}
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
		stopCh:     make(chan struct{}),
	}
	// 后台定期清理过期锁
	go func() {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-svc.stopCh:
				return
			case <-ticker.C:
				lockRepo.CleanupExpired()
			}
		}
	}()
	return svc
}

// Shutdown 停止所有后台 goroutine（优雅关闭时调用）。
func (s *CollabService) Shutdown() {
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
	}
}

// WithTenantUserRepo 注入租户用户仓库（用于获取被邀请者的 TenantID）
func (s *CollabService) WithTenantUserRepo(r *repository.TenantUserRepository) *CollabService {
	s.tenantUserRepo = r
	return s
}

// WithNotificationService 注入站内信服务
func (s *CollabService) WithNotificationService(svc *NotificationService) *CollabService {
	s.notifSvc = svc
	return s
}

// ─── 成员管理 ──────────────────────────────────────────────────────────────────

func (s *CollabService) GetMemberRole(novelID, userID uint) string {
	m, err := s.memberRepo.GetByNovelAndUser(novelID, userID)
	if err != nil || m.Status != "active" {
		return ""
	}
	return m.Role
}

// EnsureOwner 确保小说所有者在成员表中（首次访问时自动注册，原子 upsert 防竞态）。
func (s *CollabService) EnsureOwner(novelID, ownerUserID uint) error {
	return s.memberRepo.EnsureOwner(novelID, ownerUserID, time.Now())
}

// LeaveNovel 当前用户主动退出协作（owner 不能退出）。
func (s *CollabService) LeaveNovel(novelID, userID uint) error {
	role := s.GetMemberRole(novelID, userID)
	if role == "" {
		return fmt.Errorf("不是该项目的协作成员")
	}
	if role == "owner" {
		return fmt.Errorf("所有者不能退出，请先转让所有权")
	}
	return s.memberRepo.Delete(novelID, userID)
}

func (s *CollabService) ListMembers(novelID uint) ([]*NovelMemberDTO, error) {
	members, err := s.memberRepo.ListByNovel(novelID)
	if err != nil {
		return nil, err
	}

	// batch-fetch users in one query to avoid N+1
	userIDs := make([]uint, 0, len(members))
	for _, m := range members {
		userIDs = append(userIDs, m.UserID)
	}
	userMap := make(map[uint]*model.User)
	if users, err := s.userRepo.GetByIDs(userIDs); err == nil {
		for _, u := range users {
			userMap[u.ID] = u
		}
	}

	dtos := make([]*NovelMemberDTO, 0, len(members))
	for _, m := range members {
		dto := &NovelMemberDTO{
			ID:              m.ID,
			NovelID:         m.NovelID,
			UserID:          m.UserID,
			Role:            m.Role,
			Status:          m.Status,
			JoinedAt:        m.JoinedAt,
			InviteExpiresAt: m.InviteExpiresAt,
		}
		if u, ok := userMap[m.UserID]; ok {
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

// InviteMember 邀请用户（通过 email）。
// ttlMinutes：邀请链接有效期（分钟），0 表示使用默认值 10 分钟。
// 返回邀请 token，并向被邀请者发送站内信。
func (s *CollabService) InviteMember(novelID, inviterUserID uint, email, role string, ttlMinutes int) (string, error) {
	if role != "editor" && role != "viewer" {
		role = "viewer"
	}
	if ttlMinutes <= 0 {
		ttlMinutes = 10
	}
	target, err := s.userRepo.GetByEmail(email)
	if err != nil {
		return "", fmt.Errorf("用户不存在: %s", email)
	}
	expiresAt := time.Now().Add(time.Duration(ttlMinutes) * time.Minute)
	token := uuid.New().String()

	// 检查是否已是成员
	if existing, err := s.memberRepo.GetByNovelAndUser(novelID, target.ID); err == nil {
		if existing.Status == "active" {
			return "", fmt.Errorf("该用户已是协作成员")
		}
		// 重新邀请 pending 成员（刷新 token 和有效期）
		existing.InviteToken = token
		existing.InviteExpiresAt = &expiresAt
		existing.Role = role
		existing.Status = "pending"
		existing.InvitedBy = inviterUserID
		if err := s.memberRepo.Update(existing); err != nil {
			return "", err
		}
	} else {
		m := &model.NovelMember{
			NovelID:         novelID,
			UserID:          target.ID,
			Role:            role,
			Status:          "pending",
			InvitedBy:       inviterUserID,
			InviteToken:     token,
			InviteExpiresAt: &expiresAt,
		}
		if err := s.memberRepo.Create(m); err != nil {
			return "", err
		}
	}

	// 发送站内信通知（失败不影响主流程）
	go s.sendCollabInviteNotif(novelID, inviterUserID, target.ID, role, token, ttlMinutes)
	return token, nil
}

// sendCollabInviteNotif 向被邀请者发送协作邀请站内信
func (s *CollabService) sendCollabInviteNotif(novelID, inviterUserID, targetUserID uint, role, token string, ttlMinutes int) {
	if s.notifSvc == nil {
		return
	}
	novelTitle := "未知项目"
	if n, err := s.novelRepo.GetByID(novelID); err == nil {
		novelTitle = n.Title
	}
	inviterName := "协作者"
	if u, err := s.userRepo.GetByID(inviterUserID); err == nil {
		inviterName = u.Nickname
		if inviterName == "" {
			inviterName = u.Username
		}
	}
	roleLabel := "浏览者"
	if role == "editor" {
		roleLabel = "编辑者"
	}
	title := fmt.Sprintf("%s 邀请你协作编辑《%s》", inviterName, novelTitle)
	body := fmt.Sprintf("你被邀请以【%s】身份加入《%s》的协作，邀请链接 %d 分钟内有效，请尽快接受。", roleLabel, novelTitle, ttlMinutes)
	linkPath := fmt.Sprintf("/collab/accept?token=%s", token)

	// 查询被邀请者所在的 Tenant
	targetTenantID := uint(0)
	if s.tenantUserRepo != nil {
		if tu, err := s.tenantUserRepo.GetFirstByUser(targetUserID); err == nil {
			targetTenantID = tu.TenantID
		}
	}
	_ = s.notifSvc.Send(targetTenantID, targetUserID, "collab_invite", title, body, "novel", novelID, linkPath)
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
