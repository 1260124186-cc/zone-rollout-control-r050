package workflowprobe

import (
	"net/http"

	"solo-0014-zone-rollout-control/internal/domain"
)

// runCapacityPrecheck exercises the read-only capacity precheck over real
// HTTP routes: it must stay read-only, stay stable on one snapshot, bound its
// result size, honor a versioned capability catalog, and leave formal
// allocation free to re-validate eligibility on its own snapshot.
func runCapacityPrecheck() error {
	harness, err := newHarness()
	if err != nil {
		return err
	}
	defer harness.close()

	// Draft plan: precheck answers but reports plan_not_active.
	draftPlan, err := createPlan(harness, "precheck.example")
	if err != nil {
		return err
	}
	draftView, err := precheck(harness, draftPlan.ID, "east", 1, nil)
	if err != nil {
		return err
	}
	if draftView.Satisfiable {
		return ensure(false, "draft plan precheck must not be satisfiable")
	}
	if !hasReason(draftView.BlockingReasons, "plan_not_active") {
		return ensure(false, "draft plan precheck must report plan_not_active")
	}
	beforeNodes, err := listNodeCount(harness)
	if err != nil {
		return err
	}

	// Define the capability catalog, then enroll nodes with capabilities.
	catalogView, err := replaceCatalog(harness, []string{"dnssec", "anycast", "ipv6"})
	if err != nil {
		return err
	}
	if catalogView.Version != 1 {
		return ensure(false, "first catalog replacement must advance to version 1")
	}
	if _, err := enrollNodeWithCapabilities(harness, "resolver-east-01", "east", []string{"dnssec", "anycast"}); err != nil {
		return err
	}
	if _, err := enrollNodeWithCapabilities(harness, "resolver-east-02", "east", []string{"dnssec"}); err != nil {
		return err
	}
	if _, err := enrollNodeWithCapabilities(harness, "resolver-east-03", "east", nil); err != nil {
		return err
	}
	if _, err := enrollNodeWithCapabilities(harness, "resolver-west-01", "west", []string{"dnssec", "ipv6"}); err != nil {
		return err
	}
	if err := enrollQuarantined(harness, "resolver-east-04", "east"); err != nil {
		return err
	}

	// Unknown capability names are rejected before touching state.
	_, err = precheck(harness, draftPlan.ID, "east", 1, []string{"missing-cap"})
	if err := expectFailure(err, http.StatusBadRequest, string(domain.CodeValidationFailed)); err != nil {
		return err
	}
	if err := enrollNodeWithCapabilitiesError(harness, "resolver-east-05", "east", []string{"dnssec", "ghost"}); err != nil {
		return err
	}

	plan, _, err := createActivePlan(harness, "precheck.example", 2026100101, 2, 1, 100)
	if err != nil {
		return err
	}

	// 4 nodes in east, one quarantined: three eligible, quantity two fits.
	fit, err := precheck(harness, plan.ID, "east", 2, nil)
	if err != nil {
		return err
	}
	if !fit.Satisfiable || len(fit.BlockingReasons) != 0 {
		return ensure(false, "east must satisfy two plain nodes")
	}
	if fit.EligibleCount != 3 || fit.TotalCount != 4 || fit.QuarantinedCount != 1 {
		return ensure(false, "east counts must reflect three eligible of four in region")
	}
	if len(fit.EligibleNodes) != 2 {
		return ensure(false, "eligible node list is bounded by the requested quantity")
	}
	if fit.Reservation != domain.PrecheckReservationNone || fit.Guarantee != domain.PrecheckGuarantee {
		return ensure(false, "precheck must declare no reservation and revalidation guarantee")
	}
	if fit.TTLSeconds != 30 || !fit.ExpiresAt.After(fit.EvaluatedAt) {
		return ensure(false, "precheck must carry a positive 30 second validity window")
	}

	// dnssec+anycast: only one east node matches, quantity two is short.
	filtered, err := precheck(harness, plan.ID, "east", 2, []string{"dnssec", "anycast"})
	if err != nil {
		return err
	}
	if filtered.Satisfiable {
		return ensure(false, "two anycast-capable nodes do not exist in east")
	}
	if !hasReason(filtered.BlockingReasons, "capacity_insufficient") {
		return ensure(false, "capability shortfall must report capacity_insufficient")
	}
	if filtered.EligibleCount != 1 || len(filtered.EligibleNodes) != 1 {
		return ensure(false, "exactly one node matches dnssec+anycast in east")
	}
	if filtered.EligibleNodes[0].NodeName != "resolver-east-01" {
		return ensure(false, "matching node must be resolver-east-01")
	}

	// Bounded quantity is validated up front.
	if _, err := precheck(harness, plan.ID, "east", 101, nil); err != nil {
		if failure := failureOf(err); failure == nil || failure.Status != http.StatusBadRequest {
			return ensure(false, "quantity above 100 must be rejected with HTTP 400")
		}
	} else {
		return ensure(false, "quantity above 100 must be rejected")
	}

	// Repeated precheck on the unchanged snapshot is identical and read-only:
	// same deterministic id, same snapshot and catalog versions, no new nodes.
	repeat, err := precheck(harness, plan.ID, "east", 2, nil)
	if err != nil {
		return err
	}
	if repeat.ID != fit.ID {
		return ensure(false, "repeated precheck on one snapshot must have a stable id")
	}
	if repeat.SnapshotRevision != fit.SnapshotRevision || repeat.CatalogVersion != fit.CatalogVersion {
		return ensure(false, "repeated precheck must observe the same snapshot and catalog versions")
	}
	afterNodes, err := listNodeCount(harness)
	if err != nil {
		return err
	}
	if afterNodes != beforeNodes+5 {
		return ensure(false, "prechecks must not create nodes")
	}

	// Formal allocation re-validates against its own write snapshot and
	// applies the capability filter independently of any precheck.
	group, err := allocateWithCapabilities(harness, plan.ID, "lease-precheck-001", "east", 2, []string{"dnssec"})
	if err != nil {
		return err
	}
	if len(group.LeaseIDs) != 2 {
		return ensure(false, "two dnssec nodes must be allocated")
	}

	// A satisfiable precheck taken before the allocation is now stale: the
	// snapshot revision advanced and capacity shrank; nothing was reserved.
	stale, err := precheck(harness, plan.ID, "east", 2, []string{"dnssec"})
	if err != nil {
		return err
	}
	if stale.SnapshotRevision == fit.SnapshotRevision {
		return ensure(false, "allocation must advance the snapshot revision seen by precheck")
	}
	if stale.Satisfiable || !hasReason(stale.BlockingReasons, "capacity_insufficient") {
		return ensure(false, "precheck must not reserve nodes against a later allocation")
	}
	if stale.EligibleCount != 0 || len(stale.EligibleNodes) != 0 {
		return ensure(false, "no dnssec node remains available after allocation")
	}

	// Catalog replacement changes catalog version, which invalidates
	// precheck identity even though node state revision is unchanged.
	updated, err := replaceCatalog(harness, []string{"dnssec", "anycast", "ipv6", "quic"})
	if err != nil {
		return err
	}
	if updated.Version != 2 {
		return ensure(false, "adding a capability must advance catalog version")
	}
	bumped, err := precheck(harness, plan.ID, "east", 1, nil)
	if err != nil {
		return err
	}
	if bumped.CatalogVersion != 2 {
		return ensure(false, "precheck must report the new catalog version")
	}
	noOpCatalog, err := replaceCatalog(harness, []string{"dnssec", "anycast", "ipv6", "quic"})
	if err != nil {
		return err
	}
	if noOpCatalog.Version != 2 {
		return ensure(false, "unchanged catalog replacement must be idempotent")
	}

	// Unknown plan is still a 404, not a precheck result.
	_, err = precheckRaw(harness, "pln_missing", "east", 1, nil)
	if err := expectFailure(err, http.StatusNotFound, string(domain.CodeNotFound)); err != nil {
		return err
	}
	return nil
}

func createPlan(harness *harness, zone string) (domain.RolloutPlan, error) {
	var plan domain.RolloutPlan
	err := harness.call(http.MethodPut, "/v1/plans", domain.PlanCreateInput{
		Zone:                zone,
		MinimumSuccessRatio: 1,
		MaximumLatencyMS:    100,
		ExpectedNodes:       2,
	}, &plan)
	return plan, err
}

func replaceCatalog(harness *harness, capabilities []string) (domain.CapabilityCatalog, error) {
	var result domain.CapabilityCatalog
	err := harness.call(http.MethodPut, "/v1/capabilities", domain.CapabilityCatalogInput{
		Capabilities: capabilities,
	}, &result)
	return result, err
}

func enrollNodeWithCapabilities(harness *harness, name string, region string, capabilities []string) (domain.ResolverNode, error) {
	var node domain.ResolverNode
	err := harness.call(http.MethodPut, "/v1/nodes", domain.NodeCreateInput{
		NodeName:     name,
		Region:       region,
		Capabilities: capabilities,
	}, &node)
	return node, err
}

func enrollNodeWithCapabilitiesError(harness *harness, name string, region string, capabilities []string) error {
	_, err := enrollNodeWithCapabilities(harness, name, region, capabilities)
	return expectFailure(err, http.StatusBadRequest, string(domain.CodeValidationFailed))
}

func enrollQuarantined(harness *harness, name string, region string) error {
	var node domain.ResolverNode
	return harness.call(http.MethodPut, "/v1/nodes", domain.NodeCreateInput{
		NodeName:   name,
		Region:     region,
		Quarantine: true,
	}, &node)
}

func precheckRaw(harness *harness, planID string, region string, quantity int, capabilities []string) (domain.CapacityPrecheck, error) {
	var result domain.CapacityPrecheck
	err := harness.call(http.MethodPost, "/v1/plans/"+planID+"/capacity-prechecks", domain.CapacityPrecheckInput{
		Region:               region,
		Quantity:             quantity,
		RequiredCapabilities: capabilities,
	}, &result)
	return result, err
}

func precheck(harness *harness, planID string, region string, quantity int, capabilities []string) (domain.CapacityPrecheck, error) {
	return precheckRaw(harness, planID, region, quantity, capabilities)
}

func allocateWithCapabilities(
	harness *harness,
	planID string,
	requestKey string,
	region string,
	maxNodes int,
	capabilities []string,
) (domain.LeaseGroup, error) {
	var group domain.LeaseGroup
	err := harness.call(http.MethodPatch, "/v1/plans/"+planID+"/leases", domain.LeaseInput{
		RequestKey:           requestKey,
		Region:               region,
		LeaseSeconds:         600,
		MaxNodes:             maxNodes,
		RequiredCapabilities: capabilities,
	}, &group)
	return group, err
}

func listNodeCount(harness *harness) (int, error) {
	var page domain.NodePage
	err := harness.call(http.MethodGet, "/v1/nodes?limit=100", nil, &page)
	return len(page.Items), err
}

func hasReason(reasons []domain.CapacityBlockingReason, code string) bool {
	for _, reason := range reasons {
		if reason.Code == code {
			return true
		}
	}
	return false
}

func failureOf(err error) *apiFailure {
	if err == nil {
		return nil
	}
	failure, ok := err.(*apiFailure)
	if !ok {
		return nil
	}
	return failure
}
