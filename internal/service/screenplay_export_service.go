package service

// screenplay_export_service.go
//
// 导出分场剧本为 txt/markdown/docx。项目里其它需要"文档格式"的导出（FCPXML、CapCut 草稿）
// 一贯选择手写 XML/JSON 自行组装，不引入重量级第三方库；docx 本质是一个包含固定 XML 结构的
// zip 包，用标准库 archive/zip + encoding/xml.EscapeText 就能手工拼出一个能被 Word/WPS/
// Pages 正常打开的最小合法 docx，不需要新增依赖。

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"

	"github.com/inkframe/inkframe-backend/internal/model"
)

func sceneHeadingLine(sc *model.ScreenplayScene) string {
	return fmt.Sprintf("第%d场 %s", sc.SceneNo, sc.Heading)
}

// ExportScenesTXT 导出为纯文本。
func (s *ScreenplayService) ExportScenesTXT(title string, scenes []*model.ScreenplayScene) []byte {
	var b strings.Builder
	b.WriteString(title + "\n")
	b.WriteString(strings.Repeat("=", len([]rune(title))) + "\n\n")
	for _, sc := range scenes {
		heading := sceneHeadingLine(sc)
		b.WriteString(heading + "\n")
		b.WriteString(strings.Repeat("-", len([]rune(heading))) + "\n\n")
		if sc.Synopsis != "" {
			b.WriteString(sc.Synopsis + "\n\n")
		}
		if beats := strings.TrimSpace(sc.Beats); beats != "" {
			b.WriteString(beats + "\n\n")
		}
	}
	return []byte(b.String())
}

// ExportScenesMarkdown 导出为 Markdown，对话行的说话人加粗以提高可读性。
func (s *ScreenplayService) ExportScenesMarkdown(title string, scenes []*model.ScreenplayScene) []byte {
	var b strings.Builder
	b.WriteString("# " + title + "\n\n")
	for _, sc := range scenes {
		b.WriteString(fmt.Sprintf("## %s\n\n", sceneHeadingLine(sc)))
		if sc.Synopsis != "" {
			b.WriteString("> " + sc.Synopsis + "\n\n")
		}
		for _, line := range strings.Split(strings.TrimSpace(sc.Beats), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if m := beatDialogueLineRe.FindStringSubmatch(line); m != nil {
				b.WriteString(fmt.Sprintf("**%s**：%s\n\n", strings.TrimSpace(m[1]), strings.TrimSpace(m[2])))
			} else {
				b.WriteString(line + "\n\n")
			}
		}
	}
	return []byte(b.String())
}

// ─── docx（最小合法 WordprocessingML + zip）───────────────────────────────────

const docxContentTypesXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
  <Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
  <Default Extension="xml" ContentType="application/xml"/>
  <Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>
</Types>`

const docxRelsXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>
</Relationships>`

// docxXMLEscape 转义将要放进 <w:t> 里的文本内容（&/</>/引号等），避免破坏 XML 结构。
func docxXMLEscape(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

// docxParagraph 生成一个 <w:p> 段落；bold 为 true 或 sizeHalfPoints>0 时附加 run 属性
// （sizeHalfPoints 是 OOXML 的半磅字号单位，28=14pt，标准正文字号不传即可）。
func docxParagraph(text string, bold bool, sizeHalfPoints int) string {
	var rPr strings.Builder
	if bold || sizeHalfPoints > 0 {
		rPr.WriteString("<w:rPr>")
		if bold {
			rPr.WriteString("<w:b/>")
		}
		if sizeHalfPoints > 0 {
			fmt.Fprintf(&rPr, `<w:sz w:val="%d"/><w:szCs w:val="%d"/>`, sizeHalfPoints, sizeHalfPoints)
		}
		rPr.WriteString("</w:rPr>")
	}
	return fmt.Sprintf(`<w:p><w:r>%s<w:t xml:space="preserve">%s</w:t></w:r></w:p>`, rPr.String(), docxXMLEscape(text))
}

// ExportScenesDocx 导出为最小合法 docx（zip 包含 [Content_Types].xml + _rels/.rels +
// word/document.xml 三个部分——这是 Word/WPS/Pages 能正常打开一个 docx 所需的最小集合）。
func (s *ScreenplayService) ExportScenesDocx(title string, scenes []*model.ScreenplayScene) ([]byte, error) {
	var body strings.Builder
	body.WriteString(docxParagraph(title, true, 36)) // 标题：加粗 18pt
	body.WriteString("<w:p/>")
	for _, sc := range scenes {
		body.WriteString(docxParagraph(sceneHeadingLine(sc), true, 28)) // 场次标题：加粗 14pt
		if sc.Synopsis != "" {
			body.WriteString(docxParagraph(sc.Synopsis, false, 0))
		}
		body.WriteString("<w:p/>")
		for _, line := range strings.Split(strings.TrimSpace(sc.Beats), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			body.WriteString(docxParagraph(line, false, 0))
		}
		body.WriteString("<w:p/>")
	}

	documentXML := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` +
		body.String() + `<w:sectPr/></w:body></w:document>`

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// 固定写入顺序，保证同样输入产出确定性的字节流，方便测试对比。
	entries := []struct{ name, content string }{
		{"[Content_Types].xml", docxContentTypesXML},
		{"_rels/.rels", docxRelsXML},
		{"word/document.xml", documentXML},
	}
	for _, e := range entries {
		w, err := zw.Create(e.name)
		if err != nil {
			return nil, fmt.Errorf("create docx zip entry %s: %w", e.name, err)
		}
		if _, err := w.Write([]byte(e.content)); err != nil {
			return nil, fmt.Errorf("write docx zip entry %s: %w", e.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("close docx zip: %w", err)
	}
	return buf.Bytes(), nil
}
