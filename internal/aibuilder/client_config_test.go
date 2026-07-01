package aibuilder

import "testing"

// TestNewClientFromEnv_OpenRouterDefault asserts that NOTIFY_AI_API_KEY alone
// selects an openaiClient pointed at the default OpenRouter base URL with the
// default model.
func TestNewClientFromEnv_OpenRouterDefault(t *testing.T) {
	getenv := func(key string) string {
		if key == "NOTIFY_AI_API_KEY" {
			return "sk-openrouter-key"
		}
		return ""
	}
	c := NewClientFromEnv(getenv)
	if c == nil {
		t.Fatal("expected non-nil Client when NOTIFY_AI_API_KEY set")
	}
	oc, ok := c.(*openaiClient)
	if !ok {
		t.Fatalf("expected *openaiClient, got %T", c)
	}
	if oc.model != DefaultOpenAIModel {
		t.Errorf("model = %q, want %q", oc.model, DefaultOpenAIModel)
	}
	if oc.baseURL != DefaultOpenAIBaseURL {
		t.Errorf("baseURL = %q, want %q", oc.baseURL, DefaultOpenAIBaseURL)
	}
}

// TestNewClientFromEnv_ModelOverride asserts NOTIFY_AI_MODEL overrides the
// default model on the openaiClient.
func TestNewClientFromEnv_ModelOverride(t *testing.T) {
	getenv := func(key string) string {
		switch key {
		case "NOTIFY_AI_API_KEY":
			return "sk-openrouter-key"
		case "NOTIFY_AI_MODEL":
			return "anthropic/claude-opus-4"
		}
		return ""
	}
	c := NewClientFromEnv(getenv)
	oc, ok := c.(*openaiClient)
	if !ok {
		t.Fatalf("expected *openaiClient, got %T", c)
	}
	if oc.model != "anthropic/claude-opus-4" {
		t.Errorf("model = %q, want anthropic/claude-opus-4", oc.model)
	}
}

// TestNewClientFromEnv_BaseURLOverride asserts NOTIFY_AI_BASE_URL overrides the
// default OpenRouter base URL.
func TestNewClientFromEnv_BaseURLOverride(t *testing.T) {
	getenv := func(key string) string {
		switch key {
		case "NOTIFY_AI_API_KEY":
			return "sk-openai-key"
		case "NOTIFY_AI_BASE_URL":
			return "https://api.openai.com/v1"
		}
		return ""
	}
	c := NewClientFromEnv(getenv)
	oc, ok := c.(*openaiClient)
	if !ok {
		t.Fatalf("expected *openaiClient, got %T", c)
	}
	if oc.baseURL != "https://api.openai.com/v1" {
		t.Errorf("baseURL = %q, want https://api.openai.com/v1", oc.baseURL)
	}
}

// TestNewClientFromEnv_AnthropicFallback asserts that when NOTIFY_AI_API_KEY is
// unset but ANTHROPIC_API_KEY is set, the builder falls back to anthropicClient.
func TestNewClientFromEnv_AnthropicFallback(t *testing.T) {
	getenv := func(key string) string {
		if key == "ANTHROPIC_API_KEY" {
			return "ant-key"
		}
		return ""
	}
	c := NewClientFromEnv(getenv)
	if c == nil {
		t.Fatal("expected non-nil Client when ANTHROPIC_API_KEY set")
	}
	if _, ok := c.(*anthropicClient); !ok {
		t.Fatalf("expected *anthropicClient for Anthropic fallback, got %T", c)
	}
}

// TestNewClientFromEnv_Disabled asserts that when neither key is set the
// builder is gracefully disabled (nil Client).
func TestNewClientFromEnv_Disabled(t *testing.T) {
	getenv := func(string) string { return "" }
	if c := NewClientFromEnv(getenv); c != nil {
		t.Errorf("expected nil Client when no keys set, got %T", c)
	}
}

// TestNewClientFromEnv_OpenAITakesPrecedence asserts NOTIFY_AI_API_KEY wins
// over ANTHROPIC_API_KEY when both are set.
func TestNewClientFromEnv_OpenAITakesPrecedence(t *testing.T) {
	getenv := func(key string) string {
		switch key {
		case "NOTIFY_AI_API_KEY":
			return "sk-openrouter"
		case "ANTHROPIC_API_KEY":
			return "ant-key"
		}
		return ""
	}
	c := NewClientFromEnv(getenv)
	if _, ok := c.(*openaiClient); !ok {
		t.Fatalf("expected *openaiClient when NOTIFY_AI_API_KEY set, got %T", c)
	}
}
