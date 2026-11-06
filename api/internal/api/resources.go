package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"

	"github.com/ithaca-lab-org/idp-portal/api/internal/orders"
	"github.com/ithaca-lab-org/idp-portal/api/internal/resources"
)

const (
	resourceWritesPerMinute = 10
	resourceReadsPerMinute  = 120
)

func (a *API) resourceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/resources/namespaces", a.resourcesOn(a.user(a.resourceNamespaces)))
	mux.HandleFunc("POST /api/orders/buckets", a.resourcesOn(a.user(a.placeBucket)))
	mux.HandleFunc("GET /api/buckets", a.resourcesOn(a.user(a.listBuckets)))
	mux.HandleFunc("GET /api/buckets/{ns}/{name}", a.resourcesOn(a.user(a.getBucket)))
	mux.HandleFunc("POST /api/orders/grants", a.resourcesOn(a.user(a.placeGrant)))
	mux.HandleFunc("GET /api/grants", a.resourcesOn(a.user(a.listGrants)))
	// The only delete: a grant can be revoked. There is no bucket delete in v1 (deleting a claim
	// can orphan a bucket that still holds data); the platform team retires buckets.
	mux.HandleFunc("DELETE /api/grants/{ns}/{name}", a.resourcesOn(a.user(a.revokeGrant)))
}

// resourcesOn answers 404 for every resources route while the feature is off (RESOURCES_ENABLED
// not true); nothing below it runs, so nothing reaches the cluster.
func (a *API) resourcesOn(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.Resources == nil {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		next(w, r)
	}
}

func (a *API) resourceNamespaces(w http.ResponseWriter, r *http.Request, who orders.Requester) {
	if !a.resourceReads.allow(who.Sub) {
		tooMany(w)
		return
	}
	list, err := a.Resources.ListNamespaces(r.Context(), who)
	if err != nil {
		a.failResource(w, err)
		return
	}
	// the rules travel with the list so the form validates exactly like the API
	writeJSON(w, http.StatusOK, map[string]any{"namespaces": list,
		"maxNamespaceLength": resources.MaxNamespaceLen, "maxNameLength": resources.MaxNameLen,
		"maxBuckets": resources.MaxBuckets, "maxGrants": resources.MaxGrants, "access": resources.AccessMenu})
}

// decodeResourceBody runs the checks every write shares (same origin, rate limit, JSON only, size,
// unknown fields) and decodes into v. It writes the error answer and returns false when one fails.
func (a *API) decodeResourceBody(w http.ResponseWriter, r *http.Request, who orders.Requester, v any) bool {
	if !a.Auth.SameOrigin(r) {
		writeError(w, http.StatusForbidden, "forbidden")
		return false
	}
	if !a.resourceWrites.allow(who.Sub) {
		tooMany(w)
		return false
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
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

type bucketBody struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (a *API) placeBucket(w http.ResponseWriter, r *http.Request, who orders.Requester) {
	var body bucketBody
	if !a.decodeResourceBody(w, r, who, &body) {
		return
	}
	b, err := a.Resources.PlaceBucket(r.Context(), who, body.Namespace, body.Name)
	if err != nil {
		a.failResource(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, b)
}

// namespaceQuery returns the namespace of a list request: required, and the only parameter, so
// there is no fan-out over every namespace.
func namespaceQuery(w http.ResponseWriter, r *http.Request) (string, bool) {
	q := r.URL.Query()
	if len(q) != 1 || len(q["namespace"]) != 1 || q.Get("namespace") == "" {
		writeError(w, http.StatusBadRequest, "namespace is required, and the only query parameter")
		return "", false
	}
	return q.Get("namespace"), true
}

func (a *API) listBuckets(w http.ResponseWriter, r *http.Request, who orders.Requester) {
	if !a.resourceReads.allow(who.Sub) {
		tooMany(w)
		return
	}
	ns, ok := namespaceQuery(w, r)
	if !ok {
		return
	}
	list, err := a.Resources.ListBuckets(r.Context(), who, ns)
	if err != nil {
		a.failResource(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"buckets": list})
}

func (a *API) getBucket(w http.ResponseWriter, r *http.Request, who orders.Requester) {
	if !a.resourceReads.allow(who.Sub) {
		tooMany(w)
		return
	}
	b, err := a.Resources.GetBucket(r.Context(), who, r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		a.failResource(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

type grantBody struct {
	Namespace string `json:"namespace"`
	Workload  string `json:"workload"`
	Bucket    string `json:"bucket"`
	Access    string `json:"access"`
}

func (a *API) placeGrant(w http.ResponseWriter, r *http.Request, who orders.Requester) {
	var body grantBody
	if !a.decodeResourceBody(w, r, who, &body) {
		return
	}
	g, err := a.Resources.PlaceGrant(r.Context(), who, body.Namespace, body.Workload, body.Bucket, body.Access)
	if err != nil {
		a.failResource(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, g)
}

func (a *API) listGrants(w http.ResponseWriter, r *http.Request, who orders.Requester) {
	if !a.resourceReads.allow(who.Sub) {
		tooMany(w)
		return
	}
	ns, ok := namespaceQuery(w, r)
	if !ok {
		return
	}
	list, err := a.Resources.ListGrants(r.Context(), who, ns)
	if err != nil {
		a.failResource(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"grants": list})
}

func (a *API) revokeGrant(w http.ResponseWriter, r *http.Request, who orders.Requester) {
	if !a.Auth.SameOrigin(r) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	if !a.resourceWrites.allow(who.Sub) {
		tooMany(w)
		return
	}
	if err := a.Resources.RevokeGrant(r.Context(), who, r.PathValue("ns"), r.PathValue("name")); err != nil {
		a.failResource(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) failResource(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, resources.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, resources.ErrNamespaceUnsupported):
		writeError(w, http.StatusUnprocessableEntity, "this namespace name is too long to own buckets (at most 31 characters)")
	case errors.Is(err, resources.ErrBucketExists):
		writeError(w, http.StatusConflict, "a bucket with this name already exists in that namespace")
	case errors.Is(err, resources.ErrGrantExists):
		writeError(w, http.StatusConflict, "this workload already has access to that bucket; revoke it first to change the access")
	case errors.Is(err, resources.ErrGrantCollision):
		writeError(w, http.StatusConflict, "this grant name collides with an existing grant; pick different names")
	case errors.Is(err, resources.ErrBucketLimit):
		writeError(w, http.StatusConflict, "this namespace has reached its limit of 3 buckets")
	case errors.Is(err, resources.ErrGrantLimit):
		writeError(w, http.StatusConflict, "this namespace has reached its limit of 10 grants")
	case errors.Is(err, resources.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, resources.ErrUpstream):
		writeError(w, http.StatusBadGateway, "the cluster request failed, try again")
	default:
		slog.Error("resources api error", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}
