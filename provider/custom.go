package provider

// CustomProvider handles requests to any OpenAI-compatible API endpoint.
// Useful for self-hosted models (Ollama, vLLM, LocalAI, etc.).
type CustomProvider struct {
	*OpenAIProvider
}

// NewCustomProvider creates a new custom OpenAI-compatible provider.
func NewCustomProvider(base *BaseProvider) *CustomProvider {
	return &CustomProvider{OpenAIProvider: &OpenAIProvider{BaseProvider: base}}
}
