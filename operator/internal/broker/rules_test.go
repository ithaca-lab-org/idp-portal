package broker

import (
	"crypto/sha256"
	"encoding/hex"
	"math/rand"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

var testScope = Scope{ProjectNumber: "123456789", ProjectID: "proj-1", Namespace: "team-a"}

func memberOf(ns, sa string) string {
	return "principal://iam.googleapis.com/projects/123456789/locations/global/workloadIdentityPools/proj-1.svc.id.goog/subject/ns/" + ns + "/sa/" + sa
}

func TestBucketName(t *testing.T) {
	const pn = "123456789"
	long, err := BucketName(pn, strings.Repeat("n", 31), strings.Repeat("c", 18))
	if err != nil || len(long) != MaxBucketLen {
		t.Errorf("longest name = %q (%d), %v", long, len(long), err)
	}
	got, err := BucketName(pn, "team-a", "data")
	if err != nil || !regexp.MustCompile(`^idp-team-a-data-[0-9a-f]{8}$`).MatchString(got) {
		t.Errorf("BucketName = %q, %v", got, err)
	}
	if again, _ := BucketName(pn, "team-a", "data"); again != got {
		t.Error("not deterministic")
	}
	sum := sha256.Sum256([]byte(pn + "/team-a/data"))
	if !strings.HasSuffix(got, hex.EncodeToString(sum[:])[:8]) {
		t.Errorf("suffix is not sha256(projectNumber/ns/claim): %q", got)
	}
	// The hash depends on the project.
	if other, _ := BucketName("987654321", "team-a", "data"); other == got {
		t.Error("project number not part of the name")
	}
	for _, in := range [][2]string{
		{strings.Repeat("n", 32), "c"}, {strings.Repeat("n", 40), "c"}, {"n", strings.Repeat("c", 19)}, {"", "c"}, {"n", ""},
		{"Team", "c"}, {"n", "C"}, {"a.b", "c"}, {"n", "a/b"}, {"-n", "c"}, {"n", "c-"}, {"n_", "c"},
		{"google-team", "c"}, {"n", "my-google"}, {"n", "googx"}, {"goog", "c"}, {"goog-team", "c"},
	} {
		if got, err := BucketName(pn, in[0], in[1]); err == nil {
			t.Errorf("BucketName(%q,%q) = %q, want an error", in[0], in[1], got)
		}
	}
	if _, err := BucketName(pn, strings.Repeat("n", 32), "c"); err == nil || !strings.Contains(err.Error(), "31") {
		t.Errorf("the error for a long namespace should name the limit: %v", err)
	}
}

// (a-b, c) and (a, b-c) used to share idp-a-b-c; the hash separates them.
func TestNamesDoNotCollide(t *testing.T) {
	seen := map[string][2]string{}
	nss := []string{"a", "a-b", "a-b-c", "ab", "b", "b-c"}
	cls := []string{"c", "b-c", "b", "c-d", "d"}
	for _, ns := range nss {
		for _, c := range cls {
			name, err := BucketName("123456789", ns, c)
			if err != nil {
				t.Fatal(err)
			}
			if prev, dup := seen[name]; dup {
				t.Errorf("%v and %v both give %q", prev, [2]string{ns, c}, name)
			}
			seen[name] = [2]string{ns, c}
		}
	}
	a, _ := BucketName("123456789", "a-b", "c")
	b, _ := BucketName("123456789", "a", "b-c")
	if a == b {
		t.Error("a-b/c and a/b-c collide")
	}
}

func TestClassify(t *testing.T) {
	const pn = "123456789"
	const uid = "0f0f0f0f-1111-2222-3333-444444444444"
	mk := func(labels map[string]string) *BucketInfo {
		return &BucketInfo{Exists: true, Labels: labels, ProjectNumber: pn}
	}
	own := OwnerLabels("team-a", "data", uid)
	if got := Classify(mk(own), pn, "team-a", "data", uid); got != Owned {
		t.Errorf("own labels = %v", got)
	}
	// Every owner label must match, and the bucket must be in our project.
	foreign := map[string]*BucketInfo{
		"nil":               mk(nil),
		"empty":             mk(map[string]string{}),
		"not managed":       mk(map[string]string{LabelNamespace: "team-a", LabelClaim: "data", LabelClaimUID: uid}),
		"managed false":     mk(map[string]string{LabelManaged: "false", LabelNamespace: "team-a", LabelClaim: "data", LabelClaimUID: uid}),
		"managed uppercase": mk(map[string]string{LabelManaged: "True", LabelNamespace: "team-a", LabelClaim: "data", LabelClaimUID: uid}),
		"other namespace":   mk(map[string]string{LabelManaged: "true", LabelNamespace: "team-b", LabelClaim: "data", LabelClaimUID: uid}),
		"other claim":       mk(map[string]string{LabelManaged: "true", LabelNamespace: "team-a", LabelClaim: "logs", LabelClaimUID: uid}),
		"no uid":            mk(map[string]string{LabelManaged: "true", LabelNamespace: "team-a", LabelClaim: "data"}),
		"other uid":         mk(OwnerLabels("team-a", "data", "another-uid")),
		"other project":     {Exists: true, Labels: own, ProjectNumber: "999"},
		"no project":        {Exists: true, Labels: own},
	}
	for name, info := range foreign {
		if got := Classify(info, pn, "team-a", "data", uid); got != Foreign {
			t.Errorf("%s: %v, want Foreign", name, got)
		}
	}
	if Classify(mk(own), pn, "team-a", "data", "") != Foreign || Classify(mk(own), "", "team-a", "data", uid) != Foreign {
		t.Error("an empty UID or project number matched")
	}
	rel := OwnerLabels("team-a", "data", uid)
	for k, v := range ReleasingLabels() {
		rel[k] = v
	}
	if got := Classify(mk(rel), pn, "team-a", "data", uid); got != Releasing {
		t.Errorf("releasing = %v", got)
	}
	orphan := OwnerLabels("team-a", "data", uid)
	for k, v := range OrphanLabels(time.Unix(1700000000, 0)) {
		orphan[k] = v
	}
	if orphan[LabelOrphanedAt] != "1700000000" {
		t.Errorf("orphaned-at = %q", orphan[LabelOrphanedAt])
	}
	if got := Classify(mk(orphan), pn, "team-a", "data", uid); got != Orphaned {
		t.Errorf("orphan labels = %v", got)
	}
	orphan[LabelReleasing] = "true"
	if got := Classify(mk(orphan), pn, "team-a", "data", uid); got != Orphaned {
		t.Errorf("orphaned and releasing = %v", got)
	}
}

func TestRoleMenu(t *testing.T) {
	for access, want := range map[string]string{"reader": "roles/storage.objectViewer", "writer": "roles/storage.objectUser"} {
		if got, ok := RoleFor(access); !ok || got != want {
			t.Errorf("RoleFor(%q) = %q, %v", access, got, ok)
		}
	}
	for _, a := range []string{"", "admin", "Reader", "owner", "roles/storage.admin", "reader "} {
		if got, ok := RoleFor(a); ok {
			t.Errorf("RoleFor(%q) = %q", a, got)
		}
	}
}

func TestMemberAndManages(t *testing.T) {
	m, err := testScope.Member("web")
	if err != nil || m != memberOf("team-a", "web") {
		t.Fatalf("Member = %q, %v", m, err)
	}
	for _, w := range []string{"", "Web", "a/b", "a..b", "web/../x", "a b"} {
		if _, err := testScope.Member(w); err == nil {
			t.Errorf("Member(%q) accepted", w)
		}
	}
	yes := [][2]string{{RoleReader, memberOf("team-a", "web")}, {RoleWriter, memberOf("team-a", "x-1")}}
	for _, p := range yes {
		if !testScope.Manages(p[0], p[1]) {
			t.Errorf("Manages(%q, %q) = false", p[0], p[1])
		}
	}
	no := [][2]string{
		{"roles/storage.admin", memberOf("team-a", "web")},
		{"roles/storage.objectAdmin", memberOf("team-a", "web")},
		{"roles/owner", memberOf("team-a", "web")},
		{RoleReader, memberOf("team-b", "web")},
		{RoleReader, memberOf("team-a", "web/extra")},
		{RoleReader, memberOf("team-a", "")},
		{RoleReader, memberOf("team-a", "Web")},
		{RoleReader, "serviceAccount:x@proj-1.iam.gserviceaccount.com"},
		{RoleReader, "allUsers"},
		{RoleReader, "allAuthenticatedUsers"},
		{RoleReader, "user:a@example.com"},
		{RoleReader, "principalSet://iam.googleapis.com/projects/123456789/locations/global/workloadIdentityPools/proj-1.svc.id.goog/namespace/team-a"},
		{RoleReader, strings.Replace(memberOf("team-a", "web"), "123456789", "999", 1)},
		{RoleReader, strings.Replace(memberOf("team-a", "web"), "proj-1", "proj-2", 1)},
		{RoleReader, memberOf("team-a", "web") + "\n"},
	}
	for _, p := range no {
		if testScope.Manages(p[0], p[1]) {
			t.Errorf("Manages(%q, %q) = true", p[0], p[1])
		}
	}
	for _, s := range []Scope{
		{ProjectNumber: "", ProjectID: "p", Namespace: "n"},
		{ProjectNumber: "12a", ProjectID: "p", Namespace: "n"},
		{ProjectNumber: "1", ProjectID: "", Namespace: "n"},
		{ProjectNumber: "1", ProjectID: "p/q", Namespace: "n"},
		{ProjectNumber: "1", ProjectID: "p", Namespace: "N"},
	} {
		if s.Validate() == nil || s.Manages(RoleReader, "anything") {
			t.Errorf("scope %+v accepted", s)
		}
		if _, err := s.Member("w"); err == nil {
			t.Errorf("scope %+v built a member", s)
		}
	}
}

func TestApplyAddsAndRemoves(t *testing.T) {
	foreign := []Binding{
		{Role: "roles/storage.admin", Members: []string{"user:boss@example.com"}},
		{Role: RoleReader, Members: []string{"serviceAccount:ci@proj-1.iam.gserviceaccount.com", memberOf("team-b", "web"), memberOf("team-a", "old")}},
	}
	got := testScope.Apply(foreign, []Grant{{"web", "reader"}, {"api", "writer"}})
	want := []Binding{
		{Role: "roles/storage.admin", Members: []string{"user:boss@example.com"}},
		{Role: RoleReader, Members: []string{"serviceAccount:ci@proj-1.iam.gserviceaccount.com", memberOf("team-b", "web"), memberOf("team-a", "web")}},
		{Role: RoleWriter, Members: []string{memberOf("team-a", "api")}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
	if len(foreign[1].Members) != 3 {
		t.Error("Apply modified its input")
	}
	// No grants: everything the broker manages goes, the rest stays.
	got = testScope.Apply(want, nil)
	want = []Binding{
		{Role: "roles/storage.admin", Members: []string{"user:boss@example.com"}},
		{Role: RoleReader, Members: []string{"serviceAccount:ci@proj-1.iam.gserviceaccount.com", memberOf("team-b", "web")}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func TestApplyIgnoresBadGrants(t *testing.T) {
	got := testScope.Apply(nil, []Grant{{"web", "admin"}, {"Bad/Name", "reader"}, {"", "writer"}})
	if len(got) != 0 {
		t.Errorf("bad grants produced %+v", got)
	}
}

func TestApplyRepairsDriftAndDuplicates(t *testing.T) {
	// A hand-added managed member for a workload without grant, and a duplicate.
	cur := []Binding{{Role: RoleWriter, Members: []string{memberOf("team-a", "web"), memberOf("team-a", "web"), memberOf("team-a", "stray")}}}
	got := testScope.Apply(cur, []Grant{{"web", "writer"}})
	want := []Binding{{Role: RoleWriter, Members: []string{memberOf("team-a", "web")}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
}

// Property test: random policies and grants.
func TestApplyProperties(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	roles := []string{RoleReader, RoleWriter, "roles/storage.admin", "roles/storage.objectAdmin", "roles/owner", "roles/storage.legacyBucketReader"}
	members := []string{
		memberOf("team-a", "web"), memberOf("team-a", "api"), memberOf("team-a", "job"), memberOf("team-b", "web"),
		"user:a@example.com", "allUsers", "serviceAccount:s@proj-1.iam.gserviceaccount.com",
		"principalSet://iam.googleapis.com/projects/123456789/locations/global/workloadIdentityPools/proj-1.svc.id.goog/namespace/team-a",
	}
	workloads := []string{"web", "api", "job", "other", "Bad/Name"}
	accesses := []string{"reader", "writer", "admin", ""}
	for i := 0; i < 500; i++ {
		var cur []Binding
		for _, r := range roles {
			if rng.Intn(2) == 0 {
				continue
			}
			b := Binding{Role: r}
			for _, m := range members {
				if rng.Intn(2) == 0 {
					b.Members = append(b.Members, m)
				}
			}
			if len(b.Members) > 0 {
				cur = append(cur, b)
			}
		}
		var grants []Grant
		for n := rng.Intn(5); n > 0; n-- {
			grants = append(grants, Grant{workloads[rng.Intn(len(workloads))], accesses[rng.Intn(len(accesses))]})
		}
		snapshot := cloneBindings(cur)
		got := testScope.Apply(cur, grants)

		if !SameBindings(cur, snapshot) {
			t.Fatal("input modified")
		}
		// Only menu roles, only this namespace's principals differ from the input.
		if err := CheckMutation(testScope, cur, got); err != nil {
			t.Fatalf("CheckMutation: %v\ncur %+v\ngrants %+v\ngot %+v", err, cur, grants, got)
		}
		in, out := pairSet(cur), pairSet(got)
		for p := range in {
			if !out[p] && !testScope.Manages(p[0], p[1]) {
				t.Fatalf("removed unmanaged %v", p)
			}
		}
		for p := range out {
			if !in[p] && !testScope.Manages(p[0], p[1]) {
				t.Fatalf("added unmanaged %v", p)
			}
		}
		// The managed part equals exactly the grants.
		want := map[[2]string]bool{}
		for _, g := range grants {
			role, ok := RoleFor(g.Access)
			m, err := testScope.Member(g.Workload)
			if ok && err == nil {
				want[[2]string{role, m}] = true
			}
		}
		for p := range out {
			if testScope.Manages(p[0], p[1]) != want[p] && testScope.Manages(p[0], p[1]) {
				t.Fatalf("unexpected managed %v", p)
			}
		}
		for p := range want {
			if !out[p] {
				t.Fatalf("missing %v", p)
			}
		}
		// Idempotent, and no managed member twice.
		if again := testScope.Apply(got, grants); !SameBindings(got, again) {
			t.Fatalf("not idempotent: %+v -> %+v", got, again)
		}
		for _, b := range got {
			if len(b.Members) == 0 {
				t.Fatalf("empty binding %+v", b)
			}
			seenM := map[string]bool{}
			for _, m := range b.Members {
				if testScope.Manages(b.Role, m) && seenM[m] {
					t.Fatalf("duplicate %q", m)
				}
				seenM[m] = true
			}
		}
	}
}

func pairSet(bs []Binding) map[[2]string]bool {
	m := map[[2]string]bool{}
	for _, b := range bs {
		for _, x := range b.Members {
			m[[2]string{b.Role, x}] = true
		}
	}
	return m
}

func TestCheckMutation(t *testing.T) {
	base := []Binding{{Role: "roles/storage.admin", Members: []string{"user:a@example.com"}}}
	good := append(cloneBindings(base), Binding{Role: RoleReader, Members: []string{memberOf("team-a", "web")}})
	if err := CheckMutation(testScope, base, good); err != nil {
		t.Errorf("menu add refused: %v", err)
	}
	if err := CheckMutation(testScope, good, base); err != nil {
		t.Errorf("menu removal refused: %v", err)
	}
	otherProject := strings.Replace(memberOf("team-a", "web"), "proj-1.svc.id.goog", "proj-2.svc.id.goog", 1)
	otherNumber := strings.Replace(memberOf("team-a", "web"), "123456789", "999", 1)
	add := func(role string, members ...string) []Binding {
		return append(cloneBindings(base), Binding{Role: role, Members: members})
	}
	for name, after := range map[string][]Binding{
		"admin to a principal":            {{Role: "roles/storage.admin", Members: []string{"user:a@example.com", memberOf("team-a", "web")}}},
		"menu role to allUsers":           add(RoleReader, "allUsers"),
		"menu role to a user":             add(RoleReader, "user:x@example.com"),
		"another namespace's principal":   add(RoleReader, memberOf("team-b", "web")),
		"another project's pool":          add(RoleWriter, otherProject),
		"another project number":          add(RoleWriter, otherNumber),
		"a principalSet of the namespace": add(RoleReader, "principalSet://iam.googleapis.com/projects/123456789/locations/global/workloadIdentityPools/proj-1.svc.id.goog/namespace/team-a"),
		"a non-menu role for our member":  add("roles/storage.objectAdmin", memberOf("team-a", "web")),
		"foreign binding removed":         nil,
	} {
		if err := CheckMutation(testScope, base, after); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// Removing another namespace's principal is refused as well.
	theirs := add(RoleReader, memberOf("team-b", "web"))
	if err := CheckMutation(testScope, theirs, base); err == nil {
		t.Error("removal of another namespace's principal accepted")
	}
	if err := CheckMutation(Scope{}, base, good); err == nil {
		t.Error("an invalid scope accepted a change")
	}
}
