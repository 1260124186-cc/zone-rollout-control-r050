package workflowprobe

import (
	"net/http"

	"solo-0014-zone-rollout-control/internal/domain"
)

func runNodeLeaseAllocation() error {
	harness, err := newHarness()
	if err != nil {
		return err
	}
	defer harness.close()

	if _, err := enrollNodeWithCapabilities(harness, "resolver-east-01", "east", []string{"dnssec"}); err != nil {
		return err
	}
	if _, err := enrollNodeWithCapabilities(harness, "resolver-east-02", "east", []string{"dnssec", "ipv6"}); err != nil {
		return err
	}
	if _, err := enrollNode(harness, "resolver-east-03", "east"); err != nil {
		return err
	}
	plan, _, err := createActivePlan(harness, "lease.example", 1001, 2, 0.5, 100)
	if err != nil {
		return err
	}
	firstCheck, err := checkCapacity(harness, plan.ID, "?required_nodes=2&region=east&capability=dnssec")
	if err != nil {
		return err
	}
	if !firstCheck.Sufficient || firstCheck.AvailableCount != 2 || len(firstCheck.Nodes) != 2 {
		return ensure(false, "capacity precheck must return both capable nodes")
	}
	secondCheck, err := checkCapacity(harness, plan.ID, "?required_nodes=2&region=east&capability=dnssec")
	if err != nil {
		return err
	}
	if secondCheck.SnapshotRevision != firstCheck.SnapshotRevision ||
		secondCheck.CatalogRevision != firstCheck.CatalogRevision ||
		secondCheck.AvailableCount != firstCheck.AvailableCount ||
		len(secondCheck.Nodes) != len(firstCheck.Nodes) {
		return ensure(false, "repeated capacity precheck must be stable on the same snapshot")
	}
	missingCheck, err := checkCapacity(harness, plan.ID, "?required_nodes=2&region=east&capability=ipv6")
	if err != nil {
		return err
	}
	if missingCheck.Sufficient || missingCheck.AvailableCount != 1 {
		return ensure(false, "capacity precheck must report insufficient capability capacity")
	}
	first, err := allocateWithCapabilities(harness, plan.ID, "lease-request-0001", "east", 2, []string{"dnssec"})
	if err != nil {
		return err
	}
	if len(first.LeaseIDs) != 2 {
		return ensure(false, "first lease group must contain two leases")
	}
	replay, err := allocateWithCapabilities(harness, plan.ID, "lease-request-0001", "east", 2, []string{"dnssec"})
	if err != nil {
		return err
	}
	if replay.ID != first.ID {
		return ensure(false, "lease request replay returned another group")
	}
	_, err = allocateWithCapabilities(harness, plan.ID, "lease-request-0002", "east", 2, []string{"dnssec"})
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeNodeUnavailable)); err != nil {
		return err
	}
	postCheck, err := checkCapacity(harness, plan.ID, "?required_nodes=2&region=east&capability=dnssec")
	if err != nil {
		return err
	}
	if postCheck.Sufficient || postCheck.AvailableCount != 0 ||
		postCheck.SnapshotRevision == firstCheck.SnapshotRevision ||
		postCheck.CatalogRevision != firstCheck.CatalogRevision {
		return ensure(false, "precheck must observe concurrent allocation through a new snapshot")
	}
	var released domain.NodeLease
	if err := harness.call(http.MethodPatch, "/v1/leases/"+first.LeaseIDs[0]+"/release", nil, &released); err != nil {
		return err
	}
	if released.State != domain.LeaseReleased {
		return ensure(false, "lease release did not update state")
	}
	return nil
}
