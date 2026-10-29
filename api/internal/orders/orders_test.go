package orders

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

type fakeCluster struct {
	mu        sync.Mutex
	items     map[string]*TeamNamespaceInfo
	createErr error
	getErr    error
	created   []string
}

func newFakeCluster() *fakeCluster { return &fakeCluster{items: map[string]*TeamNamespaceInfo{}} }

func (f *fakeCluster) CreateTeamNamespace(_ context.Context, name, owner, bu, size, orderID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return f.createErr
	}
	if _, ok := f.items[name]; ok {
		return ErrNameTaken
	}
	f.items[name] = &TeamNamespaceInfo{Phase: "Pending", OrderID: orderID}
	f.created = append(f.created, owner+"|"+bu+"|"+size+"|"+orderID)
	return nil
}

func (f *fakeCluster) TeamNamespace(_ context.Context, name string) (*TeamNamespaceInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	i, ok := f.items[name]
	if !ok {
		return nil, ErrNotFound
	}
	c := *i
	return &c, nil
}

func (f *fakeCluster) set(name, phase, msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[name].Phase, f.items[name].Message = phase, msg
}

var alice = Requester{Sub: "sub-alice", Email: "alice@example.com", Groups: []string{"payment-bu", "web-bu"}}

func newSvc() (*Service, *MemStore, *fakeCluster) {
	st, cl := NewMemStore(), newFakeCluster()
	return NewService(st, cl, 10, 5), st, cl
}

func TestValidateName(t *testing.T) {
	good := []string{"a", "payments-team", "team1", strings.Repeat("a", 40)}
	bad := []string{"", "Upper", "-lead", "trail-", "has_underscore", "has.dot", strings.Repeat("a", 41),
		"default", "argocd", "cert-manager", "tailscale", "identity", "idp-system", "echo", "vault", "vault-demo", "vault-secrets-operator",
		"kube-system", "kube-x", "istio-system", "istio-ingress", "gke-managed"}
	for _, n := range good {
		if err := ValidateName(n); err != nil {
			t.Errorf("%q should be valid: %v", n, err)
		}
	}
	for _, n := range bad {
		if err := ValidateName(n); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q should be rejected, got %v", n, err)
		}
	}
}

func TestReservedListMatchesOperator(t *testing.T) {
	src, err := os.ReadFile("../../../operator/internal/controller/resources.go")
	if err != nil {
		t.Fatalf("cannot read the operator source to compare reserved names: %v", err)
	}
	text := string(src)
	block := regexp.MustCompile(`(?s)var reservedNames = map\[string\]bool\{(.*?)\n\}`).FindStringSubmatch(text)
	if block == nil {
		t.Fatal("reservedNames not found in the operator source")
	}
	var names []string
	for _, m := range regexp.MustCompile(`"([^"]+)":\s*true`).FindAllStringSubmatch(block[1], -1) {
		names = append(names, m[1])
	}
	pre := regexp.MustCompile(`var reservedPrefixes = \[\]string\{([^}]*)\}`).FindStringSubmatch(text)
	if pre == nil {
		t.Fatal("reservedPrefixes not found in the operator source")
	}
	var prefixes []string
	for _, m := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(pre[1], -1) {
		prefixes = append(prefixes, m[1])
	}
	sort.Strings(names)
	sort.Strings(prefixes)
	gotN, gotP := append([]string(nil), ReservedNames...), append([]string(nil), ReservedPrefixes...)
	sort.Strings(gotN)
	sort.Strings(gotP)
	if strings.Join(names, ",") != strings.Join(gotN, ",") || strings.Join(prefixes, ",") != strings.Join(gotP, ",") {
		t.Fatalf("reserved lists drifted from the operator: operator names=%v prefixes=%v, api names=%v prefixes=%v", names, prefixes, gotN, gotP)
	}
}

func TestValidateOtherFields(t *testing.T) {
	if ValidateSize("large") == nil || ValidateSize("") == nil || ValidateSize("small") != nil || ValidateSize("medium") != nil {
		t.Error("size validation")
	}
	if ValidateBusinessUnit("Payment BU") == nil || ValidateBusinessUnit("payment-bu") != nil {
		t.Error("business unit validation")
	}
	if ValidateEmail("not-an-email") || ValidateEmail("") || !ValidateEmail("alice@example.com") {
		t.Error("email validation")
	}
}

func TestPlaceOrderSuccess(t *testing.T) {
	svc, _, cl := newSvc()
	o, err := svc.PlaceNamespaceOrder(context.Background(), alice, NamespaceRequest{Name: "payments-team", BusinessUnit: "payment-bu", Size: "small"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(o.ID, "ORD-") || o.Status != StatusPending || o.Owner != "alice@example.com" {
		t.Fatalf("order %+v", o)
	}
	if len(cl.created) != 1 || cl.created[0] != "alice@example.com|payment-bu|small|"+o.ID {
		t.Fatalf("resource created with %v", cl.created)
	}
}

func TestPlaceOrderAuthorization(t *testing.T) {
	svc, st, cl := newSvc()
	ctx := context.Background()
	for name, tc := range map[string]struct {
		who Requester
		req NamespaceRequest
		err error
	}{
		"business unit not in groups":   {alice, NamespaceRequest{"x-team", "other-bu", "small"}, ErrForbidden},
		"case-sensitive group match":    {alice, NamespaceRequest{"x-team", "Payment-BU", "small"}, ErrForbidden},
		"empty groups":                  {Requester{Sub: "sub-a", Email: "a@example.com"}, NamespaceRequest{"x-team", "payment-bu", "small"}, ErrForbidden},
		"missing subject":               {Requester{Email: "a@example.com", Groups: []string{"payment-bu"}}, NamespaceRequest{"x-team", "payment-bu", "small"}, ErrForbidden},
		"missing email":                 {Requester{Sub: "sub-x", Groups: []string{"payment-bu"}}, NamespaceRequest{"x-team", "payment-bu", "small"}, ErrForbidden},
		"malformed email":               {Requester{Sub: "sub-x", Email: "nope", Groups: []string{"payment-bu"}}, NamespaceRequest{"x-team", "payment-bu", "small"}, ErrForbidden},
		"reserved name":                 {alice, NamespaceRequest{"kube-system", "payment-bu", "small"}, ErrInvalid},
		"invalid name":                  {alice, NamespaceRequest{"Bad_Name", "payment-bu", "small"}, ErrInvalid},
		"invalid size":                  {alice, NamespaceRequest{"x-team", "payment-bu", "huge"}, ErrInvalid},
		"group that is not a DNS label": {Requester{Sub: "sub-a", Email: "a@example.com", Groups: []string{"Payment BU"}}, NamespaceRequest{"x-team", "Payment BU", "small"}, ErrInvalid},
	} {
		if _, err := svc.PlaceNamespaceOrder(ctx, tc.who, tc.req); !errors.Is(err, tc.err) {
			t.Errorf("%s: want %v, got %v", name, tc.err, err)
		}
	}
	if len(cl.created) != 0 {
		t.Fatalf("rejected requests must never reach the cluster: %v", cl.created)
	}
	if n, _ := st.CountActiveByOwner(ctx, "sub-alice"); n != 0 {
		t.Fatalf("rejected requests must not be stored, %d stored", n)
	}
}

func TestCaps(t *testing.T) {
	svc, _, _ := newSvc()
	ctx := context.Background()
	svc.CapPerUser = 2
	for _, n := range []string{"one", "two"} {
		if _, err := svc.PlaceNamespaceOrder(ctx, alice, NamespaceRequest{n, "payment-bu", "small"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.PlaceNamespaceOrder(ctx, alice, NamespaceRequest{"three", "payment-bu", "small"}); !errors.Is(err, ErrCap) {
		t.Fatalf("per-user cap: %v", err)
	}
	svc2, _, _ := newSvc()
	svc2.CapPerBU = 1
	bob := Requester{Sub: "sub-bob", Email: "bob@example.com", Groups: []string{"payment-bu"}}
	if _, err := svc2.PlaceNamespaceOrder(ctx, alice, NamespaceRequest{"one", "payment-bu", "small"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc2.PlaceNamespaceOrder(ctx, bob, NamespaceRequest{"two", "payment-bu", "small"}); !errors.Is(err, ErrCap) {
		t.Fatalf("per-business-unit cap: %v", err)
	}
}

func TestDuplicateNameAndClusterFailure(t *testing.T) {
	svc, st, cl := newSvc()
	ctx := context.Background()
	if _, err := svc.PlaceNamespaceOrder(ctx, alice, NamespaceRequest{"dup", "payment-bu", "small"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PlaceNamespaceOrder(ctx, alice, NamespaceRequest{"dup", "payment-bu", "small"}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate: %v", err)
	}
	// resource exists in the cluster but there is no active order for it
	cl.items["taken"] = &TeamNamespaceInfo{Phase: "Ready", OrderID: "ORD-0-1"}
	if _, err := svc.PlaceNamespaceOrder(ctx, alice, NamespaceRequest{"taken", "payment-bu", "small"}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("existing resource: %v", err)
	}
	cl.createErr = errors.New("apiserver on fire: secret details")
	_, err := svc.PlaceNamespaceOrder(ctx, alice, NamespaceRequest{"boom", "payment-bu", "small"})
	if !errors.Is(err, ErrCluster) || strings.Contains(err.Error(), "secret details") {
		t.Fatalf("cluster failure must be generic: %v", err)
	}
	list, _ := st.ListByOwner(ctx, alice.Sub, 10)
	failed := 0
	for _, o := range list {
		if o.Status == StatusFailed {
			failed++
			if strings.Contains(o.Message, "secret details") {
				t.Fatal("internal error text leaked into the order message")
			}
		}
	}
	if failed != 2 {
		t.Fatalf("both failed attempts must be marked FAILED, got %d", failed)
	}
	if _, err := svc.PlaceNamespaceOrder(ctx, alice, NamespaceRequest{"boom", "payment-bu", "small"}); errors.Is(err, ErrNameTaken) {
		t.Fatal("a FAILED order must not block reordering the same name")
	}
}

func TestOrdersAreOwnerScopedAndStatusRefreshes(t *testing.T) {
	svc, _, cl := newSvc()
	ctx := context.Background()
	o, _ := svc.PlaceNamespaceOrder(ctx, alice, NamespaceRequest{"payments-team", "payment-bu", "small"})
	mallory := Requester{Sub: "sub-mallory", Email: "mallory@example.com", Groups: []string{"payment-bu"}}
	if _, err := svc.GetOrder(ctx, mallory, o.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user's order must look absent: %v", err)
	}
	if _, err := svc.GetOrder(ctx, alice, "ORD-2026-999999"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown order: %v", err)
	}
	if list, _ := svc.ListOrders(ctx, mallory, 10); len(list) != 0 {
		t.Fatal("list leaked another user's orders")
	}
	got, _ := svc.GetOrder(ctx, alice, o.ID)
	if got.Status != StatusPending {
		t.Fatalf("status %s", got.Status)
	}
	cl.set("payments-team", "Ready", "")
	if got, _ = svc.GetOrder(ctx, alice, o.ID); got.Status != StatusComplete {
		t.Fatalf("status after Ready: %s", got.Status)
	}
	o2, _ := svc.PlaceNamespaceOrder(ctx, alice, NamespaceRequest{"second", "payment-bu", "small"})
	cl.set("second", "Failed", "quota exceeded")
	if got, _ = svc.GetOrder(ctx, alice, o2.ID); got.Status != StatusFailed || got.Message != "quota exceeded" {
		t.Fatalf("failed order: %+v", got)
	}
}

func TestRefreshEdgeCases(t *testing.T) {
	svc, st, cl := newSvc()
	ctx := context.Background()
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	st.Now = func() time.Time { return now }
	svc.Now = func() time.Time { return now }

	o, _ := svc.PlaceNamespaceOrder(ctx, alice, NamespaceRequest{"vanish", "payment-bu", "small"})
	delete(cl.items, "vanish")
	if got, _ := svc.GetOrder(ctx, alice, o.ID); got.Status != StatusPending {
		t.Fatal("a missing resource is tolerated for a while")
	}
	now = now.Add(11 * time.Minute)
	if got, _ := svc.GetOrder(ctx, alice, o.ID); got.Status != StatusFailed {
		t.Fatalf("a resource that never appears must fail the order, got %s", got.Status)
	}

	o2, _ := svc.PlaceNamespaceOrder(ctx, alice, NamespaceRequest{"hijack", "payment-bu", "small"})
	cl.items["hijack"].OrderID = "ORD-1999-000001"
	cl.set("hijack", "Ready", "")
	if got, _ := svc.GetOrder(ctx, alice, o2.ID); got.Status != StatusFailed {
		t.Fatalf("a resource from another order must not complete this one, got %s", got.Status)
	}

	o3, _ := svc.PlaceNamespaceOrder(ctx, alice, NamespaceRequest{"flaky", "payment-bu", "small"})
	cl.getErr = errors.New("apiserver unreachable")
	got, err := svc.GetOrder(ctx, alice, o3.ID)
	if err != nil || got.Status != StatusPending {
		t.Fatalf("a cluster read error must not fail the read: %v %v", got, err)
	}
}

func TestSyncerUpdatesPendingAndStops(t *testing.T) {
	svc, st, cl := newSvc()
	ctx := context.Background()
	o, _ := svc.PlaceNamespaceOrder(ctx, alice, NamespaceRequest{"synced", "payment-bu", "small"})
	cl.set("synced", "Ready", "")

	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { svc.RunSyncer(rctx, 10*time.Millisecond, 0); close(done) }()
	deadline := time.After(2 * time.Second)
	for {
		got, _ := st.Get(ctx, o.ID)
		if got.Status == StatusComplete {
			break
		}
		select {
		case <-deadline:
			t.Fatal("syncer did not complete the order")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("syncer did not stop when the context was cancelled")
	}
}

func TestMemStoreNumbersAndNames(t *testing.T) {
	st := NewMemStore()
	st.Now = func() time.Time { return time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC) }
	ctx := context.Background()
	a := &Order{Kind: KindNamespace, Name: "a", Owner: "x@example.com", Status: StatusPending}
	b := &Order{Kind: KindNamespace, Name: "b", Owner: "x@example.com", Status: StatusPending}
	_ = st.Create(ctx, a)
	_ = st.Create(ctx, b)
	if a.ID != "ORD-2026-000001" || b.ID != "ORD-2026-000002" {
		t.Fatalf("ids %s %s", a.ID, b.ID)
	}
	if err := st.Create(ctx, &Order{Kind: KindNamespace, Name: "a", Status: StatusPending}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate: %v", err)
	}
	if err := st.UpdateStatus(ctx, a.ID, StatusPending, StatusComplete, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateStatus(ctx, a.ID, StatusPending, StatusFailed, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a finished order must not change again: %v", err)
	}
}

func TestDynamicClusterShape(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	c := DynamicCluster{Client: client}
	ctx := context.Background()
	if err := c.CreateTeamNamespace(ctx, "payments-team", "alice@example.com", "payment-bu", "small", "ORD-2026-000001"); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateTeamNamespace(ctx, "payments-team", "alice@example.com", "payment-bu", "small", "ORD-2026-000002"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("already exists must map to ErrNameTaken: %v", err)
	}
	obj, err := client.Resource(teamNamespaceGVR).Get(ctx, "payments-team", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	spec, _, _ := unstructured.NestedMap(obj.Object, "spec")
	if spec["owner"] != "alice@example.com" || spec["businessUnit"] != "payment-bu" || spec["size"] != "small" || spec["orderId"] != "ORD-2026-000001" {
		t.Fatalf("spec %v", spec)
	}
	_ = unstructured.SetNestedField(obj.Object, "Failed", "status", "phase")
	_ = unstructured.SetNestedSlice(obj.Object, []any{map[string]any{"type": "Ready", "message": "reserved"}}, "status", "conditions")
	if _, err := client.Resource(teamNamespaceGVR).Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	info, err := c.TeamNamespace(ctx, "payments-team")
	if err != nil || info.Phase != "Failed" || info.Message != "reserved" || info.OrderID != "ORD-2026-000001" {
		t.Fatalf("info %+v err %v", info, err)
	}
	if _, err := c.TeamNamespace(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestCapIsEnforcedUnderConcurrency(t *testing.T) {
	svc, st, _ := newSvc()
	svc.CapPerUser = 5
	svc.CapPerBU = 100
	ctx := context.Background()
	var wg sync.WaitGroup
	var ok, capped int32
	var mu sync.Mutex
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := svc.PlaceNamespaceOrder(ctx, alice, NamespaceRequest{fmt.Sprintf("burst-%d", i), "payment-bu", "medium"})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrCap):
				capped++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	n, _ := st.CountActiveByOwner(ctx, alice.Sub)
	if ok != 5 || n != 5 || capped != 55 {
		t.Fatalf("a burst of 60 must create exactly the cap (5): created %d, stored %d, refused %d", ok, n, capped)
	}
}

func TestOwnershipFollowsSubjectNotEmail(t *testing.T) {
	svc, _, cl := newSvc()
	ctx := context.Background()
	o, err := svc.PlaceNamespaceOrder(ctx, alice, NamespaceRequest{"payments-team", "payment-bu", "small"})
	if err != nil {
		t.Fatal(err)
	}
	// someone else ends up with alice's email address: they must not see her orders
	imposter := Requester{Sub: "sub-imposter", Email: "alice@example.com", Groups: []string{"payment-bu"}}
	if _, err := svc.GetOrder(ctx, imposter, o.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("same email, different subject must not read the order: %v", err)
	}
	if list, _ := svc.ListOrders(ctx, imposter, 10); len(list) != 0 {
		t.Fatal("same email, different subject must not list the orders")
	}
	// alice changes her email: she still owns her orders and her caps follow her
	renamed := Requester{Sub: "sub-alice", Email: "alice.new@example.com", Groups: []string{"payment-bu"}}
	if _, err := svc.GetOrder(ctx, renamed, o.ID); err != nil {
		t.Fatalf("an email change must not lock the owner out: %v", err)
	}
	svc.CapPerUser = 1
	if _, err := svc.PlaceNamespaceOrder(ctx, renamed, NamespaceRequest{"another", "payment-bu", "small"}); !errors.Is(err, ErrCap) {
		t.Fatalf("changing the email must not reset the per-user cap: %v", err)
	}
	if len(cl.created) != 1 {
		t.Fatalf("created %v", cl.created)
	}
}

func TestEmailIsNormalised(t *testing.T) {
	svc, _, cl := newSvc()
	who := Requester{Sub: "sub-bill", Email: "  Bill.Paxton@Example.COM ", Groups: []string{"payment-bu"}}
	o, err := svc.PlaceNamespaceOrder(context.Background(), who, NamespaceRequest{"bills-team", "payment-bu", "small"})
	if err != nil {
		t.Fatal(err)
	}
	if o.Owner != "bill.paxton@example.com" || !strings.HasPrefix(cl.created[0], "bill.paxton@example.com|") {
		t.Fatalf("owner %q created %v", o.Owner, cl.created)
	}
}

func TestRemovedNamespaceReleasesTheOrder(t *testing.T) {
	svc, st, cl := newSvc()
	svc.CapPerUser = 1
	ctx := context.Background()
	o, _ := svc.PlaceNamespaceOrder(ctx, alice, NamespaceRequest{"payments-team", "payment-bu", "small"})
	cl.set("payments-team", "Ready", "")
	svc.SyncOnce(ctx, true)
	if got, _ := st.Get(ctx, o.ID); got.Status != StatusComplete {
		t.Fatalf("status %s", got.Status)
	}
	if _, err := svc.PlaceNamespaceOrder(ctx, alice, NamespaceRequest{"second", "payment-bu", "small"}); !errors.Is(err, ErrCap) {
		t.Fatalf("a COMPLETE order counts against the cap: %v", err)
	}
	delete(cl.items, "payments-team") // an admin deleted the namespace
	svc.SyncOnce(ctx, false)
	if got, _ := st.Get(ctx, o.ID); got.Status != StatusComplete {
		t.Fatal("COMPLETE orders are only re-checked on the slow tick")
	}
	svc.SyncOnce(ctx, true)
	if got, _ := st.Get(ctx, o.ID); got.Status != StatusReleased {
		t.Fatalf("status after removal: %s", got.Status)
	}
	if _, err := svc.PlaceNamespaceOrder(ctx, alice, NamespaceRequest{"payments-team", "payment-bu", "small"}); err != nil {
		t.Fatalf("a released order must free the name and the cap: %v", err)
	}
	// a cluster read error must not release anything
	o2, _ := svc.Store.Get(ctx, "ORD-"+fmt.Sprint(time.Now().UTC().Year())+"-000002")
	cl.set("payments-team", "Ready", "")
	svc.SyncOnce(ctx, true)
	cl.getErr = errors.New("apiserver unreachable")
	svc.SyncOnce(ctx, true)
	if got, _ := st.Get(ctx, o2.ID); got.Status != StatusComplete {
		t.Fatalf("a read error must not release an order, got %s", got.Status)
	}
}
