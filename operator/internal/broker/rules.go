package broker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
)

// Naming, ownership and role rules of ADR 0010. Everything the broker may do
// to a bucket or a binding is decided here, so this file is the one to review.
// Nothing outside the fixed role menu and the idp-<ns>-<claim> names is ever
// written.
const (
	BucketPrefix = "idp-"

	// Longest names that keep idp-<namespace>-<claim>-<hash> within the 63
	// characters of a bucket name: 4 + 31 + 1 + 18 + 1 + 8 = 63. A TeamNamespace
	// may have up to 40 characters; one longer than MaxNamespaceLen cannot own
	// buckets (InvalidName). The claim limit is in the BucketClaim CRD.
	MaxNamespaceLen = 31
	MaxClaimLen     = 18
	MaxBucketLen    = 63
	hashLen         = 8

	LabelManaged    = "idp-managed"
	LabelNamespace  = "idp-namespace"
	LabelClaim      = "idp-claim"
	LabelClaimUID   = "idp-claim-uid"
	LabelReleasing  = "idp-releasing"
	LabelOrphaned   = "idp-orphaned"
	LabelOrphanedAt = "idp-orphaned-at"

	RoleReader = "roles/storage.objectViewer"
	RoleWriter = "roles/storage.objectUser"
)

// BucketName is the bucket of claim in namespace:
// idp-<namespace>-<claim>-<8 hex of sha256(projectNumber/namespace/claim)>.
// The hash makes (a-b, c) and (a, b-c) different buckets and the name hard to
// guess for someone squatting names in another project. It refuses anything
// that is not a DNS-1123 label within the limits, and names Cloud Storage
// would refuse (containing "google", or a part starting with "goog"), so the
// result is always a valid bucket name that starts with BucketPrefix.
func BucketName(projectNumber, namespace, claim string) (string, error) {
	if len(validation.IsDNS1123Label(namespace)) != 0 || len(namespace) > MaxNamespaceLen {
		return "", fmt.Errorf("namespace %q is not a DNS-1123 label of at most %d characters, so it cannot own buckets", namespace, MaxNamespaceLen)
	}
	if len(validation.IsDNS1123Label(claim)) != 0 || len(claim) > MaxClaimLen {
		return "", fmt.Errorf("claim %q is not a DNS-1123 label of at most %d characters", claim, MaxClaimLen)
	}
	for _, p := range []string{namespace, claim} {
		if strings.HasPrefix(p, "goog") || strings.Contains(p, "google") {
			return "", fmt.Errorf("%q cannot be part of a bucket name (Cloud Storage refuses names with \"google\" or starting with \"goog\")", p)
		}
	}
	sum := sha256.Sum256([]byte(projectNumber + "/" + namespace + "/" + claim))
	name := BucketPrefix + namespace + "-" + claim + "-" + hex.EncodeToString(sum[:])[:hashLen]
	if len(name) > MaxBucketLen || strings.Contains(name, "google") {
		return "", fmt.Errorf("bucket name %q is not valid", name)
	}
	return name, nil
}

// OwnerLabels are the labels every broker bucket is created with. The claim
// UID ties the bucket to one incarnation of the claim: a claim re-created
// under the same name never adopts the old bucket.
func OwnerLabels(namespace, claim, uid string) map[string]string {
	return map[string]string{LabelManaged: "true", LabelNamespace: namespace, LabelClaim: claim, LabelClaimUID: uid}
}

// Ownership is what a bucket's project and labels say about a claim.
type Ownership int

const (
	// Foreign: not in our project, or not labelled for this namespace, claim
	// and claim UID (or not by the broker at all). Never touched.
	Foreign Ownership = iota
	// Owned: created by the broker for exactly this claim.
	Owned
	// Releasing: owned, and the claim's deletion has started; no grant may
	// bind anything any more.
	Releasing
	// Orphaned: owned, but retired when the claim was deleted with objects
	// inside (or the release gave up). Never adopted again.
	Orphaned
)

// Classify decides what the broker may do with an existing bucket for the
// claim (namespace, name, uid). Every check of ownership goes through here, so
// a bucket of another project with copied labels is Foreign.
func Classify(info *BucketInfo, projectNumber, namespace, claim, uid string) Ownership {
	l := info.Labels
	switch {
	case projectNumber == "" || info.ProjectNumber != projectNumber:
		return Foreign
	case uid == "" || l[LabelManaged] != "true" || l[LabelNamespace] != namespace || l[LabelClaim] != claim || l[LabelClaimUID] != uid:
		return Foreign
	case l[LabelOrphaned] == "true":
		return Orphaned
	case l[LabelReleasing] == "true":
		return Releasing
	}
	return Owned
}

// ReleasingLabels mark a bucket whose claim is being deleted.
func ReleasingLabels() map[string]string { return map[string]string{LabelReleasing: "true"} }

// OrphanLabels marks a bucket as retired at the given time (Unix seconds).
func OrphanLabels(now time.Time) map[string]string {
	return map[string]string{LabelOrphaned: "true", LabelOrphanedAt: strconv.FormatInt(now.Unix(), 10)}
}

// RoleFor maps an access level to its role. This switch is the whole menu.
func RoleFor(access string) (string, bool) {
	switch access {
	case idpv1alpha1.AccessReader:
		return RoleReader, true
	case idpv1alpha1.AccessWriter:
		return RoleWriter, true
	}
	return "", false
}

func inMenu(role string) bool { return role == RoleReader || role == RoleWriter }

// Scope is the IAM territory of one namespace in one project: the members the
// broker may write are the principals of the ServiceAccounts of Namespace.
type Scope struct {
	ProjectNumber string
	ProjectID     string
	Namespace     string
}

// Validate refuses a scope that could produce a malformed or too wide member.
func (s Scope) Validate() error {
	if s.ProjectNumber == "" || strings.Trim(s.ProjectNumber, "0123456789") != "" {
		return fmt.Errorf("project number %q is not numeric", s.ProjectNumber)
	}
	if s.ProjectID == "" || strings.ContainsAny(s.ProjectID, "/: \t\n") {
		return fmt.Errorf("project id %q is not valid", s.ProjectID)
	}
	if len(validation.IsDNS1123Label(s.Namespace)) != 0 {
		return fmt.Errorf("namespace %q is not a DNS-1123 label", s.Namespace)
	}
	return nil
}

func (s Scope) memberPrefix() string {
	return "principal://iam.googleapis.com/projects/" + s.ProjectNumber +
		"/locations/global/workloadIdentityPools/" + s.ProjectID + ".svc.id.goog/subject/ns/" + s.Namespace + "/sa/"
}

// Member is the direct principal of the workload's ServiceAccount.
func (s Scope) Member(workload string) (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	if len(validation.IsDNS1123Label(workload)) != 0 {
		return "", fmt.Errorf("workload %q is not a DNS-1123 label", workload)
	}
	return s.memberPrefix() + workload, nil
}

// Manages reports whether (role, member) is a binding the broker owns in this
// scope: a menu role and a principal of one ServiceAccount of this namespace
// in this project. Everything else is left alone.
func (s Scope) Manages(role, member string) bool {
	if !inMenu(role) || s.Validate() != nil {
		return false
	}
	sa, ok := strings.CutPrefix(member, s.memberPrefix())
	return ok && len(validation.IsDNS1123Label(sa)) == 0
}

// Grant is the part of a ResourceGrant that decides a binding.
type Grant struct {
	Workload string
	Access   string
}

// Apply returns current with the broker-managed bindings of this scope made
// equal to the grants: managed members without a grant are removed, missing
// ones added, and every other member and binding is kept as it was (same
// order). It never modifies current and is idempotent. A binding emptied by the
// removal is dropped.
func (s Scope) Apply(current []Binding, grants []Grant) []Binding {
	type pair struct{ role, member string }
	want := map[pair]bool{}
	var order []pair
	for _, g := range grants {
		role, ok := RoleFor(g.Access)
		if !ok {
			continue
		}
		member, err := s.Member(g.Workload)
		if err != nil {
			continue
		}
		p := pair{role, member}
		if !want[p] {
			want[p] = true
			order = append(order, p)
		}
	}
	slices.SortFunc(order, func(a, b pair) int {
		if c := strings.Compare(a.role, b.role); c != 0 {
			return c
		}
		return strings.Compare(a.member, b.member)
	})

	seen := map[pair]bool{}
	out := make([]Binding, 0, len(current)+1)
	for _, b := range current {
		kept := make([]string, 0, len(b.Members))
		changed := false
		for _, m := range b.Members {
			p := pair{b.Role, m}
			switch {
			case !s.Manages(b.Role, m):
				kept = append(kept, m)
			case want[p] && !seen[p]:
				seen[p] = true
				kept = append(kept, m)
			default:
				changed = true
			}
		}
		if len(kept) == 0 && changed {
			continue
		}
		out = append(out, Binding{Role: b.Role, Members: kept})
	}
	for _, p := range order {
		if seen[p] {
			continue
		}
		if i := slices.IndexFunc(out, func(b Binding) bool { return b.Role == p.role }); i >= 0 {
			out[i].Members = append(out[i].Members, p.member)
		} else {
			out = append(out, Binding{Role: p.role, Members: []string{p.member}})
		}
	}
	return out
}

// SameBindings reports whether two binding lists are identical.
func SameBindings(a, b []Binding) bool {
	return slices.EqualFunc(a, b, func(x, y Binding) bool {
		return x.Role == y.Role && slices.Equal(x.Members, y.Members)
	})
}

// CheckMutation is the last guard before an IAM policy is written: whatever
// the caller computed, every (role, member) added or removed must be one the
// scope manages (menu role, principal of a ServiceAccount of this namespace in
// this project's pool). Any other difference between before and after is
// refused, so a bug upstream cannot widen or revoke anything else.
func CheckMutation(s Scope, before, after []Binding) error {
	pairs := func(bs []Binding) map[[2]string]bool {
		m := map[[2]string]bool{}
		for _, b := range bs {
			for _, member := range b.Members {
				m[[2]string{b.Role, member}] = true
			}
		}
		return m
	}
	was, now := pairs(before), pairs(after)
	check := func(from, other map[[2]string]bool, verb string) error {
		for p := range from {
			if !other[p] && !s.Manages(p[0], p[1]) {
				return fmt.Errorf("refusing to %s binding %s for %s: not a menu role for a principal of namespace %q", verb, p[0], p[1], s.Namespace)
			}
		}
		return nil
	}
	if err := check(now, was, "add"); err != nil {
		return err
	}
	return check(was, now, "remove")
}
