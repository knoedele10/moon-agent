package web

// CUA 的模型代理（契约 docs/cua/06-integration-contract.md 第 3 节）。
//
// CUA 子进程没有 session cookie，所以这条路由不挂 authRequired，
// 改用「每 run 一次性 Bearer token」：token 由 Go 生成，经 CUA_MODEL_API_KEY
// 传给子进程，子进程把它当 OpenAI api_key 用。
//
// 代理把 OpenAI chat completions 的 messages 映射到既有 llm registry：
// content 为数组时，image_url 的 data URI 会被解出 MIME 与原始字节，
// 落到 llm.Message.Images（见 llm/provider.go:33-46）。

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"unicode"

	"blog-my/internal/llm"

	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/gin-gonic/gin"
	goopenai "github.com/meguminnnnnnnnn/go-openai"
)

// cuaMinMaxTokens 是模型代理的 max_tokens 下限（契约 3.1，2026-10-11 二次裁定）。
//
// 4096 不够用：真实桌面任务在 step 12 死于 E_MODEL_EMPTY，轨迹显示当时正在正常推进
// （记事本已打开、文字已输入、正在处理保存），并非模型跑偏。高保真复测（CUA 真实系统
// 提示词 + 真实 1472x920 截图，prompt 5299 tok）三次调用同一个 prompt：
//
//	max_tokens=4096  -> completion=414
//	max_tokens=8192  -> completion=195
//	max_tokens=16384 -> completion=1339
//
// completion 在 195~1339 之间波动，长推理步会把 4096 吃光并返回空 content。
// CUA 默认只发 1024，所以这里统一抬到不低于 16384。
const cuaMinMaxTokens = 16384

// cuaResolveKey 是 resolveKey 的间接层：生产路径就是 resolveKey 本身，
// 单元测试可替换掉它，从而不必为了跑一次代理而连数据库。
var cuaResolveKey = resolveKey

// cuaEmptyContentMessage 是空回复的错误文案（契约 3.1：空 content 必须当错误，不能当合法回复）。
// 文案里的 E_MODEL_EMPTY 是给 CUA 侧识别的稳定标记，错误 JSON 形状仍是 {"error":{"message":"..."}}。
const cuaEmptyContentMessage = "模型返回空 content（E_MODEL_EMPTY）：多为 max_tokens 过小或上游截断，代理已按不低于 16384 兜底"

// cuaApplyUpstreamOverride 把模型代理的上游指到测试桩（默认关闭，只在设了环境变量时生效）。
//
//	CUA_LLM_UPSTREAM=http://127.0.0.1:8399/v1   # 上游 base_url（原样使用，OpenAI 兼容服务自带 /v1）
//	CUA_LLM_ENDPOINT_OVERRIDE=deepseek                    # 要覆盖哪个端点，默认 deepseek
//
// 用于 docs/verify 的桩验证（data URI → llm.Message.Images、空 content 必须报错）。
func cuaApplyUpstreamOverride(reg *llm.Registry) {
	base := strings.TrimSpace(os.Getenv("CUA_LLM_UPSTREAM"))
	if base == "" || reg == nil {
		return
	}
	target := strings.TrimSpace(os.Getenv("CUA_LLM_ENDPOINT_OVERRIDE"))
	if target == "" {
		target = "deepseek"
	}
	eps := reg.Endpoints()
	hit := false
	for i := range eps {
		if eps[i].ID == target {
			eps[i].BaseURL = base
			hit = true
		}
	}
	if !hit {
		log.Printf("cua: CUA_LLM_UPSTREAM 想覆盖端点 %q，但注册表里没有它", target)
		return
	}
	reg.SetEndpoints(eps)
	log.Printf("cua: 模型代理上游已覆盖 → endpoint=%s base_url=%s", target, base)
}

// cuaLLMProxy 处理 POST /internal/cua/llm/v1/chat/completions（非流式）。
func cuaLLMProxy(c *gin.Context) {
	if registry == nil {
		cuaLLMFail(c, http.StatusServiceUnavailable, "模型注册表未初始化")
		return
	}

	token := cuaBearerToken(c.GetHeader("Authorization"))
	if token == "" {
		cuaLLMFail(c, http.StatusUnauthorized, "缺少 Bearer token")
		return
	}
	run := cuaMgr.findByToken(token)
	if run == nil {
		cuaLLMFail(c, http.StatusUnauthorized, "无效的 CUA 模型代理 token")
		return
	}

	var body struct {
		Model       string             `json:"model"`
		Messages    []cuaOpenAIMessage `json:"messages"`
		Temperature *float64           `json:"temperature"`
		MaxTokens   int                `json:"max_tokens"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		cuaLLMFail(c, http.StatusBadRequest, "请求体不是合法的 OpenAI chat completions JSON："+err.Error())
		return
	}

	modelID := strings.TrimSpace(body.Model)
	if modelID == "" {
		cuaLLMFail(c, http.StatusBadRequest, "model 不能为空")
		return
	}
	ep, ok := registry.FindByModel(modelID)
	if !ok {
		cuaLLMFail(c, http.StatusBadRequest, "未知模型："+modelID)
		return
	}

	messages, err := cuaConvertMessages(body.Messages)
	if err != nil {
		cuaLLMFail(c, http.StatusBadRequest, err.Error())
		return
	}

	req := llm.ChatRequest{Messages: messages}
	if body.Temperature != nil {
		req.Temperature = *body.Temperature
	}
	// 契约 3.1：不得设置过小的 max_tokens，否则上游会返回空 content。
	req.MaxTokens = cuaMinMaxTokens
	if body.MaxTokens > cuaMinMaxTokens {
		req.MaxTokens = body.MaxTokens
	} else if body.MaxTokens > 0 && body.MaxTokens < cuaMinMaxTokens {
		log.Printf("cua: max_tokens %d 过小，代理抬到 %d（契约 3.1）", body.MaxTokens, cuaMinMaxTokens)
	}

	// 用 run 创建者的 key：用户自填优先，回落服务端环境变量。
	key := cuaResolveKey(run.UserID, ep)
	// 非流式：契约 §3 的代理是「请求非流式 + 响应非流式」，
	// 走 registry.Generate 也让上游不必支持 SSE（普通 JSON 响应即可）。
	content, err := registry.Generate(c.Request.Context(), modelID, key, req)
	if err != nil {
		cuaUpstreamFail(c, err)
		return
	}
	if strings.TrimSpace(content) == "" {
		// 契约 3.1：空 content 当错误，绝不返回给 CUA 当合法回复。
		log.Printf("cua: run %s 的模型 %s 返回空 content", run.ID, modelID)
		cuaLLMFail(c, http.StatusBadGateway, cuaEmptyContentMessage)
		return
	}
	c.JSON(http.StatusOK, gin.H{"choices": []gin.H{{
		"message": gin.H{"role": llm.RoleAssistant, "content": content},
	}}})
}

func cuaLLMFail(c *gin.Context, status int, msg string) {
	c.JSON(status, gin.H{"error": gin.H{"message": msg}})
}

// cuaUpstreamFail 把上游的真实原因透出来（契约 3）：
// 不能再出现「模型服务返回 HTTP 400」这种把上下文超限说成 key/模型名问题的误导。
func cuaUpstreamFail(c *gin.Context, err error) {
	status, msg := cuaUpstreamErrorInfo(err)
	cuaLLMFail(c, status, msg)
}

func cuaUpstreamErrorInfo(err error) (int, string) {
	if err == nil {
		return http.StatusBadGateway, "上游模型服务返回未知错误"
	}
	switch {
	case errors.Is(err, llm.ErrNoEndpoint):
		return http.StatusBadRequest, "未知模型：" + err.Error()
	case errors.Is(err, llm.ErrMissingKey):
		return http.StatusBadRequest, "该模型没有可用的 API Key（用户未填，服务端环境变量也为空）"
	case errors.Is(err, llm.ErrNoBackend):
		return http.StatusBadGateway, "该模型的后端未注册：" + err.Error()
	}

	// 注意：Eino 的 openai 组件会把底层的 *go-openai.APIError 转成**组件自己的**
	// APIError（eino-ext components/model/openai types.go:convOrigAPIError），
	// 那个类型不 Unwrap 回底层类型，所以必须先匹配组件类型。
	var einoErr *einoopenai.APIError
	if errors.As(err, &einoErr) {
		msg := strings.TrimSpace(einoErr.Message)
		if msg == "" {
			msg = einoErr.Error()
		}
		return cuaHTTPStatusOrBadGateway(einoErr.HTTPStatusCode), msg
	}

	// 兜底：底层 go-openai 的错误直接漏出来时（其他后端/其他调用路径）也认。
	var apiErr *goopenai.APIError
	if errors.As(err, &apiErr) {
		msg := strings.TrimSpace(apiErr.Message)
		if msg == "" {
			msg = apiErr.Error()
		}
		return cuaHTTPStatusOrBadGateway(apiErr.HTTPStatusCode), msg
	}
	var reqErr *goopenai.RequestError
	if errors.As(err, &reqErr) {
		if m := cuaErrorMessageFromBody(reqErr.Body); m != "" {
			return cuaHTTPStatusOrBadGateway(reqErr.HTTPStatusCode), m
		}
		return cuaHTTPStatusOrBadGateway(reqErr.HTTPStatusCode), reqErr.Error()
	}
	return http.StatusBadGateway, err.Error()
}

func cuaHTTPStatusOrBadGateway(status int) int {
	if status < 400 || status > 599 {
		return http.StatusBadGateway
	}
	return status
}

// cuaErrorMessageFromBody 从上游错误 body 里取 {"error":{"message":"..."}} 或 {"message":"..."}。
func cuaErrorMessageFromBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ""
	}
	if s := strings.TrimSpace(envelope.Error.Message); s != "" {
		return s
	}
	return strings.TrimSpace(envelope.Message)
}

// cuaBearerToken 解析 "Bearer xxx"，大小写不敏感。
func cuaBearerToken(header string) string {
	header = strings.TrimSpace(header)
	const prefix = "Bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

type cuaContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

type cuaOpenAIMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// cuaConvertMessages 把 OpenAI 的 messages 映射成 llm.Message。
// content 是字符串就直接用；是数组就按 part 拆文字与图片。
func cuaConvertMessages(in []cuaOpenAIMessage) ([]llm.Message, error) {
	out := make([]llm.Message, 0, len(in))
	for i, m := range in {
		role := m.Role
		if role == "" {
			role = llm.RoleUser
		}
		msg := llm.Message{Role: role}

		raw := strings.TrimSpace(string(m.Content))
		if raw == "" || raw == "null" {
			out = append(out, msg)
			continue
		}
		var text string
		if err := json.Unmarshal(m.Content, &text); err == nil {
			msg.Content = text
			out = append(out, msg)
			continue
		}

		var parts []cuaContentPart
		if err := json.Unmarshal(m.Content, &parts); err != nil {
			return nil, fmt.Errorf("messages[%d].content 既不是字符串也不是数组", i)
		}
		texts := make([]string, 0, len(parts))
		for _, p := range parts {
			switch p.Type {
			case "text":
				if p.Text != "" {
					texts = append(texts, p.Text)
				}
			case "image_url":
				if p.ImageURL == nil || p.ImageURL.URL == "" {
					continue
				}
				mime, data, err := cuaDecodeDataURI(p.ImageURL.URL)
				if err != nil {
					return nil, fmt.Errorf("messages[%d] 的图片：%w", i, err)
				}
				msg.Images = append(msg.Images, llm.Image{MIME: mime, Data: data})
			default:
				// 未知 part（audio / refusal 等）忽略，不阻断整次调用。
			}
		}
		msg.Content = strings.Join(texts, "\n")
		out = append(out, msg)
	}
	return out, nil
}

// cuaDecodeDataURI 解析 data:image/png;base64,xxx，返回 MIME 与原始字节。
// 只支持 data URI：代理不代下载外链图片（本地 Ollama 也只吃内联字节）。
func cuaDecodeDataURI(uri string) (string, []byte, error) {
	uri = strings.TrimSpace(uri)
	if !strings.HasPrefix(uri, "data:") {
		return "", nil, errors.New("只支持 data: URI 图片")
	}
	rest := uri[len("data:"):]
	comma := strings.Index(rest, ",")
	if comma < 0 {
		return "", nil, errors.New("data URI 缺少逗号分隔")
	}
	meta, payload := rest[:comma], rest[comma+1:]
	if !strings.Contains(strings.ToLower(meta), ";base64") {
		return "", nil, errors.New("只支持 base64 编码的 data URI")
	}
	mime := meta
	if i := strings.Index(mime, ";"); i >= 0 {
		mime = mime[:i]
	}
	mime = strings.TrimSpace(mime)
	if mime == "" {
		mime = "application/octet-stream"
	}

	// 有些客户端会在 base64 里插换行，解码前统一去掉空白。
	payload = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, payload)
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", nil, fmt.Errorf("base64 解码失败：%w", err)
	}
	return mime, data, nil
}
