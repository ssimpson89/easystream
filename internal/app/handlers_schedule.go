package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ssimpson89/easystream/internal/schedule"
)

// --- Recurring schedules ---

func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	if s.schedStore == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, s.schedStore.Schedules())
}

func (s *Server) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	if s.schedStore == nil {
		writeError(w, http.StatusBadRequest, "schedule storage not configured")
		return
	}
	var sched schedule.Schedule
	if err := json.NewDecoder(r.Body).Decode(&sched); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := s.schedStore.CreateSchedule(sched)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) handleUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	if s.schedStore == nil {
		writeError(w, http.StatusBadRequest, "schedule storage not configured")
		return
	}
	var sched schedule.Schedule
	if err := json.NewDecoder(r.Body).Decode(&sched); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sched.ID = r.PathValue("id")
	result, err := s.schedStore.UpdateSchedule(sched)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	if s.schedStore == nil {
		writeError(w, http.StatusBadRequest, "schedule storage not configured")
		return
	}
	id := r.PathValue("id")
	if err := s.schedStore.DeleteSchedule(id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// --- One-time overrides ---

func (s *Server) handleListOverrides(w http.ResponseWriter, r *http.Request) {
	if s.schedStore == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, s.schedStore.Overrides())
}

func (s *Server) handleCreateOverride(w http.ResponseWriter, r *http.Request) {
	if s.schedStore == nil {
		writeError(w, http.StatusBadRequest, "schedule storage not configured")
		return
	}
	var o schedule.Override
	if err := json.NewDecoder(r.Body).Decode(&o); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := s.schedStore.CreateOverride(o)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) handleUpdateOverride(w http.ResponseWriter, r *http.Request) {
	if s.schedStore == nil {
		writeError(w, http.StatusBadRequest, "schedule storage not configured")
		return
	}
	var o schedule.Override
	if err := json.NewDecoder(r.Body).Decode(&o); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	o.ID = r.PathValue("id")
	result, err := s.schedStore.UpdateOverride(o)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleDeleteOverride(w http.ResponseWriter, r *http.Request) {
	if s.schedStore == nil {
		writeError(w, http.StatusBadRequest, "schedule storage not configured")
		return
	}
	id := r.PathValue("id")
	if err := s.schedStore.DeleteOverride(id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// --- Upcoming events (computed from schedules + overrides) ---

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	if s.schedStore == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, s.schedStore.NextEvents(20, time.Now().UTC()))
}
