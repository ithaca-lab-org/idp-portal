package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"

	"github.com/ithaca-lab-org/idp-portal/api/internal/orders"
	"github.com/ithaca-lab-org/idp-portal/api/internal/workloads"
)

const (
	workloadWritesPerMinute = 10
	workloadReadsPerMinute  = 120
)

func (a *API) workloadRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/workloads/namespaces", a.workloadsOn(a.user(a.workloadNamespaces)))
	mux.HandleFunc("POST /api/orders/workloads", a.workloadsOn(a.user(a.placeWorkload)))
	mux.HandleFunc("GET /api/workloads", a.workloadsOn(a.user(a.listWorkloads)))
	mux.HandleFunc("GET /api/workloads/{ns}/{name}", a.workloadsOn(a.user(a.getWorkload)))
}

// workloadsOn answers 404 for every workloads route while the feature is off.
func (a *API) workloadsOn(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.Workloads == nil {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		next(w, r)
	}
}

func (a *API) workloadNamespaces(w http.ResponseWriter, r *http.Request, who orders.Requester) {
	if !a.workloadReads.allow(who.Sub) {
		tooMany(w)
		return
	}
	list, err := a.Workloads.ListNamespaces(r.Context(), who)
	if err != nil {
		a.failWorkload(w, err)
		return
	}
	// the naming rules travel with the list so the form validates exactly like the API
	writeJSON(w, http.StatusOK, map[string]any{"namespaces": list,
		"reservedNames": workloads.ReservedNames, "reservedPrefixes": workloads.ReservedPrefixes})
}

type workloadBody struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (a *API) placeWorkload(w http.ResponseWriter, r *http.Request, who orders.Requester) {
	if !a.Auth.SameOrigin(r) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	if !a.workloadWrites.allow(who.Sub) {
		tooMany(w)
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	var body workloadBody
	if err := dec.Decode(&body); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	wl, err := a.Workloads.Place(r.Context(), who, body.Namespace, body.Name)
	if err != nil {
		a.failWorkload(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, wl)
}

func (a *API) listWorkloads(w http.ResponseWriter, r *http.Request, who orders.Requester) {
	if !a.workloadReads.allow(who.Sub) {
		tooMany(w)
		return
	}
	q := r.URL.Query()
	if len(q) != 1 || len(q["namespace"]) != 1 || q.Get("namespace") == "" {
		writeError(w, http.StatusBadRequest, "namespace is required, and the only query parameter")
		return
	}
	list, err := a.Workloads.List(r.Context(), who, q.Get("namespace"))
	if err != nil {
		a.failWorkload(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workloads": list})
}

func (a *API) getWorkload(w http.ResponseWriter, r *http.Request, who orders.Requester) {
	if !a.workloadReads.allow(who.Sub) {
		tooMany(w)
		return
	}
	wl, err := a.Workloads.Get(r.Context(), who, r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		a.failWorkload(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wl)
}

func (a *API) failWorkload(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, workloads.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, workloads.ErrExists):
		writeError(w, http.StatusConflict, "a workload with this name already exists in that namespace")
	case errors.Is(err, workloads.ErrLimit):
		writeError(w, http.StatusConflict, "this namespace has reached its limit of 50 workloads")
	case errors.Is(err, workloads.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, workloads.ErrUpstream):
		writeError(w, http.StatusBadGateway, "the cluster request failed, try again")
	default:
		slog.Error("workloads api error", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}
