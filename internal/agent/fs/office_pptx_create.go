package fs

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 往 PPTX 骨架里注入幻灯片。
//
// 骨架来自 officecli 的 `create`（母版/版式/主题齐全），我们只补三处 + 两个新部件：
//  1. ppt/presentation.xml 的 <p:sldIdLst> 里加 sldId；
//  2. ppt/_rels/presentation.xml.rels 里加 slide 关系（Id 从 rId10 起，避开它已用的 rId1/rId2）；
//  3. [Content_Types].xml 里加 slide 的 Override；
//  4. 新增 ppt/slides/slideN.xml 与它自己的 .rels（指向 slideLayout1）。
//
// 为什么不调 officecli 的 `add slide`：实测这个版本报成功却不落盘（zip 里没有 slide1.xml），
// 而它生成的骨架是好的。所以骨架用它、内容我们自己写。

// SlideInput 是新建 pptx 的一页。
type SlideInput struct {
	Title   string
	Bullets []string
}

const (
	nsA = "http://schemas.openxmlformats.org/drawingml/2006/main"
	nsP = "http://schemas.openxmlformats.org/presentationml/2006/main"
	nsR = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
)

// InjectPptxSlides 把页面注入骨架，返回新的 pptx 字节。
func InjectPptxSlides(skeleton []byte, slides []SlideInput) ([]byte, error) {
	if len(slides) == 0 {
		return skeleton, nil
	}
	zr, err := zip.NewReader(bytes.NewReader(skeleton), int64(len(skeleton)))
	if err != nil {
		return nil, fmt.Errorf("pptx 骨架打不开: %w", err)
	}

	pres, ok := readZipEntry(zr, "ppt/presentation.xml")
	if !ok {
		return nil, fmt.Errorf("骨架里没有 ppt/presentation.xml")
	}
	rels, ok := readZipEntry(zr, "ppt/_rels/presentation.xml.rels")
	if !ok {
		return nil, fmt.Errorf("骨架里没有 ppt/_rels/presentation.xml.rels")
	}
	ct, ok := readZipEntry(zr, "[Content_Types].xml")
	if !ok {
		return nil, fmt.Errorf("骨架里没有 [Content_Types].xml")
	}

	var sldIds, newRels, newTypes strings.Builder
	extra := map[string]string{}
	order := []string{}

	for i, s := range slides {
		n := i + 1
		rid := fmt.Sprintf("rId%d", 10+i)
		sldIds.WriteString(fmt.Sprintf(
			`<p:sldId id="%d" xmlns:r="%s" r:id="%s"/>`, 256+i, nsR, rid))
		newRels.WriteString(fmt.Sprintf(
			`<Relationship Id="%s" Type="%s/slide" Target="/ppt/slides/slide%d.xml"/>`, rid, nsR, n))
		newTypes.WriteString(fmt.Sprintf(
			`<Override PartName="/ppt/slides/slide%d.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slide+xml"/>`, n))

		name := fmt.Sprintf("ppt/slides/slide%d.xml", n)
		extra[name] = slideXML(s)
		order = append(order, name)
		relName := fmt.Sprintf("ppt/slides/_rels/slide%d.xml.rels", n)
		extra[relName] = slideRelsXML()
		order = append(order, relName)
	}

	// 1) sldIdLst：骨架里是自闭合的 <p:sldIdLst />，替换成带内容的。
	newPres, ok := insertSldIds(string(pres), sldIds.String())
	if !ok {
		return nil, fmt.Errorf("骨架的 presentation.xml 结构不符合预期（找不到 <p:sldIdLst/>）")
	}
	// 2)(3) 关系与内容类型：在收尾标签前追加。
	newRelsXML := strings.Replace(string(rels), "</Relationships>", newRels.String()+"</Relationships>", 1)
	newTypesXML := strings.Replace(string(ct), "</Types>", newTypes.String()+"</Types>", 1)

	replaced := map[string]string{
		"ppt/presentation.xml":            newPres,
		"ppt/_rels/presentation.xml.rels": newRelsXML,
		"[Content_Types].xml":             newTypesXML,
	}

	// 重打包：原有部件按原样复制（被替换的用新内容），再补上新增的 slide 部件。
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range zr.File {
		content := []byte(nil)
		if repl, ok := replaced[f.Name]; ok {
			content = []byte(repl)
			delete(replaced, f.Name)
		} else {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			c := new(bytes.Buffer)
			if _, err := c.ReadFrom(rc); err != nil {
				rc.Close()
				return nil, err
			}
			rc.Close()
			content = c.Bytes()
		}
		w, err := zw.Create(f.Name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(content); err != nil {
			return nil, err
		}
	}
	// 剩下的（骨架里本来没有的）就是新增部件。
	for _, name := range order {
		w, err := zw.Create(name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write([]byte(extra[name])); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// insertSldIds 把自闭合的 sldIdLst 换成带内容的；已带内容则在其中追加。
func insertSldIds(pres, inner string) (string, bool) {
	for _, selfClosed := range []string{"<p:sldIdLst />", "<p:sldIdLst/>"} {
		if strings.Contains(pres, selfClosed) {
			return strings.Replace(pres, selfClosed, "<p:sldIdLst>"+inner+"</p:sldIdLst>", 1), true
		}
	}
	if strings.Contains(pres, "</p:sldIdLst>") {
		return strings.Replace(pres, "</p:sldIdLst>", inner+"</p:sldIdLst>", 1), true
	}
	return pres, false
}

// slideXML 生成一页幻灯片：标题 + 正文项目符号。
func slideXML(s SlideInput) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>`)
	b.WriteString(fmt.Sprintf(`<p:sld xmlns:a="%s" xmlns:r="%s" xmlns:p="%s"><p:cSld><p:spTree>`, nsA, nsR, nsP))
	b.WriteString(`<p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr/>`)
	if strings.TrimSpace(s.Title) != "" {
		b.WriteString(pptxTextBox(2, "Title 1", s.Title, 838200, 365125, 10515600, 1325563, 2400, true))
	}
	if len(s.Bullets) > 0 {
		b.WriteString(pptxTextBox(3, "Content Placeholder 1", strings.Join(s.Bullets, "\n"),
			838200, 1825625, 10515600, 4351338, 1600, false))
	}
	b.WriteString(`</p:spTree></p:cSld><p:clrMapOvr><a:masterClrMapping/></p:clrMapOvr></p:sld>`)
	return b.String()
}

// pptxTextBox 生成一个文本框，每行一个段落。
func pptxTextBox(id int, name, text string, x, y, cx, cy, sizePt int, bold bool) string {
	var paras strings.Builder
	for _, line := range strings.Split(text, "\n") {
		boldAttr := ""
		if bold {
			boldAttr = ` b="1"`
		}
		fmt.Fprintf(&paras,
			`<a:p><a:r><a:rPr lang="zh-CN" sz="%d"%s dirty="0"/><a:t>%s</a:t></a:r></a:p>`,
			sizePt*100, boldAttr, xmlEscape(line))
	}
	return fmt.Sprintf(
		`<p:sp><p:nvSpPr><p:cNvPr id="%d" name="%s"/><p:cNvSpPr txBox="1"/><p:nvPr/></p:nvSpPr>`+
			`<p:spPr><a:xfrm><a:off x="%d" y="%d"/><a:ext cx="%d" cy="%d"/></a:xfrm>`+
			`<a:prstGeom prst="rect"><a:avLst/></a:prstGeom></p:spPr>`+
			`<p:txBody><a:bodyPr wrap="square"/><a:lstStyle/>%s</p:txBody></p:sp>`,
		id, name, x, y, cx, cy, paras.String())
}

// slideRelsXML 是新幻灯片的部件关系：指向骨架里的第一个版式。
// createPptxWithOfficeCLI 两步走：OfficeCLI 出**合法骨架**，我们再注入幻灯片内容。
func createPptxWithOfficeCLI(a officeArgs) ([]byte, error) {
	tmp, err := os.MkdirTemp("", "blogmy-pptx-")
	if err != nil {
		return nil, fmt.Errorf("创建临时目录失败: %w", err)
	}
	defer os.RemoveAll(tmp)

	skeletonPath := filepath.Join(tmp, "skeleton.pptx")
	if err := OfficeCLICreate(context.Background(), skeletonPath); err != nil {
		return nil, fmt.Errorf("调用 OfficeCLI 生成骨架失败: %w", err)
	}
	skeleton, err := os.ReadFile(skeletonPath)
	if err != nil {
		return nil, fmt.Errorf("读取骨架失败: %w", err)
	}

	var slides []SlideInput
	for _, s := range a.Slides {
		title, _ := s["title"].(string)
		var bullets []string
		if raw, ok := s["bullets"].([]any); ok {
			for _, b := range raw {
				bullets = append(bullets, fmt.Sprint(b))
			}
		}
		slides = append(slides, SlideInput{Title: title, Bullets: bullets})
	}
	if len(slides) == 0 {
		slides = []SlideInput{{Title: a.Title}}
	}
	return InjectPptxSlides(skeleton, slides)
}

func slideRelsXML() string {
	return `<?xml version="1.0" encoding="utf-8"?>` +
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		`<Relationship Id="rId1" Type="` + nsR + `/slideLayout" Target="../slideLayouts/slideLayout1.xml"/>` +
		`</Relationships>`
}
