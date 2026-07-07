package provider

// GroqProvider handles requests to the Groq API.
// Groq uses OpenAI-compatible format, so we embed OpenAIProvider.
type GroqProvider struct {
	*OpenAIProvider
}

// NewGroqProvider creates a new Groq provider.
func NewGroqProvider(base *BaseProvider) *GroqProvider {
	return &GroqProvider{OpenAIProvider: &OpenAIProvider{BaseProvider: base}}
}
