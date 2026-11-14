package server

// The console's reads: the fleet page, the runtime snapshot and the overview's
// counters. They used to be the bus's reads of this module's collections; the
// collections are this module's, so it answers.

import (
	"log/slog"
	"net/http"
	"strconv"
)

// restConsoleRunners serves the whole fleet, most recently registered first.
func (s *Server) restConsoleRunners(w http.ResponseWriter, r *http.Request) {
	runners, err := s.store.ListRunners()
	if err != nil {
		slog.Error("list runners", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runners": runners})
}

// restConsoleRunnerCounts answers the online number the overview prints.
func (s *Server) restConsoleRunnerCounts(w http.ResponseWriter, r *http.Request) {
	online, err := s.store.CountOnlineRunners()
	if err != nil {
		slog.Error("count online runners", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"online": online})
}

// restConsoleContainers serves the queue's rows, optionally for one owner.
func (s *Server) restConsoleContainers(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	containers, err := s.store.ListContainerSummaries(r.URL.Query().Get("user_id"), limit)
	if err != nil {
		slog.Error("list containers", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"containers": containers})
}

// restConsoleContainerCounts answers the status buckets, not one number per view:
// "active" is a sum the caller decides.
func (s *Server) restConsoleContainerCounts(w http.ResponseWriter, r *http.Request) {
	byStatus, err := s.store.CountContainersByStatus()
	if err != nil {
		slog.Error("count containers", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"by_status": byStatus})
}
