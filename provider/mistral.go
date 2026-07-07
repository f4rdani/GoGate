package provider

// MistralProvider handles requests to the Mistral AI API.
// Mistral uses OpenAI-compatible format, so we embed OpenAIProvider.
type MistralProvider struct {
	*OpenAIProvider
}

// NewMistralProvider creates a new Mistral provider.
func NewMistralProvider(base *BaseProvider) *MistralProvider {
	return &MistralProvider{OpenAIProvider: &OpenAIProvider{BaseProvider: base}}
}
