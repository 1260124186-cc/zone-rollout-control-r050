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

const (
	CapacityResultTTLSeconds         = 15
	CapacityBlockPlanNotActive       = "plan_not_active"
	CapacityBlockNoAvailableNodes    = "no_available_nodes"
	CapacityBlockInsufficientNodes   = "insufficient_available_nodes"
	CapacityBlockUnknownCapability   = "unknown_capability"
	CapacityBlockCapabilityUnavailable = "capability_unavailable"
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
		"%s|%s|%s|%d|%d",
		planID,
		input.RequestKey,
		input.Region,
		input.LeaseSeconds,
		input.MaxNodes,
	)
	if len(input.Capabilities) > 0 {
		payload += "|" + strings.Join(input.Capabilities, ",")
	}
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
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

func CapabilityCatalogRevision(nodes []ResolverNode) string {
	seen := make(map[string]struct{})
	catalog := make([]string, 0)
	for _, node := range nodes {
		for _, capability := range node.Capabilities {
			if _, exists := seen[capability]; exists {
				continue
			}
			seen[capability] = struct{}{}
			catalog = append(catalog, capability)
		}
	}
	sort.Strings(catalog)
	sum := sha256.Sum256([]byte(strings.Join(catalog, ",")))
	return hex.EncodeToString(sum[:])
}

func BuildCapacityCheck(
	plan RolloutPlan,
	allNodes []ResolverNode,
	leases map[string]NodeLease,
	input CapacityCheckInput,
	snapshotRevision uint64,
	now time.Time,
) CapacityCheck {
	now = now.UTC()
	catalog := make(map[string]struct{})
	for _, node := range allNodes {
		for _, capability := range node.Capabilities {
			catalog[capability] = struct{}{}
		}
	}
	catalogRevision := CapabilityCatalogRevision(allNodes)
	activeLeases := make(map[string]struct{})
	expiredLeasedNodes := make(map[string]struct{})
	for _, lease := range leases {
		if lease.State != LeaseClaimed {
			continue
		}
		if lease.ExpiresAt.After(now) {
			activeLeases[lease.NodeID] = struct{}{}
		} else {
			expiredLeasedNodes[lease.NodeID] = struct{}{}
		}
	}
	availableCount := 0
	unknown := make(map[string]struct{})
	for _, capability := range input.Capabilities {
		if _, exists := catalog[capability]; !exists {
			unknown[capability] = struct{}{}
		}
	}
	eligible := make([]ResolverNode, 0, MaxCapacityRequired)
	for _, node := range allNodes {
		if !nodeMatchesCapacity(node, activeLeases, expiredLeasedNodes, input) {
			continue
		}
		availableCount++
		if node.State == NodeLeased {
			if _, expired := expiredLeasedNodes[node.ID]; expired {
				node.State = NodeReady
			}
		}
		if len(eligible) < MaxCapacityRequired {
			eligible = append(eligible, node)
		}
	}
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].Region != eligible[j].Region {
			return eligible[i].Region < eligible[j].Region
		}
		return eligible[i].ID < eligible[j].ID
	})
	if len(eligible) > MaxCapacityRequired {
		eligible = eligible[:MaxCapacityRequired]
	}
	if len(eligible) > input.NodeLimit {
		eligible = eligible[:input.NodeLimit]
	}

	hasUnknownCapability := len(unknown) > 0
	unknownCapabilities := make([]string, 0, len(unknown))
	for capability := range unknown {
		unknownCapabilities = append(unknownCapabilities, capability)
	}
	sort.Strings(unknownCapabilities)
	reasons := make([]string, 0)
	if plan.State != PlanActive {
		reasons = append(reasons, CapacityBlockPlanNotActive)
	}
	if availableCount == 0 {
		if hasUnknownCapability {
			reasons = append(reasons, CapacityBlockUnknownCapability)
		} else if len(input.Capabilities) > 0 {
			reasons = append(reasons, CapacityBlockCapabilityUnavailable)
		}
		reasons = append(reasons, CapacityBlockNoAvailableNodes)
	} else if availableCount < input.Required {
		reasons = append(reasons, CapacityBlockInsufficientNodes)
		if hasUnknownCapability {
			reasons = append(reasons, CapacityBlockUnknownCapability)
		}
	}
	sort.Strings(reasons)

	nodes := make([]CapacityNode, 0, len(eligible))
	for _, node := range eligible {
		nodes = append(nodes, CapacityNode{
			ID:           node.ID,
			Region:       node.Region,
			State:        node.State,
			Version:      node.Version,
			Capabilities: append([]string(nil), node.Capabilities...),
		})
	}
	validUntil := now.Add(CapacityResultTTLSeconds * time.Second)
	sufficient := plan.State == PlanActive && !hasUnknownCapability && availableCount >= input.Required
	result := CapacityCheck{
		PlanID:                plan.ID,
		PlanState:             plan.State,
		Region:                input.Region,
		Required:              input.Required,
		RequiredCapabilities:  append([]string(nil), input.Capabilities...),
		AvailableCount:        availableCount,
		Sufficient:            sufficient,
		Blocked:               !sufficient,
		Nodes:                 nodes,
		BlockingReasons:       reasons,
		UnknownCapabilities:   unknownCapabilities,
		NodeCountReturned:     len(nodes),
		NodeLimit:             input.NodeLimit,
		SnapshotRevision:      snapshotRevision,
		CatalogRevision:       catalogRevision,
		CheckedAt:             now,
		ResultValidUntil:      validUntil,
		ResultValiditySeconds: CapacityResultTTLSeconds,
	}
	return result
}

func SelectAvailableNodes(
	nodes map[string]ResolverNode,
	leases map[string]NodeLease,
	region string,
	capabilities []string,
	now time.Time,
) []ResolverNode {
	input := CapacityCheckInput{
		Region:       region,
		Capabilities: capabilities,
		NodeLimit:    MaxCapacityRequired,
	}
	activeLeases := make(map[string]struct{})
	expiredLeasedNodes := make(map[string]struct{})
	for _, lease := range leases {
		if lease.State != LeaseClaimed {
			continue
		}
		if lease.ExpiresAt.After(now) {
			activeLeases[lease.NodeID] = struct{}{}
		} else {
			expiredLeasedNodes[lease.NodeID] = struct{}{}
		}
	}
	result := make([]ResolverNode, 0)
	for _, node := range nodes {
		if nodeMatchesCapacity(node, activeLeases, expiredLeasedNodes, input) {
			result = append(result, node)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Region != result[j].Region {
			return result[i].Region < result[j].Region
		}
		return result[i].ID < result[j].ID
	})
	return result
}

func nodeMatchesCapacity(
	node ResolverNode,
	activeLeases map[string]struct{},
	expiredLeasedNodes map[string]struct{},
	input CapacityCheckInput,
) bool {
	if node.State == NodeQuarantined {
		return false
	}
	if input.Region != "" && node.Region != input.Region {
		return false
	}
	if !hasAllCapabilities(node.Capabilities, input.Capabilities) {
		return false
	}
	if _, leased := activeLeases[node.ID]; leased {
		return false
	}
	if node.State == NodeReady {
		return true
	}
	if node.State == NodeLeased {
		_, expired := expiredLeasedNodes[node.ID]
		return expired
	}
	return false
}

func hasAllCapabilities(nodeCapabilities []string, required []string) bool {
	if len(required) == 0 {
		return true
	}
	available := make(map[string]struct{}, len(nodeCapabilities))
	for _, capability := range nodeCapabilities {
		available[capability] = struct{}{}
	}
	for _, capability := range required {
		if _, exists := available[capability]; !exists {
			return false
		}
	}
	return true
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
