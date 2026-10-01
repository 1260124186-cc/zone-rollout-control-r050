// Package catalog owns the versioned node capability catalog. The catalog is
// the registry of capability names trusted by capacity prechecks and lease
// allocations.
package catalog

import (
	"context"
	"sort"

	"solo-0014-zone-rollout-control/internal/domain"
	"solo-0014-zone-rollout-control/internal/platform"
	"solo-0014-zone-rollout-control/internal/store"
)

type Service struct {
	store *store.Store
	clock platform.Clock
	ids   platform.IDGenerator
}

func New(stateStore *store.Store, clock platform.Clock, ids platform.IDGenerator) *Service {
	return &Service{store: stateStore, clock: clock, ids: ids}
}

// Get returns the current catalog inside a read snapshot.
func (s *Service) Get(ctx context.Context) (domain.CapabilityCatalog, error) {
	var result domain.CapabilityCatalog
	err := s.store.Read(ctx, func(state *domain.State) error {
		result = copyCatalog(state.CapabilityCatalog)
		return nil
	})
	return result, err
}

// Replace applies the desired key set. The version advances only when the
// key set actually changes, so unchanged replacements are idempotent and keep
// precheck identifiers stable.
func (s *Service) Replace(
	ctx context.Context,
	input domain.CapabilityCatalogInput,
) (domain.CapabilityCatalog, error) {
	if err := domain.ValidateCatalogInput(input); err != nil {
		return domain.CapabilityCatalog{}, err
	}
	input = domain.NormalizeCatalogInput(input)
	now := s.clock.Now().UTC()
	desired := make(map[string]struct{}, len(input.Capabilities))
	for _, key := range input.Capabilities {
		desired[key] = struct{}{}
	}

	var result domain.CapabilityCatalog
	err := s.store.Write(ctx, func(state *domain.State) error {
		current := state.CapabilityCatalog
		added, removed := diffCatalog(current.Capabilities, desired)
		if len(added) == 0 && len(removed) == 0 {
			result = copyCatalog(current)
			return nil
		}

		next := make(map[string]domain.Capability, len(desired))
		for key := range desired {
			if existing, ok := current.Capabilities[key]; ok {
				next[key] = existing
				continue
			}
			next[key] = domain.Capability{Key: key, AddedAt: now, UpdatedAt: now}
		}
		current.Capabilities = next
		current.Version++
		current.UpdatedAt = now
		state.CapabilityCatalog = current

		store.AddAudit(state, s.ids, "capability_catalog", "default", "replaced", now, map[string]any{
			"version": current.Version,
			"added":   added,
			"removed": removed,
		})
		result = copyCatalog(current)
		return nil
	})
	return result, err
}

func diffCatalog(current map[string]domain.Capability, desired map[string]struct{}) (added []string, removed []string) {
	for key := range desired {
		if _, ok := current[key]; !ok {
			added = append(added, key)
		}
	}
	for key := range current {
		if _, ok := desired[key]; !ok {
			removed = append(removed, key)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

func copyCatalog(source domain.CapabilityCatalog) domain.CapabilityCatalog {
	copied := domain.CapabilityCatalog{
		Version:      source.Version,
		UpdatedAt:    source.UpdatedAt,
		Capabilities: make(map[string]domain.Capability, len(source.Capabilities)),
	}
	for key, capability := range source.Capabilities {
		copied.Capabilities[key] = capability
	}
	return copied
}
