package httpapi

import (
	"net/http"

	"solo-0014-zone-rollout-control/internal/domain"
)

func (s *Server) getCapabilityCatalog(w http.ResponseWriter, r *http.Request) {
	result, err := s.capabilities.Get(r.Context())
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, result)
}

func (s *Server) replaceCapabilityCatalog(w http.ResponseWriter, r *http.Request) {
	var input domain.CapabilityCatalogInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	result, err := s.capabilities.Replace(r.Context(), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, result)
}

func (s *Server) capacityPrecheck(w http.ResponseWriter, r *http.Request) {
	var input domain.CapacityPrecheckInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	result, err := s.capacity.Precheck(r.Context(), r.PathValue("id"), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, result)
}
