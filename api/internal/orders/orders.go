// Package orders holds the order model, validation, storage and the service that turns an
// order into a TeamNamespace custom resource.
package orders

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type Status string

const (
	StatusPending  Status = "PENDING"
	StatusComplete Status = "COMPLETE"
	StatusFailed   Status = "FAILED"
	// StatusReleased marks a COMPLETE order whose namespace was later removed: it frees the name
	// and no longer counts against the caps.
	StatusReleased Status = "RELEASED"

	KindNamespace = "namespace"
)

type Order struct {
	ID           string    `bson:"_id" json:"id"`
	Kind         string    `bson:"kind" json:"kind"`
	Name         string    `bson:"name" json:"name"`
	BusinessUnit string    `bson:"businessUnit" json:"businessUnit"`
	Owner        string    `bson:"owner" json:"owner"`
	OwnerSub     string    `bson:"ownerSub" json:"-"` // the Keycloak subject: authorization and caps use this, not the email
	Size         string    `bson:"size" json:"size"`
	Status       Status    `bson:"status" json:"status"`
	Message      string    `bson:"message,omitempty" json:"message,omitempty"`
	CreatedAt    time.Time `bson:"createdAt" json:"createdAt"`
	UpdatedAt    time.Time `bson:"updatedAt" json:"updatedAt"`
}

var (
	ErrNotFound  = errors.New("order not found")
	ErrNameTaken = errors.New("an active order or namespace with this name already exists")
	ErrForbidden = errors.New("forbidden")
	ErrInvalid   = errors.New("invalid request")
	ErrCap       = errors.New("order limit reached")
	ErrCluster   = errors.New("cluster request failed")
)

// invalid wraps ErrInvalid with a message that is safe to show to the caller.
func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// The shapes below mirror the TeamNamespace CRD (operator/api/v1alpha1) so a request the
// operator would reject is refused before an order is stored.
var (
	dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	emailRe  = regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9-]+([.][a-zA-Z0-9-]+)+$`)
	// ReservedNames and ReservedPrefixes must match operator/internal/controller/resources.go.
	ReservedNames    = []string{"default", "argocd", "cert-manager", "tailscale", "identity", "idp-system", "echo", "vault"}
	ReservedPrefixes = []string{"kube-", "istio-", "gke-", "vault-"}
)

const maxNameLen = 40

func ValidateName(name string) error {
	if name == "" || len(name) > maxNameLen || !dnsLabel.MatchString(name) {
		return invalid("name must be a lowercase DNS label of at most %d characters", maxNameLen)
	}
	for _, r := range ReservedNames {
		if name == r {
			return invalid("name %q is reserved", name)
		}
	}
	for _, p := range ReservedPrefixes {
		if strings.HasPrefix(name, p) {
			return invalid("names starting with %q are reserved", p)
		}
	}
	return nil
}

func ValidateBusinessUnit(bu string) error {
	if len(bu) > 63 || !dnsLabel.MatchString(bu) {
		return invalid("business unit is not a valid DNS label")
	}
	return nil
}

func ValidateSize(size string) error {
	if size != "small" && size != "medium" {
		return invalid("size must be small or medium")
	}
	return nil
}

func ValidateEmail(email string) bool { return len(email) <= 254 && emailRe.MatchString(email) }
