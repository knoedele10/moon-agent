package llm

import "testing"

func TestPromptCacheSwitch(t *testing.T) {
	t.Setenv("AGENT_PROMPT_CACHE", "")
	if len(CacheOptions("claude")) == 0 {
		t.Fatal("claude cache should be on by default")
	}
	if len(CacheOptions("openai")) != 0 {
		t.Fatal("non-claude backends must not get cache options")
	}
	t.Setenv("AGENT_PROMPT_CACHE", "off")
	if len(CacheOptions("claude")) != 0 {
		t.Fatal("explicit off must leave the request unchanged")
	}
}
