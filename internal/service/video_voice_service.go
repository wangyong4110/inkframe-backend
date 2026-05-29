package service

import (
	"time"

	"github.com/inkframe/inkframe-backend/internal/model"
)

// charListEntry is a TTL-bounded cache entry for ListByNovel results.
type charListEntry struct {
	chars     []*model.Character
	expiresAt time.Time
}

// startCharListCacheCleanup 启动 charListCache 的后台定期清理（每 5 分钟扫描一次，删除已过期条目）。
// 应在 VideoService 构造时调用一次，防止长期运行后缓存无限膨胀。
func (s *VideoService) startCharListCacheCleanup() {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				now := time.Now()
				s.charListCache.Range(func(k, v interface{}) bool {
					if entry, ok := v.(*charListEntry); ok && now.After(entry.expiresAt) {
						s.charListCache.Delete(k)
					}
					return true
				})
			case <-s.stopCh:
				return
			}
		}
	}()
}

// listCharsByNovelCached returns the character list for a novel, using a 60-second
// in-process cache to avoid repeated DB calls during batch voice generation.
func (s *VideoService) listCharsByNovelCached(novelID uint) ([]*model.Character, error) {
	if v, ok := s.charListCache.Load(novelID); ok {
		if entry := v.(*charListEntry); time.Now().Before(entry.expiresAt) {
			return entry.chars, nil
		}
	}
	chars, err := s.characterRepo.ListByNovel(novelID)
	if err != nil {
		return nil, err
	}
	s.charListCache.Store(novelID, &charListEntry{
		chars:     chars,
		expiresAt: time.Now().Add(60 * time.Second),
	})
	return chars, nil
}
