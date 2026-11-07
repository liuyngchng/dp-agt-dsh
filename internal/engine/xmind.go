package engine

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// XMind 解析 — 将 .xmind 文件转换为 Markdown 文本，供记忆库使用。
//
// .xmind 本质是 ZIP 包，内含 content.xml（脑图结构）。
// 解析 content.xml 中的 <topic> 树，递归生成缩进 Markdown 列表。
// 仅依赖 Go 标准库 archive/zip + encoding/xml。

type xmindTopic struct {
	Title    string
	Children []*xmindTopic
}

// readContentXML 提取 ZIP 中 content.xml 的文本内容
func readContentXML(buf []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(buf), int64(len(buf)))
	if err != nil {
		return "", fmt.Errorf("无法读取 .xmind 文件（不是有效的 ZIP 包）")
	}
	for _, f := range zr.File {
		if f.Name == "content.xml" {
			rc, err := f.Open()
			if err != nil {
				return "", fmt.Errorf("无法读取 content.xml")
			}
			defer rc.Close()
			data, err := io.ReadAll(rc)
			if err != nil {
				return "", fmt.Errorf("无法读取 content.xml")
			}
			return string(data), nil
		}
		if f.Name == "content.json" {
			return "", fmt.Errorf("暂不支持 XMind 2020+ 的 content.json 格式，请另存为旧版 .xmind 或导出为 Markdown")
		}
	}
	return "", fmt.Errorf("无法在 .xmind 中找到 content.xml")
}

// parseXmindTopics 用 XML token 流解析 content.xml，构建 topic 树。
// 忽略 XML namespace（XMind 各版本默认 namespace 不同），只匹配元素 local name。
func parseXmindTopics(xmlContent string) ([]*xmindTopic, error) {
	dec := xml.NewDecoder(strings.NewReader(xmlContent))

	// 栈维护当前 topic 嵌套关系。栈底是根层 topic 列表，栈顶是当前正在构建的 topic。
	// 用哨兵节点收集根层（可能有多个顶层 topic）。
	roots := &xmindTopic{}
	stack := []*xmindTopic{roots}

	var inTitle bool
	var titleBuf strings.Builder

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("content.xml 解析失败，文件可能已损坏")
		}

		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "topic":
				child := &xmindTopic{}
				parent := stack[len(stack)-1]
				parent.Children = append(parent.Children, child)
				stack = append(stack, child)
			case "title":
				inTitle = true
				titleBuf.Reset()
			}
		case xml.CharData:
			if inTitle {
				titleBuf.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "title":
				inTitle = false
				if len(stack) > 0 {
					stack[len(stack)-1].Title = strings.TrimSpace(titleBuf.String())
				}
			case "topic":
				if len(stack) > 1 {
					stack = stack[:len(stack)-1]
				}
			}
		}
	}
	return roots.Children, nil
}

// findRootTopic 查找第一个带非空 title 的 topic（作为根主题）
func findRootTopic(topics []*xmindTopic) *xmindTopic {
	for _, t := range topics {
		if t.Title != "" {
			return t
		}
		if found := findRootTopic(t.Children); found != nil {
			return found
		}
	}
	return nil
}

// renderXmindTopic 递归将 topic 树渲染为缩进 Markdown 列表
func renderXmindTopic(topic *xmindTopic, depth int, lines *[]string) {
	if topic.Title != "" {
		*lines = append(*lines, strings.Repeat("  ", depth)+"- "+topic.Title)
	}
	for _, kid := range topic.Children {
		renderXmindTopic(kid, depth+1, lines)
	}
}

// XmindToMarkdown 将 .xmind 文件 buffer 解析为 Markdown 字符串。
// 失败时返回中文错误信息。
func XmindToMarkdown(buf []byte) (string, error) {
	xmlContent, err := readContentXML(buf)
	if err != nil {
		return "", err
	}

	topics, err := parseXmindTopics(xmlContent)
	if err != nil {
		return "", err
	}

	root := findRootTopic(topics)
	if root == nil {
		return "", fmt.Errorf("无法识别脑图结构，请确认是有效的 .xmind 文件")
	}

	var lines []string
	if root.Title != "" {
		lines = append(lines, "# "+root.Title)
		lines = append(lines, "")
	}
	// 根主题已作为标题，仅递归渲染其子节点，避免标题重复
	for _, kid := range root.Children {
		renderXmindTopic(kid, 0, &lines)
	}
	return strings.Join(lines, "\n"), nil
}
