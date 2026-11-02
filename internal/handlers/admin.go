package handlers

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vinhpnguyen123/goalstream/internal/httpx"
	"github.com/vinhpnguyen123/goalstream/internal/transcode"
)

// createJob is POST /admin/clips/{id}/transcode → 202 {job_id}: the row first, then the push.
// A failed push is still a 202 — the row is the job, and the reaper pushes it after
// REQUEUE_AFTER.
func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	clip := chi.URLParam(r, "id")
	if uuid.Validate(clip) != nil {
		httpx.Error(w, r, http.StatusNotFound, "not_found", "clip not found")
		return
	}
	job, err := transcode.CreateJob(r.Context(), s.Pool, clip)
	var active *transcode.ActiveJobError
	switch {
	case err == nil:
		if perr := s.Queue.Push(r.Context(), job); perr != nil {
			httpx.Log(r.Context()).Warn("queue push failed; the reaper will push the job", "job", job, "err", perr)
		}
		httpx.JSON(w, r, http.StatusAccepted, map[string]string{"job_id": job})
	case errors.Is(err, transcode.ErrClipNotFound):
		httpx.Error(w, r, http.StatusNotFound, "not_found", "clip not found")
	case errors.As(err, &active):
		httpx.ErrorWith(w, r, http.StatusConflict, "job_active", "a transcode job is already active for this clip",
			map[string]string{"job_id": active.JobID})
	default:
		httpx.Internal(w, r, err)
	}
}

// setDelivery is POST /admin/clips/{id}/delivery with exactly one of
// {"source_type":"mp4"} or {"hls_version":"<uuid>"}.
func (s *Server) setDelivery(w http.ResponseWriter, r *http.Request) {
	clip := chi.URLParam(r, "id")
	if uuid.Validate(clip) != nil {
		httpx.Error(w, r, http.StatusNotFound, "not_found", "clip not found")
		return
	}
	var req struct {
		SourceType *string `json:"source_type"`
		HLSVersion *string `json:"hls_version"`
	}
	if !decode(w, r, &req) {
		return
	}
	toMP4 := req.SourceType != nil && *req.SourceType == "mp4" && req.HLSVersion == nil
	toVersion := req.SourceType == nil && req.HLSVersion != nil && uuid.Validate(*req.HLSVersion) == nil
	if !toMP4 && !toVersion {
		httpx.Error(w, r, http.StatusBadRequest, "validation_error", `body must be {"source_type":"mp4"} or {"hls_version":"<uuid>"}`)
		return
	}
	version := ""
	if toVersion {
		version = *req.HLSVersion
	}
	changed, err := transcode.SetDelivery(r.Context(), s.Pool, clip, version)
	switch {
	case err == nil:
		httpx.JSON(w, r, http.StatusOK, map[string]bool{"changed": changed})
	case errors.Is(err, transcode.ErrClipNotFound):
		httpx.Error(w, r, http.StatusNotFound, "not_found", "clip not found")
	case errors.Is(err, transcode.ErrVersionNotFound):
		httpx.Error(w, r, http.StatusNotFound, "version_not_found", "no committed hls version with that id")
	default:
		httpx.Internal(w, r, err)
	}
}

// clipStatus is GET /admin/clips/{id}/status (docs/API.md).
func (s *Server) clipStatus(w http.ResponseWriter, r *http.Request) {
	clip := chi.URLParam(r, "id")
	if uuid.Validate(clip) != nil {
		httpx.Error(w, r, http.StatusNotFound, "not_found", "clip not found")
		return
	}
	st, err := transcode.Status(r.Context(), s.Pool, clip)
	switch {
	case err == nil:
		httpx.JSON(w, r, http.StatusOK, st)
	case errors.Is(err, transcode.ErrClipNotFound):
		httpx.Error(w, r, http.StatusNotFound, "not_found", "clip not found")
	default:
		httpx.Internal(w, r, err)
	}
}
