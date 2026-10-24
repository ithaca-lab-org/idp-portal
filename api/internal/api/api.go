// Package api serves the portal's JSON API. Every route needs a verified session; the caller's
// identity (email and groups) always comes from the session, never from the request.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/ithaca-lab-org/idp-portal/api/internal/auth"
	"github.com/ithaca-lab-org/idp-portal/api/internal/orders"
)

const (
	maxBody        = 8 << 10
	requestTimeout = 15 * time.Second
	defaultLimit   = 50
	maxLimit       = 100
)

// six digits at least: the counter is not capped at 999999 a year
var orderIDRe = regexp.MustCompile(`^ORD-[0-9]{4}-[0-9]{6,}$`)

type API struct {
	Auth    *auth.Handler
	Svc     *orders.Service
	limiter *limiter
}

// New builds the API with a per-user limit on order placement: every attempt consumes an order
// number and a database write, so it must not be unbounded.
func New(a *auth.Handler, svc *orders.Service) *API {
	return &API{Auth: a, Svc: svc, limiter: newLimiter(10, time.Minute)}
}

// limiter allows at most max events per key within a sliding window.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string][]time.Time
	now    func() time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, hits: map[string][]time.Time{}, now: time.Now}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cutoff := now.Add(-l.window)
	recent := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	if len(recent) >= l.max {
		l.hits[key] = recent
		return false
	}
	l.hits[key] = append(recent, now)
	if len(l.hits) > 1024 { // keep memory bounded: drop keys with no recent activity
		for k, v := range l.hits {
			if len(v) == 0 || !v[len(v)-1].After(cutoff) {
				delete(l.hits, k)
			}
		}
	}
	return true
}

func (a *API) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/business-units", a.user(a.businessUnits))
	mux.HandleFunc("POST /api/orders/namespaces", a.user(a.placeNamespace))
	mux.HandleFunc("GET /api/orders", a.user(a.listOrders))
	mux.HandleFunc("GET /api/orders/{id}", a.user(a.getOrder))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (a *API) user(next func(http.ResponseWriter, *http.Request, orders.Requester)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := a.Auth.UserFrom(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		next(w, r.WithContext(ctx), orders.Requester{Sub: u.Sub, Email: u.Email, Groups: u.Groups})
	}
}

func (a *API) businessUnits(w http.ResponseWriter, _ *http.Request, who orders.Requester) {
	groups := who.Groups
	if groups == nil {
		groups = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"businessUnits": groups})
}

type namespaceBody struct {
	Name         string `json:"name"`
	BusinessUnit string `json:"businessUnit"`
	Size         string `json:"size"`
}

func (a *API) placeNamespace(w http.ResponseWriter, r *http.Request, who orders.Requester) {
	if !a.Auth.SameOrigin(r) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	if !a.limiter.allow(who.Sub) {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "too many orders, slow down")
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	var body namespaceBody
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
	o, err := a.Svc.PlaceNamespaceOrder(r.Context(), who, orders.NamespaceRequest{
		Name: body.Name, BusinessUnit: body.BusinessUnit, Size: body.Size})
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, o)
}

func (a *API) listOrders(w http.ResponseWriter, r *http.Request, who orders.Requester) {
	limit := defaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive number")
			return
		}
		limit = min(n, maxLimit)
	}
	list, err := a.Svc.ListOrders(r.Context(), who, limit)
	if err != nil {
		a.fail(w, err)
		return
	}
	if list == nil {
		list = []orders.Order{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"orders": list})
}

func (a *API) getOrder(w http.ResponseWriter, r *http.Request, who orders.Requester) {
	id := r.PathValue("id")
	if !orderIDRe.MatchString(id) {
		writeError(w, http.StatusNotFound, "order not found")
		return
	}
	o, err := a.Svc.GetOrder(r.Context(), who, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (a *API) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, orders.ErrForbidden):
		writeError(w, http.StatusForbidden, "you cannot order for that business unit")
	case errors.Is(err, orders.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, orders.ErrCap):
		writeError(w, http.StatusTooManyRequests, "order limit reached, try again after an existing order is removed")
	case errors.Is(err, orders.ErrNameTaken):
		writeError(w, http.StatusConflict, "a namespace with that name already exists or is already ordered")
	case errors.Is(err, orders.ErrNotFound):
		writeError(w, http.StatusNotFound, "order not found")
	case errors.Is(err, orders.ErrCluster):
		writeError(w, http.StatusBadGateway, "the cluster request failed, the order was marked FAILED")
	default:
		slog.Error("api error", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}
