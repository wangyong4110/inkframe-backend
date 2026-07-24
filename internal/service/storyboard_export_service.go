package service

// storyboard_export_service.go
//
// 导出"分镜脚本"（按分场剧本场次分组，每场下列出对应镜头）为 txt/markdown/docx，
// docx 复用 screenplay_export_service.go 里的 buildMinimalDocx，保持两个导出功能实现方式一致。

import (
	"fmt"
	"strings"

	"github.com/inkframe/inkframe-backend/internal/model"
)

const unassignedShotsHeading = "未分场镜头"

// groupShotsByScene 按 ScreenplaySceneID 把镜头分组到所属场次（组内保持调用方传入的原始顺序，
// 即 shot_no 升序）。返回值第二项是未归属任何现有场次的镜头（ScreenplaySceneID 为空，或指向的
// 场次已被删除——兼容分镜生成早于分场剧本改造前的旧数据）。
func groupShotsByScene(scenes []*model.ScreenplayScene, shots []*model.StoryboardShot) (map[uint][]*model.StoryboardShot, []*model.StoryboardShot) {
	sceneIDs := make(map[uint]bool, len(scenes))
	for _, sc := range scenes {
		sceneIDs[sc.ID] = true
	}
	bySceneID := make(map[uint][]*model.StoryboardShot)
	var unassigned []*model.StoryboardShot
	for _, sh := range shots {
		if sh.ScreenplaySceneID != nil && sceneIDs[*sh.ScreenplaySceneID] {
			bySceneID[*sh.ScreenplaySceneID] = append(bySceneID[*sh.ScreenplaySceneID], sh)
		} else {
			unassigned = append(unassigned, sh)
		}
	}
	return bySceneID, unassigned
}

// ExportStoryboardTXT 导出分镜脚本为纯文本，按剧本场次分组，每场下列出对应分镜。
func (s *ScreenplayService) ExportStoryboardTXT(title string, scenes []*model.ScreenplayScene, shots []*model.StoryboardShot) []byte {
	bySceneID, unassigned := groupShotsByScene(scenes, shots)

	var b strings.Builder
	b.WriteString(title + "\n")
	b.WriteString(strings.Repeat("=", len([]rune(title))) + "\n\n")

	writeSection := func(heading string, sceneShots []*model.StoryboardShot) {
		b.WriteString(heading + "\n")
		b.WriteString(strings.Repeat("-", len([]rune(heading))) + "\n\n")
		if len(sceneShots) == 0 {
			b.WriteString("（本场暂无分镜）\n\n")
			return
		}
		for _, sh := range sceneShots {
			fmt.Fprintf(&b, "镜%d（%.1fs）\n", sh.ShotNo, sh.Duration)
			if sh.Description != "" {
				b.WriteString("画面：" + sh.Description + "\n")
			}
			if narr := sh.Narration(); narr != "" {
				b.WriteString("旁白：" + narr + "\n")
			}
			if dial := sh.Dialogue(); dial != "" {
				b.WriteString("台词：" + dial + "\n")
			}
			b.WriteString("\n")
		}
	}
	for _, sc := range scenes {
		writeSection(sceneHeadingLine(sc), bySceneID[sc.ID])
	}
	if len(unassigned) > 0 {
		writeSection(unassignedShotsHeading, unassigned)
	}
	return []byte(b.String())
}

// ExportStoryboardMarkdown 导出分镜脚本为 Markdown，按剧本场次分组，每场下列出对应分镜。
func (s *ScreenplayService) ExportStoryboardMarkdown(title string, scenes []*model.ScreenplayScene, shots []*model.StoryboardShot) []byte {
	bySceneID, unassigned := groupShotsByScene(scenes, shots)

	var b strings.Builder
	b.WriteString("# " + title + "\n\n")

	writeSection := func(heading string, sceneShots []*model.StoryboardShot) {
		b.WriteString("## " + heading + "\n\n")
		if len(sceneShots) == 0 {
			b.WriteString("*（本场暂无分镜）*\n\n")
			return
		}
		for _, sh := range sceneShots {
			fmt.Fprintf(&b, "**镜 %d**（%.1fs）\n\n", sh.ShotNo, sh.Duration)
			if sh.Description != "" {
				b.WriteString("- 画面：" + sh.Description + "\n")
			}
			if narr := sh.Narration(); narr != "" {
				b.WriteString("- 旁白：" + narr + "\n")
			}
			if dial := sh.Dialogue(); dial != "" {
				b.WriteString("- 台词：" + dial + "\n")
			}
			b.WriteString("\n")
		}
	}
	for _, sc := range scenes {
		writeSection(sceneHeadingLine(sc), bySceneID[sc.ID])
	}
	if len(unassigned) > 0 {
		writeSection(unassignedShotsHeading, unassigned)
	}
	return []byte(b.String())
}

// ExportStoryboardDocx 导出分镜脚本为最小合法 docx，按剧本场次分组，每场下列出对应分镜。
func (s *ScreenplayService) ExportStoryboardDocx(title string, scenes []*model.ScreenplayScene, shots []*model.StoryboardShot) ([]byte, error) {
	bySceneID, unassigned := groupShotsByScene(scenes, shots)

	var body strings.Builder
	body.WriteString(docxParagraph(title, true, 36)) // 标题：加粗 18pt
	body.WriteString("<w:p/>")

	writeSection := func(heading string, sceneShots []*model.StoryboardShot) {
		body.WriteString(docxParagraph(heading, true, 28)) // 场次标题：加粗 14pt
		if len(sceneShots) == 0 {
			body.WriteString(docxParagraph("（本场暂无分镜）", false, 0))
			body.WriteString("<w:p/>")
			return
		}
		for _, sh := range sceneShots {
			body.WriteString(docxParagraph(fmt.Sprintf("镜%d（%.1fs）", sh.ShotNo, sh.Duration), true, 0))
			if sh.Description != "" {
				body.WriteString(docxParagraph("画面："+sh.Description, false, 0))
			}
			if narr := sh.Narration(); narr != "" {
				body.WriteString(docxParagraph("旁白："+narr, false, 0))
			}
			if dial := sh.Dialogue(); dial != "" {
				body.WriteString(docxParagraph("台词："+dial, false, 0))
			}
		}
		body.WriteString("<w:p/>")
	}
	for _, sc := range scenes {
		writeSection(sceneHeadingLine(sc), bySceneID[sc.ID])
	}
	if len(unassigned) > 0 {
		writeSection(unassignedShotsHeading, unassigned)
	}

	documentXML := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` +
		body.String() + `<w:sectPr/></w:body></w:document>`
	return buildMinimalDocx(documentXML)
}
