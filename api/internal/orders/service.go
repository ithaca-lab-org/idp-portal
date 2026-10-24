package orders

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"
)

// Requester is the authenticated caller. Email and Groups come from the verified session only.
type Requester struct {
	Email  string
	Groups []string
}

type NamespaceRequest struct {
	Name         string
	BusinessUnit string
	Size         string
}

type Service struct {
	Store          Store
	Cluster        Cluster
	CapPerBU       int
	CapPerUser     int
	PendingTimeout time.Duration // a PENDING order whose resource never appears fails after this
	Now            func() time.Time
}

func NewService(store Store, cluster Cluster, capPerBU, capPerUser int) *Service {
	return &Service{Store: store, Cluster: cluster, CapPerBU: capPerBU, CapPerUser: capPerUser,
		PendingTimeout: 10 * time.Minute, Now: time.Now}
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// PlaceNamespaceOrder stores a PENDING order and creates the TeamNamespace for it. The owner is
// always the caller's session email. The caps are checked before the insert without a lock, so
// two simultaneous requests can exceed a cap by a small amount.
func (s *Service) PlaceNamespaceOrder(ctx context.Context, who Requester, req NamespaceRequest) (*Order, error) {
	if !ValidateEmail(who.Email) {
		return nil, ErrForbidden
	}
	if !contains(who.Groups, req.BusinessUnit) {
		return nil, ErrForbidden
	}
	if err := ValidateBusinessUnit(req.BusinessUnit); err != nil {
		return nil, err
	}
	if err := ValidateName(req.Name); err != nil {
		return nil, err
	}
	if err := ValidateSize(req.Size); err != nil {
		return nil, err
	}
	if n, err := s.Store.CountActiveByBusinessUnit(ctx, req.BusinessUnit); err != nil {
		return nil, err
	} else if n >= s.CapPerBU {
		return nil, ErrCap
	}
	if n, err := s.Store.CountActiveByOwner(ctx, who.Email); err != nil {
		return nil, err
	} else if n >= s.CapPerUser {
		return nil, ErrCap
	}

	o := &Order{Kind: KindNamespace, Name: req.Name, BusinessUnit: req.BusinessUnit,
		Owner: who.Email, Size: req.Size, Status: StatusPending}
	if err := s.Store.Create(ctx, o); err != nil {
		return nil, err
	}
	if err := s.Cluster.CreateTeamNamespace(ctx, o.Name, o.Owner, o.BusinessUnit, o.Size, o.ID); err != nil {
		msg, out := "the cluster request failed", ErrCluster
		if errors.Is(err, ErrNameTaken) {
			msg, out = "a namespace with this name already exists", ErrNameTaken
		} else {
			slog.Error("create teamnamespace", "order", o.ID, "err", err)
		}
		if uerr := s.Store.UpdateStatus(ctx, o.ID, StatusFailed, msg); uerr != nil {
			slog.Error("mark order failed", "order", o.ID, "err", uerr)
		}
		return nil, out
	}
	return o, nil
}

// GetOrder returns the caller's own order. Someone else's order is reported as not found so
// order ids cannot be enumerated.
func (s *Service) GetOrder(ctx context.Context, who Requester, id string) (*Order, error) {
	o, err := s.Store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(o.Owner, who.Email) {
		return nil, ErrNotFound
	}
	return s.refresh(ctx, o), nil
}

func (s *Service) ListOrders(ctx context.Context, who Requester, limit int) ([]Order, error) {
	list, err := s.Store.ListByOwner(ctx, who.Email, limit)
	if err != nil {
		return nil, err
	}
	for i := range list {
		list[i] = *s.refresh(ctx, &list[i])
	}
	return list, nil
}

// refresh brings a PENDING order in step with its resource. Failures to read the cluster leave
// the order unchanged: a read must not fail because the cluster is briefly unreachable.
func (s *Service) refresh(ctx context.Context, o *Order) *Order {
	if o.Status != StatusPending {
		return o
	}
	to, msg := StatusPending, ""
	info, err := s.Cluster.TeamNamespace(ctx, o.Name)
	switch {
	case errors.Is(err, ErrNotFound):
		if s.Now().Sub(o.CreatedAt) > s.PendingTimeout {
			to, msg = StatusFailed, "the namespace request was not found in the cluster"
		}
	case err != nil:
		slog.Warn("read teamnamespace", "order", o.ID, "err", err)
	case info.OrderID != o.ID:
		to, msg = StatusFailed, "the namespace belongs to a different order"
	case info.Phase == "Ready":
		to = StatusComplete
	case info.Phase == "Failed":
		to, msg = StatusFailed, info.Message
		if msg == "" {
			msg = "the namespace could not be created"
		}
	}
	if to == StatusPending {
		return o
	}
	if err := s.Store.UpdateStatus(ctx, o.ID, to, msg); err != nil && !errors.Is(err, ErrNotFound) {
		slog.Error("update order status", "order", o.ID, "err", err)
		return o
	}
	if fresh, err := s.Store.Get(ctx, o.ID); err == nil {
		return fresh
	}
	return o
}

// SyncOnce refreshes every PENDING order.
func (s *Service) SyncOnce(ctx context.Context) {
	pending, err := s.Store.ListPending(ctx, 200)
	if err != nil {
		slog.Warn("list pending orders", "err", err)
		return
	}
	for i := range pending {
		s.refresh(ctx, &pending[i])
	}
}

// RunSyncer polls until ctx is cancelled. A poll over PENDING orders only is enough for a single
// replica: it costs one GET per pending order.
func (s *Service) RunSyncer(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.SyncOnce(ctx)
		}
	}
}
