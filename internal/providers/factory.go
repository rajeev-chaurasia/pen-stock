package providers

import (
	"fmt"

	"github.com/rajeev-chaurasia/pen-stock/internal/config"
)

// Builder constructs a Provider from one config entry.
type Builder func(cfg config.ProviderConfig) (Provider, error)

// builders maps a provider kind to its adapter constructor. Adapter
// packages register themselves at init time, so a binary must blank
// import every adapter it intends to serve. The indirection exists
// because this package cannot import its own adapters without an import
// cycle.
var builders = map[config.ProviderKind]Builder{}

// RegisterKind wires a provider kind to its adapter constructor.
// Call from an adapter package init only; not safe for concurrent use.
func RegisterKind(kind config.ProviderKind, b Builder) {
	builders[kind] = b
}

// BuildAll constructs one Provider per config entry, keyed by name.
func BuildAll(cfgs []config.ProviderConfig) (map[string]Provider, error) {
	out := make(map[string]Provider, len(cfgs))
	for _, cfg := range cfgs {
		if _, dup := out[cfg.Name]; dup {
			return nil, fmt.Errorf("provider %q: duplicate name", cfg.Name)
		}
		build, ok := builders[cfg.Kind]
		if !ok {
			return nil, fmt.Errorf("provider %q: unknown kind %q (no adapter registered for it)", cfg.Name, cfg.Kind)
		}
		p, err := build(cfg)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", cfg.Name, err)
		}
		out[cfg.Name] = p
	}
	return out, nil
}
