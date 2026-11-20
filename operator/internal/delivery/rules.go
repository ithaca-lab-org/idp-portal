// Package delivery is the delivery controller of ADR 0011 (Order #04
// milestone 2): it turns DeliveryPipelines and DeliveryRuns into Spinnaker
// applications, pipelines and executions through Gate. It is the only Gate
// caller and shares nothing with the operator manager or the broker but the
// API types.
package delivery

import (
	"fmt"
	"regexp"
	"strings"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
)

const (
	// DefaultPath is the Kustomize directory when spec.path is empty.
	DefaultPath = "deploy/base"
	// PipelineName is the one pipeline every application has.
	PipelineName = "deploy"
	// DefaultAllowedOrgs is the repo owner allowlist when none is configured.
	DefaultAllowedOrgs = "ithaca-lab-org"
	// maxAppName: Spinnaker wants fewer than 63 characters.
	maxAppName = 62
)

var (
	labelRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	// The same shape as the CRD pattern (api/v1alpha1/deliverypipeline_types.go).
	repoRe = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9][A-Za-z0-9-]{0,38})/([A-Za-z0-9._-]{1,100})$`)
	pathRe = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._/-]*$`)
	shaRe  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	execRe = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)
)

// ParseOrgs splits a comma separated allowlist; blanks are dropped and names
// are lowercased (GitHub owners are case insensitive).
func ParseOrgs(s string) []string {
	var out []string
	for _, o := range strings.Split(s, ",") {
		if o = strings.ToLower(strings.TrimSpace(o)); o != "" {
			out = append(out, o)
		}
	}
	return out
}

// AppName is the Spinnaker application of a workload: <namespace>-<workload>.
// Spinnaker's Kubernetes provider needs ^[a-zA-Z][a-zA-Z0-9-]*$ and fewer than
// 63 characters.
func AppName(namespace, workload string) (string, error) {
	if !labelRe.MatchString(namespace) || !labelRe.MatchString(workload) {
		return "", fmt.Errorf("namespace and workload must be DNS labels")
	}
	if namespace[0] < 'a' || namespace[0] > 'z' {
		return "", fmt.Errorf("namespace %q must start with a letter (Spinnaker application names do)", namespace)
	}
	app := namespace + "-" + workload
	if len(app) > maxAppName {
		return "", fmt.Errorf("application name %s is too long (%d characters at most)", app, maxAppName)
	}
	return app, nil
}

// AppDescription marks an application as owned by one namespace and workload.
// The join <ns>-<workload> is ambiguous (a-b + c versus a + b-c), so an
// existing application whose marker names another pair is never adopted.
func AppDescription(namespace, workload string) string {
	return "idp-managed namespace=" + namespace + " workload=" + workload
}

// ValidRef reports whether ref is a full lowercase 40-hex commit SHA.
func ValidRef(ref string) bool { return shaRe.MatchString(ref) }

// ValidExecutionID reports whether id looks like a Spinnaker execution ULID.
func ValidExecutionID(id string) bool { return execRe.MatchString(id) }

// ValidateSpec applies the CRD rules again (so they hold if the CRD is ever
// loosened) plus the owner allowlist, which only the controller knows. It
// returns the effective Kustomize path.
func ValidateSpec(namespace, name string, spec idpv1alpha1.DeliveryPipelineSpec, allowedOrgs []string) (string, error) {
	if !labelRe.MatchString(spec.Workload) || len(spec.Workload) > 40 {
		return "", fmt.Errorf("spec.workload %q is not a workload name", spec.Workload)
	}
	if name != spec.Workload {
		return "", fmt.Errorf("a DeliveryPipeline must be named after its workload")
	}
	if _, err := AppName(namespace, spec.Workload); err != nil {
		return "", err
	}
	m := repoRe.FindStringSubmatch(spec.Repo)
	if m == nil || m[2] == "." || m[2] == ".." || strings.HasSuffix(m[2], ".git") {
		return "", fmt.Errorf("spec.repo must look like https://github.com/<org>/<repo>")
	}
	allowed := false
	for _, o := range allowedOrgs {
		if strings.EqualFold(o, m[1]) {
			allowed = true
		}
	}
	if !allowed {
		return "", fmt.Errorf("repository owner %q is not allowed", m[1])
	}
	path := spec.Path
	if path == "" {
		path = DefaultPath
	}
	if len(path) > 128 || !pathRe.MatchString(path) || strings.Contains(path, "..") ||
		strings.Contains(path, "//") || strings.HasSuffix(path, "/") {
		return "", fmt.Errorf("spec.path must be a relative directory without '..'")
	}
	return path, nil
}
