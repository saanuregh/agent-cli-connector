package agent

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
)

// Registry maps provider names to providers, for hosts that choose a
// harness by name at run time. The zero value is ready to use.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

// NewRegistry returns a registry holding providers.
func NewRegistry(providers ...Provider) *Registry {
	r := &Registry{}
	for _, p := range providers {
		r.Register(p)
	}
	return r
}

// Register adds or replaces p under p.Name().
func (r *Registry) Register(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.providers == nil {
		r.providers = make(map[string]Provider)
	}
	r.providers[p.Name()] = p
}

// Get returns the provider with the given name.
func (r *Registry) Get(name string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	return p, ok
}

// Names returns the registered provider names in sorted order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := slices.AppendSeq(make([]string, 0, len(r.providers)), maps.Keys(r.providers))
	slices.Sort(names)
	return names
}

// Start starts a session with the named provider.
func (r *Registry) Start(ctx context.Context, provider string, cfg Config) (Session, error) {
	p, ok := r.Get(provider)
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", provider)
	}
	return p.Start(ctx, cfg)
}
