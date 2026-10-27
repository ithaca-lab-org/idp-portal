package orders

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Requester is the authenticated caller. Sub, Email and Groups come from the verified session
// only. Sub (the Keycloak subject) is the identity used for authorization and caps: the email is
// editable in the directory and is not verified, so it is only used as the namespace owner.
type Requester struct {
	Sub    string
	Email  string
	Groups []string
}

type NamespaceRequest struct {
	Name         string
	BusinessUnit string
	Size         string
}

const refreshTimeout = 10 * time.Second

type Service struct {
	Store          Store
	Cluster        Cluster
	CapPerBU       int
	CapPerUser     int
	PendingTimeout time.Duration // a PENDING order whose resource never appears fails after this
	Now            func() time.Time
	Events         Publisher // optional: nil disables event publishing

	// placeMu makes "count active orders, then insert" atomic for this process. The portal runs a
	// single replica; with more replicas the reservation would have to move into the database.
	placeMu sync.Mutex
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

// reserve checks the caps and stores the PENDING order under one lock, so concurrent requests
// cannot all pass the count before any of them is inserted.
func (s *Service) reserve(ctx context.Context, o *Order) error {
	s.placeMu.Lock()
	defer s.placeMu.Unlock()
	if n, err := s.Store.CountActiveByBusinessUnit(ctx, o.BusinessUnit); err != nil {
		return err
	} else if n >= s.CapPerBU {
		return ErrCap
	}
	if n, err := s.Store.CountActiveByOwner(ctx, o.OwnerSub); err != nil {
		return err
	} else if n >= s.CapPerUser {
		return ErrCap
	}
	return s.Store.Create(ctx, o)
}

// PlaceNamespaceOrder stores a PENDING order and creates the TeamNamespace for it. The owner is
// always the caller's session email (lowercased).
func (s *Service) PlaceNamespaceOrder(ctx context.Context, who Requester, req NamespaceRequest) (*Order, error) {
	email := strings.ToLower(strings.TrimSpace(who.Email))
	if who.Sub == "" || !ValidateEmail(email) {
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

	o := &Order{Kind: KindNamespace, Name: req.Name, BusinessUnit: req.BusinessUnit,
		Owner: email, OwnerSub: who.Sub, Size: req.Size, Status: StatusPending}
	if err := s.reserve(ctx, o); err != nil {
		return nil, err
	}
	if err := s.Cluster.CreateTeamNamespace(ctx, o.Name, o.Owner, o.BusinessUnit, o.Size, o.ID); err != nil {
		msg, out := "the cluster request failed", ErrCluster
		if errors.Is(err, ErrNameTaken) {
			msg, out = "a namespace with this name already exists", ErrNameTaken
		} else {
			slog.Error("create teamnamespace", "order", o.ID, "err", err)
		}
		if uerr := s.Store.UpdateStatus(ctx, o.ID, StatusPending, StatusFailed, msg); uerr != nil {
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
	if who.Sub == "" || o.OwnerSub != who.Sub {
		return nil, ErrNotFound
	}
	return s.refresh(ctx, o), nil
}

func (s *Service) ListOrders(ctx context.Context, who Requester, limit int) ([]Order, error) {
	if who.Sub == "" {
		return nil, ErrForbidden
	}
	list, err := s.Store.ListByOwner(ctx, who.Sub, limit)
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
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
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
	return s.transition(ctx, o, StatusPending, to, msg)
}

func (s *Service) transition(ctx context.Context, o *Order, from, to Status, msg string) *Order {
	err := s.Store.UpdateStatus(ctx, o.ID, from, to, msg)
	if err != nil && !errors.Is(err, ErrNotFound) {
		slog.Error("update order status", "order", o.ID, "err", err)
		return o
	}
	if err == nil { // ErrNotFound means another updater won; it publishes, not us
		s.emit(ctx, o, to, msg)
	}
	if fresh, err := s.Store.Get(ctx, o.ID); err == nil {
		return fresh
	}
	return o
}

// releaseIfGone marks a COMPLETE order RELEASED when its namespace resource no longer exists,
// which frees the name and the caps. It is only called from the poller.
func (s *Service) releaseIfGone(ctx context.Context, o *Order) {
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	if _, err := s.Cluster.TeamNamespace(ctx, o.Name); errors.Is(err, ErrNotFound) {
		s.transition(ctx, o, StatusComplete, StatusReleased, "the namespace was removed")
	} else if err != nil {
		slog.Warn("read teamnamespace", "order", o.ID, "err", err)
	}
}

// SyncOnce refreshes every PENDING order; with complete set it also releases COMPLETE orders
// whose namespace is gone.
func (s *Service) SyncOnce(ctx context.Context, complete bool) {
	pending, err := s.Store.ListByStatus(ctx, StatusPending, 200)
	if err != nil {
		slog.Warn("list pending orders", "err", err)
		return
	}
	for i := range pending {
		s.refresh(ctx, &pending[i])
	}
	if !complete {
		return
	}
	done, err := s.Store.ListByStatus(ctx, StatusComplete, 500)
	if err != nil {
		slog.Warn("list complete orders", "err", err)
		return
	}
	for i := range done {
		s.releaseIfGone(ctx, &done[i])
	}
}

// RunSyncer polls until ctx is cancelled: PENDING orders every interval, COMPLETE orders every
// completeEvery intervals. A poll costs one GET per order checked.
func (s *Service) RunSyncer(ctx context.Context, interval time.Duration, completeEvery int) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for tick := 1; ; tick++ {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.SyncOnce(ctx, completeEvery > 0 && tick%completeEvery == 0)
		}
	}
}

func logPublishFailure(order, typ string, err error) {
	slog.Error("publish order event", "order", order, "type", typ, "err", err)
}
