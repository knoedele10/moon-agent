package web

import (
	"os"
	"sync"

	"blog-my/internal/model"
	"blog-my/internal/rag"
)

// kbMaxUploadBytes 限制单个上传文件大小。
// 20MB 对长文与截图都够用，又能避免一个巨大请求体把内存吃光。
const kbMaxUploadBytes = 20 << 20

var (
	kbOnce sync.Once
	kbIng  *rag.Ingester
)

// kb 懒加载知识库实例：真正用到时才连本地模型，
// 这样没装 / 没启 Ollama 的机器上服务依旧能正常启动与登录。
func kb() *rag.Ingester {
	kbOnce.Do(func() {
		cfg := rag.OllamaConfig{
			BaseURL:     os.Getenv("OLLAMA_BASE_URL"),
			EmbedModel:  os.Getenv("KB_EMBED_MODEL"),
			VisionModel: os.Getenv("KB_VISION_MODEL"),
		}
		kbIng = rag.NewIngester(rag.NewStore(model.DB), rag.NewOllamaEmbedder(cfg), rag.NewOllamaVision(cfg))
	})
	return kbIng
}
