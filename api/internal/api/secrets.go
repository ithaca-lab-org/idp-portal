package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"

	"github.com/ithaca-lab-org/idp-portal/api/internal/orders"
	"github.com/ithaca-lab-org/idp-portal/api/internal/secrets"
)

const (
	maxSecretBody         = 128 << 10
	secretWritesPerMinute = 20
	secretReadsPerMinute  = 120
)

func (a *API) secretRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/namespaces", a.secretsOn(a.user(a.listNamespaces)))
	mux.HandleFunc("GET /api/namespaces/{ns}/secrets", a.secretsOn(a.user(a.listSecrets)))
	mux.HandleFunc("PUT /api/namespaces/{ns}/secrets/{name}", a.secretsOn(a.user(a.putSecret)))
	mux.HandleFunc("DELETE /api/namespaces/{ns}/secrets/{name}", a.secretsOn(a.user(a.deleteSecret)))
}

// secretsOn answers 404 for every secrets route while the feature is off.
func (a *API) secretsOn(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.Secrets == nil {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		next(w, r)
	}
}

func (a *API) listNamespaces(w http.ResponseWriter, r *http.Request, who orders.Requester) {
	if !a.secretReads.allow(who.Sub) {
		tooMany(w)
		return
	}
	list, err := a.Secrets.ListNamespaces(r.Context(), who)
	if err != nil {
		a.failSecret(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"namespaces": list})
}

func (a *API) listSecrets(w http.ResponseWriter, r *http.Request, who orders.Requester) {
	if !a.secretReads.allow(who.Sub) {
		tooMany(w)
		return
	}
	list, err := a.Secrets.List(r.Context(), who, r.PathValue("ns"))
	if err != nil {
		a.failSecret(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"secrets": list})
}

type secretBody struct {
	Data map[string]string `json:"data"`
}

func (a *API) putSecret(w http.ResponseWriter, r *http.Request, who orders.Requester) {
	if !a.Auth.SameOrigin(r) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	if !a.secretWrites.allow(who.Sub) {
		tooMany(w)
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSecretBody))
	dec.DisallowUnknownFields()
	var body secretBody
	if err := dec.Decode(&body); err != nil {
		// never echo the decoder error: it can quote parts of the body
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
	res, err := a.Secrets.Put(r.Context(), who, r.PathValue("ns"), r.PathValue("name"), body.Data)
	if err != nil {
		a.failSecret(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *API) deleteSecret(w http.ResponseWriter, r *http.Request, who orders.Requester) {
	if !a.Auth.SameOrigin(r) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	if !a.secretWrites.allow(who.Sub) {
		tooMany(w)
		return
	}
	if err := a.Secrets.Delete(r.Context(), who, r.PathValue("ns"), r.PathValue("name")); err != nil {
		a.failSecret(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func tooMany(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "60")
	writeError(w, http.StatusTooManyRequests, "too many requests, slow down")
}

func (a *API) failSecret(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, secrets.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error()) // validation messages never contain a value
	case errors.Is(err, secrets.ErrUpstream):
		writeError(w, http.StatusBadGateway, "the secret backend is unavailable, try again")
	default:
		slog.Error("secrets api error", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}
