package rag

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Visioner 把一张图片变成可检索的文本。
//
// 为什么必须有它：bge-m3 是**文本**嵌入模型，图片本身进不了向量空间。
// 先让视觉模型把图片“翻译”成描述 + OCR 文字，图片才能和文本走同一条检索链 ——
// 这是本方案能叫“多模态知识库”的关键一步，也是最大的质量瓶颈：
// 视觉模型没看出来的东西，检索永远找不到。
type Visioner interface {
	Describe(ctx context.Context, img []byte) (string, error)
}

type ollamaVision struct {
	cfg    OllamaConfig
	client *http.Client
}

func NewOllamaVision(cfg OllamaConfig) Visioner {
	cfg = cfg.withDefaults()
	return &ollamaVision{cfg: cfg, client: &http.Client{Timeout: cfg.Timeout}}
}

// VisionPrompt 是喂给视觉模型的固定指令。
// 分成“描述”与“逐字拄录”两段是有意的：描述负责语义检索，
// OCR 文字负责精确关键词命中，少任何一段都会漏检。
const VisionPrompt = `你是知识库的图片索引器。请仔细观察这张图，用中文输出两部分：
【描述】客观描述画面：主体、场景、颜色、结构、图表趋势。不要评价、不要客套。
【文字】逐字拄录图中所有可见文字，包括标题、标签、表格、代码。没有文字就写“无”。
只输出这两部分。`

type ollamaChatResponse struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	Error string `json:"error"`
}

func (v *ollamaVision) Describe(ctx context.Context, img []byte) (string, error) {
	if len(img) == 0 {
		return "", fmt.Errorf("图片内容为空")
	}
	body, err := json.Marshal(map[string]any{
		"model":  v.cfg.VisionModel,
		"stream": false,
		"messages": []map[string]any{
			{
				"role":    "user",
				"content": VisionPrompt,
				"images":  []string{base64.StdEncoding.EncodeToString(img)},
			},
		},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.cfg.BaseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := v.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("连接本地视觉模型失败（%s）：%w", v.cfg.BaseURL, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("视觉模型返回 %d：%s", resp.StatusCode, clipText(string(raw), 200))
	}

	var out ollamaChatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("解析视觉模型响应失败：%w", err)
	}
	if out.Error != "" {
		return "", fmt.Errorf("视觉模型报错：%s", out.Error)
	}
	desc := out.Message.Content
	if desc == "" {
		return "", fmt.Errorf("视觉模型返回了空描述")
	}
	return desc, nil
}
