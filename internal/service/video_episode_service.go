package service

// video_episode_service.go
//
// "剧集列表"聚合查询：按章节汇总每一章对应的视频项目状态（剧本场次数 + 分镜视频完成度），
// 供前端一次性渲染全小说的 EP 卡片列表，避免逐章调用分场剧本/分镜接口造成 N+1。

import (
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
)

// EpisodeSceneBrief 剧集列表卡片里展示的单场剧本摘要（标题+一行概要，不含完整节拍）。
type EpisodeSceneBrief struct {
	SceneNo  int    `json:"scene_no"`
	Heading  string `json:"heading"`
	Synopsis string `json:"synopsis"`
}

// EpisodeSummary 一个章节（=一个视频项目）在剧集列表里展示所需的全部信息。
// Scenes 展示的是分场剧本的场次列表，不是章节大纲（Chapter.Summary 不用于此列表）。
type EpisodeSummary struct {
	ChapterID    uint                 `json:"chapter_id"`
	ChapterNo    int                  `json:"chapter_no"`
	Title        string               `json:"title"`
	VideoID      *uint                `json:"video_id,omitempty"` // nil = 本章尚未创建视频项目
	Duration     float64              `json:"duration"`
	Scenes       []EpisodeSceneBrief  `json:"scenes"`
	ShotsTotal   int                  `json:"shots_total"`      // 该章分镜总数
	ShotsWithVid int                  `json:"shots_with_video"` // 该章已生成视频的分镜数
}

// ListEpisodeSummaries 按章节顺序返回一部小说的剧集列表聚合数据。
// 内部只发 4 次查询（章节列表、视频列表、全小说分场剧本列表、分镜分组计数），
// 章节数量再多也不会随之增加请求数。
func (s *VideoService) ListEpisodeSummaries(novelID, tenantID uint) ([]*EpisodeSummary, error) {
	chapters, err := s.chapterRepo.ListByNovel(novelID)
	if err != nil {
		return nil, err
	}

	videos, err := s.videoRepo.ListAllByNovel(novelID, tenantID)
	if err != nil {
		return nil, err
	}
	videoByChapter := make(map[uint]*model.Video, len(videos))
	videoIDs := make([]uint, 0, len(videos))
	for _, v := range videos {
		if v.ChapterID != nil {
			videoByChapter[*v.ChapterID] = v
		}
		videoIDs = append(videoIDs, v.ID)
	}

	scenesByChapter := make(map[uint][]EpisodeSceneBrief, len(chapters))
	if s.screenplaySvc != nil {
		scenes, scErr := s.screenplaySvc.ListScenesByNovel(novelID)
		if scErr != nil {
			logger.Errorf("[VideoService] ListEpisodeSummaries: ListScenesByNovel novelID=%d: %v", novelID, scErr)
		}
		for _, sc := range scenes {
			scenesByChapter[sc.ChapterID] = append(scenesByChapter[sc.ChapterID], EpisodeSceneBrief{
				SceneNo: sc.SceneNo, Heading: sc.Heading, Synopsis: sc.Synopsis,
			})
		}
	}

	shotCounts, err := s.storyboardRepo.CountShotsGroupedByVideo(videoIDs)
	if err != nil {
		logger.Errorf("[VideoService] ListEpisodeSummaries: CountShotsGroupedByVideo novelID=%d: %v", novelID, err)
	}

	summaries := make([]*EpisodeSummary, 0, len(chapters))
	for _, ch := range chapters {
		summary := &EpisodeSummary{
			ChapterID: ch.ID,
			ChapterNo: ch.ChapterNo,
			Title:     ch.Title,
			Scenes:    scenesByChapter[ch.ID],
		}
		if v, ok := videoByChapter[ch.ID]; ok {
			vid := v.ID
			summary.VideoID = &vid
			summary.Duration = v.PublishMeta.Duration
			if counts, ok := shotCounts[v.ID]; ok {
				summary.ShotsTotal = counts.Total
				summary.ShotsWithVid = counts.WithVideo
			}
		}
		summaries = append(summaries, summary)
	}
	return summaries, nil
}
