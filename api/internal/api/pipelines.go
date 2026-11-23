package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"

	"github.com/ithaca-lab-org/idp-portal/api/internal/orders"
	"github.com/ithaca-lab-org/idp-portal/api/internal/pipelines"
)

const (
	pipelineWritesPerMinute = 10
	pipelineReadsPerMinute  = 120
)

func (a *API) pipelineRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/pipelines/namespaces", a.pipelinesOn(a.user(a.pipelineNamespaces)))
	mux.HandleFunc("GET /api/namespaces/{ns}/pipelines", a.pipelinesOn(a.user(a.listPipelines)))
	mux.HandleFunc("POST /api/namespaces/{ns}/pipelines", a.pipelinesOn(a.user(a.placePipeline)))
	mux.HandleFunc("GET /api/namespaces/{ns}/pipelines/{name}/runs", a.pipelinesOn(a.user(a.listRuns)))
	mux.HandleFunc("POST /api/namespaces/{ns}/pipelines/{name}/runs", a.pipelinesOn(a.user(a.placeRun)))
}

// pipelinesOn answers 404 for every pipelines route while the feature is off.
func (a *API) pipelinesOn(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.Pipelines == nil {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		next(w, r)
	}
}

func (a *API) pipelineNamespaces(w http.ResponseWriter, r *http.Request, who orders.Requester) {
	if !a.pipelineReads.allow(who.Sub) {
		tooMany(w)
		return
	}
	list, err := a.Pipelines.ListNamespaces(r.Context(), who)
	if err != nil {
		a.failPipeline(w, err)
		return
	}
	// the rules travel with the list so the form validates like the API
	writeJSON(w, http.StatusOK, map[string]any{"namespaces": list, "allowedOrgs": a.Pipelines.AllowedOrgs,
		"defaultPath": pipelines.DefaultPath, "maxRuns": pipelines.MaxRuns})
}

func (a *API) listPipelines(w http.ResponseWriter, r *http.Request, who orders.Requester) {
	if !a.pipelineReads.allow(who.Sub) {
		tooMany(w)
		return
	}
	list, err := a.Pipelines.List(r.Context(), who, r.PathValue("ns"))
	if err != nil {
		a.failPipeline(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pipelines": list})
}

// decodePipelineBody checks origin, rate, content type and shape of a write; false means the
// answer is already written.
func (a *API) decodePipelineBody(w http.ResponseWriter, r *http.Request, who orders.Requester, into any) bool {
	if !a.Auth.SameOrigin(r) {
		writeError(w, http.StatusForbidden, "forbidden")
		return false
	}
	if !a.pipelineWrites.allow(who.Sub) {
		tooMany(w)
		return false
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

type pipelineBody struct {
	Workload string `json:"workload"`
	Repo     string `json:"repo"`
	Path     string `json:"path"`
}

func (a *API) placePipeline(w http.ResponseWriter, r *http.Request, who orders.Requester) {
	var body pipelineBody
	if !a.decodePipelineBody(w, r, who, &body) {
		return
	}
	p, err := a.Pipelines.Place(r.Context(), who, r.PathValue("ns"), body.Workload, body.Repo, body.Path)
	if err != nil {
		a.failPipeline(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, p)
}

func (a *API) listRuns(w http.ResponseWriter, r *http.Request, who orders.Requester) {
	if !a.pipelineReads.allow(who.Sub) {
		tooMany(w)
		return
	}
	list, err := a.Pipelines.ListRuns(r.Context(), who, r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		a.failPipeline(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": list})
}

// runBody has no subject: who asked comes from the session, and an unknown field is refused.
type runBody struct {
	Ref string `json:"ref"`
}

func (a *API) placeRun(w http.ResponseWriter, r *http.Request, who orders.Requester) {
	var body runBody
	if !a.decodePipelineBody(w, r, who, &body) {
		return
	}
	run, err := a.Pipelines.PlaceRun(r.Context(), who, r.PathValue("ns"), r.PathValue("name"), body.Ref)
	if err != nil {
		a.failPipeline(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (a *API) failPipeline(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, pipelines.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, pipelines.ErrExists):
		writeError(w, http.StatusConflict, "a pipeline for this workload already exists in that namespace")
	case errors.Is(err, pipelines.ErrRunLimit):
		writeError(w, http.StatusTooManyRequests, "this namespace has reached its limit of 100 delivery runs; the portal cannot delete runs, ask the platform team to clean up")
	case errors.Is(err, pipelines.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, pipelines.ErrUpstream):
		writeError(w, http.StatusBadGateway, "the cluster request failed, try again")
	default:
		slog.Error("pipelines api error", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}
