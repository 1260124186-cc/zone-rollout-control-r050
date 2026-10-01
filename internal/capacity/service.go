// Package capacity provides a read-only capacity precheck. A precheck
// answers "could this plan lease this many nodes right now?" without creating
// leases, mutating nodes, or writing audit events. Its result is bound to
// exactly one store snapshot and expires after a fixed TTL; formal allocation
// always re-validates eligibility independently.
package capacity

import (
	"context"

	"solo-0014-zone-rollout-control/internal/domain"
	"solo-0014-zone-rollout-control/internal/platform"
	"solo-0014-zone-rollout-control/internal/store"
)

type Service struct {
	store *store.Store
	clock platform.Clock
}

func New(stateStore *store.Store, clock platform.Clock) *Service {
	return &Service{store: stateStore, clock: clock}
}

// Precheck evaluates one question under a single read transaction. The
// store's RLock is held while the snapshot is copied, so concurrent node or
// catalog changes cannot mix two revisions into one answer.
func (s *Service) Precheck(
	ctx context.Context,
	planID string,
	input domain.CapacityPrecheckInput,
) (domain.CapacityPrecheck, error) {
	if err := domain.ValidatePrecheckInput(input); err != nil {
		return domain.CapacityPrecheck{}, err
	}
	input = domain.NormalizePrecheckInput(input)
	now := s.clock.Now().UTC()

	var result domain.CapacityPrecheck
	err := s.store.Read(ctx, func(state *domain.State) error {
		plan, ok := state.Plans[planID]
		if !ok {
			return domain.NotFound("plan", planID)
		}
		if err := domain.ValidateCapabilitiesAgainstCatalog(input.RequiredCapabilities, state.CapabilityCatalog); err != nil {
			return err
		}
		nodes := make([]domain.ResolverNode, 0, len(state.Nodes))
		for _, node := range state.Nodes {
			nodes = append(nodes, node)
		}
		leases := make([]domain.NodeLease, 0, len(state.Leases))
		for _, lease := range state.Leases {
			leases = append(leases, lease)
		}
		result = domain.BuildCapacityPrecheck(
			plan,
			input,
			nodes,
			leases,
			state.CapabilityCatalog,
			state.Revision,
			now,
			domain.PrecheckTTL,
		)
		return nil
	})
	return result, err
}
