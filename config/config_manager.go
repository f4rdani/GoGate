package config

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// SyncProviderModels ensures that every provider's Models list reflects all
// models mapped to that provider in c.Models (direct routes and combo backends)
// as well as any existing configured models, with duplicates removed and sorted.
func (c *Config) SyncProviderModels() {
	for i := range c.Providers {
		provName := c.Providers[i].Name
		modelsSet := make(map[string]bool)
		for _, m := range c.Providers[i].Models {
			m = strings.TrimSpace(m)
			if m != "" {
				modelsSet[m] = true
			}
		}
		for _, mr := range c.Models {
			if mr.Strategy == "" && mr.Provider == provName {
				target := mr.Model
				if target == "" {
					target = mr.Name
				}
				if target != "" {
					modelsSet[target] = true
				}
			}
			for _, b := range mr.Backends {
				if b.Provider == provName && b.Model != "" {
					modelsSet[b.Model] = true
				}
			}
		}
		var list []string
		for m := range modelsSet {
			list = append(list, m)
		}
		sort.Strings(list)
		c.Providers[i].Models = list
	}
}

// SaveConfig writes the config back to the YAML file.
func (c *Config) SaveConfig(path string) error {
	c.SyncProviderModels()
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
//   - Removes this provider's backends from combo models. A combo left with
//     fewer than 2 backends is removed as well (invalid combo).
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
			if len(keptBackends) < 2 {
				// Zero backends — or a single leftover that can never
				// satisfy combo validation (min 2) — remove entire combo.
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
	c.SyncProviderModels()
	return nil
}

// DeleteModel removes a model route by name AND cascades:
//   - For direct models: removes the model ID from the provider's Models list,
//     and removes matching combo backends (same provider + upstream model).
//     Combos left with fewer than 2 backends are deleted as well (a combo
//     needs at least 2 backends to stay valid).
//   - Removes the model name (plus any combo names deleted by the cascade)
//     from all API key AllowedModels.
//
// Returns (combosUpdated, combosDeleted, error). combosUpdated lists combos
// that lost a backend but survived; combosDeleted lists combos removed by
// the cascade.
func (c *Config) DeleteModel(name string) ([]string, []string, error) {
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
		return nil, nil, fmt.Errorf("model %q not found", name)
	}

	var combosUpdated []string
	var combosDeleted []string

	// For direct models: strip matching backends from every combo.
	if deleted.Strategy == "" && deleted.Provider != "" {
		var kept []ModelConfig
		for _, m := range c.Models {
			if m.Strategy == "" {
				kept = append(kept, m)
				continue
			}
			var keptBackends []BackendConfig
			for _, b := range m.Backends {
				if b.Provider == deleted.Provider && b.Model == deleted.Model {
					continue
				}
				keptBackends = append(keptBackends, b)
			}
			if len(keptBackends) == len(m.Backends) {
				kept = append(kept, m)
				continue
			}
			if len(keptBackends) < 2 {
				// Combo no longer valid — drop it entirely.
				combosDeleted = append(combosDeleted, m.Name)
				continue
			}
			m.Backends = keptBackends
			kept = append(kept, m)
			combosUpdated = append(combosUpdated, m.Name)
		}
		c.Models = kept

		// Only remove from provider's Models list if no remaining route
		// (direct or combo backend) still references this upstream model on this provider.
		stillReferenced := false
		for _, m := range c.Models {
			if m.Strategy == "" {
				if m.Provider == deleted.Provider && m.Model == deleted.Model {
					stillReferenced = true
					break
				}
			} else {
				for _, b := range m.Backends {
					if b.Provider == deleted.Provider && b.Model == deleted.Model {
						stillReferenced = true
						break
					}
				}
				if stillReferenced {
					break
				}
			}
		}

		if !stillReferenced {
			for i := range c.Providers {
				if c.Providers[i].Name == deleted.Provider {
					var keptModels []string
					for _, m := range c.Providers[i].Models {
						if m != deleted.Model {
							keptModels = append(keptModels, m)
						}
					}
					c.Providers[i].Models = keptModels
					break
				}
			}
		}
	}

	// Remove the deleted route (plus cascade-deleted combos) from API keys.
	removedSet := map[string]bool{name: true}
	for _, n := range combosDeleted {
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

	return combosUpdated, combosDeleted, nil
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
