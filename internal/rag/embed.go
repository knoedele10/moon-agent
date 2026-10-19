package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// OllamaConfig 指向本地 Ollama 服务。嵌入与视觉共用同一个服务，只是模型不同。
type OllamaConfig struct {
	BaseURL     string
	EmbedModel  string
	VisionModel string
	Timeout     time.Duration
}

func (c OllamaConfig) withDefaults() OllamaConfig {
	if c.BaseURL == "" {
		c.BaseURL = "http://127.0.0.1:11434"
	}
	if c.EmbedModel == "" {
		c.EmbedModel = "bge-m3:latest"
	}
	if c.VisionModel == "" {
		c.VisionModel = "qwen2.5vl:7b"
	}
	// 视觉模型在只跑 CPU/单张显卡时可能很慢，超时给得比普通对话宽松得多。
	if c.Timeout <= 0 {
		c.Timeout = 5 * time.Minute
	}
	return c
}

// Embedder 把一批文本转成向量。
// 入参是数组是有意的：一篇文档切完块后一次请求全部嵌入，比逐块往返快得多。
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Dim 返回向量维度。首次调用会实际探测一次，避免把维度写死在常量里
	// （换嵌入模型时维度会变，写死就会静默算错相似度）。
	Dim(ctx context.Context) (int, error)
}

type ollamaEmbedder struct {
	cfg    OllamaConfig
	client *http.Client
}

func NewOllamaEmbedder(cfg OllamaConfig) Embedder {
	cfg = cfg.withDefaults()
	return &ollamaEmbedder{cfg: cfg, client: &http.Client{Timeout: cfg.Timeout}}
}

type ollamaEmbedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
	Error      string      `json:"error"`
}

func (e *ollamaEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(map[string]any{"model": e.cfg.EmbedModel, "input": texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.BaseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接本地嵌入模型失败（%s）：%w", e.cfg.BaseURL, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("嵌入模型返回 %d：%s", resp.StatusCode, clipText(string(raw), 200))
	}

	var out ollamaEmbedResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("解析嵌入响应失败：%w", err)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("嵌入模型报错：%s", out.Error)
	}
	if len(out.Embeddings) != len(texts) {
		return nil, fmt.Errorf("嵌入数量不匹配：请求 %d 条，返回 %d 条", len(texts), len(out.Embeddings))
	}
	return out.Embeddings, nil
}

func (e *ollamaEmbedder) Dim(ctx context.Context) (int, error) {
	vecs, err := e.Embed(ctx, []string{"维度探测"})
	if err != nil {
		return 0, err
	}
	if len(vecs) == 0 || len(vecs[0]) == 0 {
		return 0, fmt.Errorf("嵌入模型返回了空向量")
	}
	return len(vecs[0]), nil
}

// clipText 按 rune 截断，避免把中文截成半个字。
func clipText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
