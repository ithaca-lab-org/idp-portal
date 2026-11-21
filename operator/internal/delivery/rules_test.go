package delivery

import (
	"strings"
	"testing"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
)

func spec(repo, path string) idpv1alpha1.DeliveryPipelineSpec {
	return idpv1alpha1.DeliveryPipelineSpec{Workload: "orders", Repo: repo, Path: path}
}

func TestValidateSpec(t *testing.T) {
	orgs := ParseOrgs(" ithaca-lab-org, Other-Org ,,")
	if len(orgs) != 2 || orgs[1] != "other-org" {
		t.Fatalf("orgs = %v", orgs)
	}
	good := "https://github.com/ithaca-lab-org/sample-orders-api"
	for _, tc := range []struct {
		name, repo, path, want string
		bad                    bool
	}{
		{"default path", good, "", "deploy/base", false},
		{"custom path", good, "k8s/prod", "k8s/prod", false},
		{"dot dir", good, "./x", "./x", false},
		{"other org case insensitive", "https://github.com/OTHER-org/x.y_z", "", "deploy/base", false},
		{"disallowed org", "https://github.com/evil/x", "", "", true},
		{"http", "http://github.com/ithaca-lab-org/x", "", "", true},
		{"other host", "https://gitlab.com/ithaca-lab-org/x", "", "", true},
		{"userinfo", "https://user@github.com/ithaca-lab-org/x", "", "", true},
		{"extra segment", good + "/tree/main", "", "", true},
		{"trailing slash", good + "/", "", "", true},
		{"dot git", good + ".git", "", "", true},
		{"query", good + "?x=1", "", "", true},
		{"no repo", "https://github.com/ithaca-lab-org", "", "", true},
		{"dotdot repo", "https://github.com/ithaca-lab-org/..", "", "", true},
		{"absolute path", good, "/etc", "", true},
		{"traversal", good, "a/../b", "", true},
		{"leading dotdot", good, "../x", "", true},
		{"double slash", good, "a//b", "", true},
		{"trailing slash path", good, "a/", "", true},
		{"space in path", good, "a b", "", true},
		{"long path", good, strings.Repeat("a", 129), "", true},
	} {
		got, err := ValidateSpec("demo", "orders", spec(tc.repo, tc.path), orgs)
		if tc.bad != (err != nil) || (!tc.bad && got != tc.want) {
			t.Errorf("%s: got %q, %v", tc.name, got, err)
		}
	}
	if _, err := ValidateSpec("demo", "other", spec(good, ""), orgs); err == nil {
		t.Error("name must equal workload")
	}
	if _, err := ValidateSpec("demo", "orders", spec(good, ""), nil); err == nil {
		t.Error("empty allowlist must refuse everything")
	}
}

func TestAppName(t *testing.T) {
	if got, err := AppName("demo", "orders"); err != nil || got != "demo-orders" {
		t.Errorf("got %q, %v", got, err)
	}
	for _, c := range [][2]string{{"1demo", "orders"}, {"demo", "Orders"}, {"", "x"}, {strings.Repeat("a", 31), strings.Repeat("b", 31)}} {
		if got, err := AppName(c[0], c[1]); err == nil {
			t.Errorf("AppName(%q, %q) = %q accepted", c[0], c[1], got)
		}
	}
	if _, err := AppName(strings.Repeat("a", 31), strings.Repeat("b", 30)); err != nil {
		t.Errorf("62 characters rejected: %v", err)
	}
}

func TestRefAndExecutionID(t *testing.T) {
	sha := strings.Repeat("ab12", 10)
	if !ValidRef(sha) {
		t.Error("good sha rejected")
	}
	for _, r := range []string{"", "main", sha[:39], sha + "0", strings.ToUpper(sha), strings.Repeat("g", 40), sha[:39] + "\n", "${T(java.lang.Runtime)}"} {
		if ValidRef(r) {
			t.Errorf("ref %q accepted", r)
		}
	}
	if !ValidExecutionID("01KABCDEF") || ValidExecutionID("a/b") || ValidExecutionID("") || ValidExecutionID(strings.Repeat("a", 65)) {
		t.Error("execution id rules")
	}
}
