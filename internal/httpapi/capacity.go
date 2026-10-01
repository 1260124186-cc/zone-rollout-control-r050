package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"solo-0014-zone-rollout-control/internal/domain"
)

func (s *Server) checkCapacity(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	required := 1
	if raw := firstNonEmpty(query.Get("required_nodes"), query.Get("quantity"), query.Get("count")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			sendError(w, r, domain.Validation("required_nodes must be an integer", map[string]any{
				"required_nodes": raw,
			}))
			return
		}
		required = parsed
	}
	nodeLimit := domain.MaxCapacityNodes
	if raw := firstNonEmpty(query.Get("node_limit"), query.Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			sendError(w, r, domain.Validation("node_limit must be an integer", map[string]any{
				"node_limit": raw,
			}))
			return
		}
		nodeLimit = parsed
	}
	var capabilities []string
	for _, key := range []string{"capability", "capabilities"} {
		for _, commaSeparated := range query[key] {
			capabilities = append(capabilities, strings.Split(commaSeparated, ",")...)
		}
	}
	input := domain.CapacityCheckInput{
		Region:       firstNonEmpty(query.Get("region"), query.Get("zone")),
		Required:     required,
		NodeLimit:    nodeLimit,
		Capabilities: capabilities,
	}
	result, err := s.allocations.CheckCapacity(r.Context(), r.PathValue("id"), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, result)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
