package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// SaveConfig writes the config back to the YAML file.
func (c *Config) SaveConfig(path string) error {
	// Remove env var expansion artifacts — write clean YAML
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write config file: %w", err)
	}
	return nil
}

// ==================== Provider CRUD ====================

// GetProvider returns a pointer to the provider with the given name, or nil.
func (c *Config) GetProvider(name string) *ProviderConfig {
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			return &c.Providers[i]
		}
	}
	return nil
}

// AddProvider adds a new provider. Returns error if name already exists.
func (c *Config) AddProvider(p ProviderConfig) error {
	if c.GetProvider(p.Name) != nil {
		return fmt.Errorf("provider %q already exists", p.Name)
	}
	c.Providers = append(c.Providers, p)
	return nil
}

// UpdateProvider replaces the provider with the same name. Returns error if not found.
func (c *Config) UpdateProvider(p ProviderConfig) error {
	for i := range c.Providers {
		if c.Providers[i].Name == p.Name {
			c.Providers[i] = p
			return nil
		}
	}
	return fmt.Errorf("provider %q not found", p.Name)
}

// DeleteProvider removes a provider by name AND cascades:
//   - Removes all direct model routes referencing this provider
//   - Removes this provider's backends from combo models (removes combo if empty)
//   - Cleans up AllowedModels references in API keys
//
// Returns (routesRemoved, combosRemoved, error).
func (c *Config) DeleteProvider(name string) (int, int, error) {
	found := false
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			c.Providers = append(c.Providers[:i], c.Providers[i+1:]...)
			found = true
			break
		}
	}
	if !found {
		return 0, 0, fmt.Errorf("provider %q not found", name)
	}

	// Collect model names being removed (direct routes)
	var removedModelNames []string
	var kept []ModelConfig
	routesRemoved := 0
	combosRemoved := 0

	for _, m := range c.Models {
		if m.Strategy == "" {
			// Direct model
			if m.Provider == name {
				removedModelNames = append(removedModelNames, m.Name)
				routesRemoved++
				continue
			}
		} else {
			// Combo model — remove backends referencing this provider
			var keptBackends []BackendConfig
			for _, b := range m.Backends {
				if b.Provider != name {
					keptBackends = append(keptBackends, b)
				}
			}
			if len(keptBackends) == 0 {
				// All backends gone — remove entire combo
				removedModelNames = append(removedModelNames, m.Name)
				combosRemoved++
				continue
			}
			m.Backends = keptBackends
		}
		kept = append(kept, m)
	}
	c.Models = kept

	// Clean up AllowedModels in API keys
	if len(removedModelNames) > 0 {
		removedSet := make(map[string]bool, len(removedModelNames))
		for _, n := range removedModelNames {
			removedSet[n] = true
		}
		for i := range c.APIKeys {
			var keptModels []string
			for _, m := range c.APIKeys[i].AllowedModels {
				if !removedSet[m] {
					keptModels = append(keptModels, m)
				}
			}
			c.APIKeys[i].AllowedModels = keptModels
		}
	}

	return routesRemoved, combosRemoved, nil
}

// ==================== Model CRUD ====================

// GetModel returns a pointer to the model route with the given name, or nil.
func (c *Config) GetModel(name string) *ModelConfig {
	for i := range c.Models {
		if c.Models[i].Name == name {
			return &c.Models[i]
		}
	}
	return nil
}

// AddModel adds a new model route. Returns error if name already exists.
func (c *Config) AddModel(m ModelConfig) error {
	if c.GetModel(m.Name) != nil {
		return fmt.Errorf("model %q already exists", m.Name)
	}
	c.Models = append(c.Models, m)
	return nil
}

// DeleteModel removes a model route by name AND cascades:
//   - For direct models: removes the model ID from the provider's Models list
//   - Removes the model name from all API key AllowedModels
//
// Returns error if not found.
func (c *Config) DeleteModel(name string) error {
	var deleted ModelConfig
	found := false
	for i := range c.Models {
		if c.Models[i].Name == name {
			deleted = c.Models[i]
			c.Models = append(c.Models[:i], c.Models[i+1:]...)
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("model %q not found", name)
	}

	// For direct models: remove model ID from provider's Models list
	if deleted.Strategy == "" && deleted.Provider != "" {
		for i := range c.Providers {
			if c.Providers[i].Name == deleted.Provider {
				var kept []string
				for _, m := range c.Providers[i].Models {
					if m != deleted.Model {
						kept = append(kept, m)
					}
				}
				c.Providers[i].Models = kept
				break
			}
		}
	}

	// Remove from all API key AllowedModels
	for i := range c.APIKeys {
		var kept []string
		for _, m := range c.APIKeys[i].AllowedModels {
			if m != name {
				kept = append(kept, m)
			}
		}
		c.APIKeys[i].AllowedModels = kept
	}

	return nil
}

// ==================== API Key CRUD ====================

// GetAPIKey returns a pointer to the API key config with the given key, or nil.
func (c *Config) GetAPIKey(key string) *APIKeyConfig {
	for i := range c.APIKeys {
		if c.APIKeys[i].Key == key {
			return &c.APIKeys[i]
		}
	}
	return nil
}

// AddAPIKey adds a new API key. Returns error if key already exists.
func (c *Config) AddAPIKey(k APIKeyConfig) error {
	if c.GetAPIKey(k.Key) != nil {
		return fmt.Errorf("api key already exists")
	}
	c.APIKeys = append(c.APIKeys, k)
	return nil
}

// DeleteAPIKey removes an API key. Returns error if not found.
func (c *Config) DeleteAPIKey(key string) error {
	for i := range c.APIKeys {
		if c.APIKeys[i].Key == key {
			c.APIKeys = append(c.APIKeys[:i], c.APIKeys[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("api key not found")
}
