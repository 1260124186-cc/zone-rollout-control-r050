package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

func ActivatePlan(plan *RolloutPlan, now time.Time) error {
	if plan.State != PlanDraft {
		return InvalidState("plan can be activated only from draft", map[string]any{
			"plan_id": plan.ID,
			"state":   plan.State,
		})
	}
	if plan.LatestRevisionID == "" {
		return InvalidState("plan needs a zone revision before activation", map[string]any{
			"plan_id": plan.ID,
		})
	}
	plan.State = PlanActive
	plan.ActivatedAt = timePointer(now.UTC())
	plan.Version++
	return nil
}

func CompletePlan(plan *RolloutPlan, now time.Time) error {
	if plan.State != PlanActive {
		return InvalidState("plan can be completed only from active", map[string]any{
			"plan_id": plan.ID,
			"state":   plan.State,
		})
	}
	plan.State = PlanCompleted
	plan.CompletedAt = timePointer(now.UTC())
	plan.Version++
	return nil
}

func LeaseFingerprint(planID string, input LeaseInput) string {
	payload := fmt.Sprintf(
		"%s|%s|%s|%d|%d|%s",
		planID,
		input.RequestKey,
		input.Region,
		input.LeaseSeconds,
		input.MaxNodes,
		strings.Join(input.RequiredCapabilities, ","),
	)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// PrecheckFingerprint is deterministic over the question and the exact
// snapshots that answer it. Repeating a precheck while neither state revision
// nor catalog version changes yields the same identifier.
func PrecheckFingerprint(
	planID string,
	input CapacityPrecheckInput,
	snapshotRevision uint64,
	catalogVersion uint64,
) string {
	payload := fmt.Sprintf(
		"precheck|%s|%s|%d|%s|%d|%d",
		planID,
		input.Region,
		input.Quantity,
		strings.Join(input.RequiredCapabilities, ","),
		snapshotRevision,
		catalogVersion,
	)
	sum := sha256.Sum256([]byte(payload))
	return "cap_" + hex.EncodeToString(sum[:16])
}

// BuildCapacityPrecheck evaluates capacity against one immutable snapshot. It
// is a pure function: it never mutates state and its output depends only on
// its inputs, which keeps repeated prechecks on the same snapshot stable.
func BuildCapacityPrecheck(
	plan RolloutPlan,
	input CapacityPrecheckInput,
	nodes []ResolverNode,
	leases []NodeLease,
	catalog CapabilityCatalog,
	snapshotRevision uint64,
	now time.Time,
	ttl time.Duration,
) CapacityPrecheck {
	now = now.UTC()
	required := input.RequiredCapabilities
	requiredSet := make(map[string]struct{}, len(required))
	for _, capability := range required {
		requiredSet[capability] = struct{}{}
	}

	// Mirror what allocation observes after its expiry sweep: an unexpired
	// claimed lease keeps its node occupied, while a due claimed lease frees
	// it back to ready during the sweep.
	occupied := make(map[string]struct{})
	freedBySweep := make(map[string]struct{})
	for _, lease := range leases {
		if lease.State != LeaseClaimed {
			continue
		}
		if lease.ExpiresAt.After(now) {
			occupied[lease.NodeID] = struct{}{}
			continue
		}
		freedBySweep[lease.NodeID] = struct{}{}
	}

	inRegion := make([]ResolverNode, 0, len(nodes))
	ready, leased, quarantined := 0, 0, 0
	for _, node := range nodes {
		if input.Region != "" && node.Region != input.Region {
			continue
		}
		inRegion = append(inRegion, node)
		switch node.State {
		case NodeReady:
			ready++
		case NodeLeased:
			leased++
		case NodeQuarantined:
			quarantined++
		}
	}

	eligible := make([]ResolverNode, 0)
	for _, node := range inRegion {
		if node.State == NodeQuarantined {
			continue
		}
		if _, busy := occupied[node.ID]; busy {
			continue
		}
		// Exactly the candidates the allocation sweep would leave selectable:
		// ready nodes, and leased nodes whose claimed lease is already due.
		allocatable := node.State == NodeReady
		if node.State == NodeLeased {
			_, allocatable = freedBySweep[node.ID]
		}
		if !allocatable {
			continue
		}
		if !hasAllCapabilities(node.Capabilities, requiredSet) {
			continue
		}
		eligible = append(eligible, node)
	}
	// Identical ordering to allocation: region first, then node ID.
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].Region != eligible[j].Region {
			return eligible[i].Region < eligible[j].Region
		}
		return eligible[i].ID < eligible[j].ID
	})

	satisfiable := plan.State == PlanActive && len(eligible) >= input.Quantity
	reasons := make([]CapacityBlockingReason, 0)
	if plan.State != PlanActive {
		reasons = append(reasons, CapacityBlockingReason{
			Code:    "plan_not_active",
			Message: "plan must be active for node leasing",
			Details: map[string]any{"plan_id": plan.ID, "state": plan.State},
		})
	}
	if len(eligible) < input.Quantity {
		reasons = append(reasons, CapacityBlockingReason{
			Code:    "capacity_insufficient",
			Message: "fewer eligible nodes are ready than the requested quantity",
			Details: map[string]any{
				"requested":   input.Quantity,
				"available":   len(eligible),
				"shortfall":   input.Quantity - len(eligible),
				"ready":       ready,
				"leased":      leased,
				"quarantined": quarantined,
			},
			AffectedCount: input.Quantity - len(eligible),
		})
	}
	sort.SliceStable(reasons, func(i, j int) bool {
		return reasons[i].Code < reasons[j].Code
	})

	// Bounded node list: never emit more than the requested quantity.
	shown := eligible
	if len(shown) > input.Quantity {
		shown = shown[:input.Quantity]
	}
	views := make([]CapacityNodeView, 0, len(shown))
	for _, node := range shown {
		views = append(views, CapacityNodeView{
			NodeID:       node.ID,
			NodeName:     node.NodeName,
			Region:       node.Region,
			State:        node.State,
			Capabilities: append([]string(nil), node.Capabilities...),
		})
	}

	precheck := CapacityPrecheck{
		PlanID:               plan.ID,
		Region:               input.Region,
		Quantity:             input.Quantity,
		RequiredCapabilities: append([]string(nil), required...),
		Satisfiable:          satisfiable,
		BlockingReasons:      reasons,
		EligibleNodes:        views,
		TotalCount:           len(inRegion),
		EligibleCount:        len(eligible),
		ReadyCount:           ready,
		LeasedCount:          leased,
		QuarantinedCount:     quarantined,
		Reservation:          PrecheckReservationNone,
		Guarantee:            PrecheckGuarantee,
		SnapshotRevision:     snapshotRevision,
		CatalogVersion:       catalog.Version,
		EvaluatedAt:          now,
		ExpiresAt:            now.Add(ttl),
		TTLSeconds:           int(ttl.Seconds()),
	}
	precheck.ID = PrecheckFingerprint(plan.ID, input, snapshotRevision, catalog.Version)
	return precheck
}

func hasAllCapabilities(nodeCapabilities []string, required map[string]struct{}) bool {
	for capability := range required {
		found := false
		for _, owned := range nodeCapabilities {
			if owned == capability {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func ProbeFingerprint(leaseID string, input ProbeInput) string {
	payload := fmt.Sprintf(
		"%s|%s|%t|%d|%d|%s",
		leaseID,
		input.RequestKey,
		input.Success,
		input.ObservedSerial,
		input.LatencyMS,
		input.ObservedAt.UTC().Format(time.RFC3339Nano),
	)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

func BuildAssessment(
	id string,
	plan RolloutPlan,
	revision ZoneRevision,
	probes []Probe,
	now time.Time,
) RolloutAssessment {
	reasons := make(map[string]struct{})
	if len(probes) == 0 {
		reasons["no_probes"] = struct{}{}
	}
	probeCount := len(probes)
	successCount := 0
	maximumLatency := 0
	nodes := make(map[string]struct{})
	for _, probe := range probes {
		nodes[probe.NodeID] = struct{}{}
		if probe.LatencyMS > maximumLatency {
			maximumLatency = probe.LatencyMS
		}
		if probe.Success {
			successCount++
			if probe.ObservedSerial != revision.Serial {
				reasons["serial_mismatch"] = struct{}{}
			}
		}
	}
	if len(nodes) < plan.ExpectedNodes {
		reasons["node_not_observed"] = struct{}{}
	}
	successRatio := 0.0
	if probeCount > 0 {
		successRatio = round(float64(successCount)/float64(probeCount), 6)
	}
	if successRatio < plan.MinimumSuccessRatio {
		reasons["success_ratio_below_minimum"] = struct{}{}
	}
	if maximumLatency > plan.MaximumLatencyMS {
		reasons["latency_limit_exceeded"] = struct{}{}
	}
	reasonList := make([]string, 0, len(reasons))
	for reason := range reasons {
		reasonList = append(reasonList, reason)
	}
	sort.Strings(reasonList)
	verdict := VerdictConsistent
	if len(reasonList) > 0 {
		verdict = VerdictReviewRequired
	}
	return RolloutAssessment{
		ID:               id,
		PlanID:           plan.ID,
		RevisionID:       revision.ID,
		Verdict:          verdict,
		Reasons:          reasonList,
		ProbeCount:       probeCount,
		SuccessCount:     successCount,
		SuccessRatio:     successRatio,
		MaximumLatencyMS: maximumLatency,
		CreatedAt:        now.UTC(),
	}
}

func round(value float64, places int) float64 {
	scale := math.Pow10(places)
	return math.Round(value*scale) / scale
}

func timePointer(value time.Time) *time.Time {
	return &value
}
