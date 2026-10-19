package llm

// NewDefaultRegistry configures production providers in one place. Tests can
// still use NewRegistry to install only the backends they need.
func NewDefaultRegistry() *Registry {
	r := NewRegistry()
	r.SetEndpoints(DefaultEndpoints())
	r.Register("deepseek", NewDeepSeekBackend)
	r.Register("openai", NewOpenAIBackend)
	r.Register("claude", NewClaudeBackend)
	r.Register("gemini", NewGeminiBackend)
	return r
}
