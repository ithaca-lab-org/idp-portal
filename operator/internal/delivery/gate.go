package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Gate is the slice of Spinnaker's Gate API the controller uses. Tests fake it.
// No method returns a response body: failures are a *GateError (status code and
// a short fixed reason) so nothing Gate says can reach a status or a log.
type Gate interface {
	// Application returns whether the application exists and its description.
	Application(ctx context.Context, app string) (exists bool, description string, err error)
	// CreateApplication submits the create task; the application appears
	// shortly after (callers re-check with Application).
	CreateApplication(ctx context.Context, app, description string) error
	// PipelineID returns the id of the named pipeline, "" if there is none.
	PipelineID(ctx context.Context, app, name string) (string, error)
	// SavePipeline creates the pipeline (id "") or updates it.
	SavePipeline(ctx context.Context, id string, pipelineJSON []byte) error
	// Trigger starts the pipeline with the parameters and returns the execution id.
	Trigger(ctx context.Context, app, pipeline string, params map[string]string) (string, error)
	// ExecutionStatus returns Spinnaker's status of the execution (RUNNING, SUCCEEDED, TERMINAL, ...).
	ExecutionStatus(ctx context.Context, id string) (string, error)
}

// GateError is a sanitised failure.
type GateError struct {
	// Status is the HTTP status, 0 when Gate could not be reached.
	Status int
	Reason string
}

func (e *GateError) Error() string {
	if e.Status == 0 {
		return "gate: " + e.Reason
	}
	return fmt.Sprintf("gate: HTTP %d (%s)", e.Status, e.Reason)
}

// Permanent reports a client error that retrying will not fix.
func (e *GateError) Permanent() bool {
	return e.Status >= 400 && e.Status < 500 && e.Status != http.StatusRequestTimeout && e.Status != http.StatusTooManyRequests
}

// IsPermanent reports whether err is a permanent Gate refusal.
func IsPermanent(err error) bool {
	var ge *GateError
	return errors.As(err, &ge) && ge.Permanent()
}

// Safe returns a message fit for a status field.
func Safe(err error) string {
	var ge *GateError
	if errors.As(err, &ge) {
		return ge.Error()
	}
	return "gate: unexpected error"
}

const (
	gateTimeout  = 20 * time.Second
	maxBodyBytes = 4 << 20
	gateUser     = "idp-delivery"
	apiPath      = "/api/v1"
)

// HTTPGate talks to Gate. Gate is network protected and Fiat is off (ADR 0011),
// so no credentials are sent.
type HTTPGate struct {
	base string
	hc   *http.Client
}

// NewHTTPGate validates the URL. Gate serves under /api/v1; it is appended when
// the URL has no path of its own.
func NewHTTPGate(raw string) (*HTTPGate, error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("GATE_URL must be an http(s) URL without credentials, query or fragment")
	}
	if u.Path == "" {
		u.Path = apiPath
	}
	return &HTTPGate{
		base: u.String(),
		hc: &http.Client{
			Timeout: gateTimeout,
			// Gate never needs a redirect; following one could leave the cluster.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// do sends the request. On success it decodes at most maxBodyBytes of JSON into
// out (when not nil). On failure the body is discarded.
func (g *HTTPGate) do(ctx context.Context, method, path string, body any, out any) (int, error) {
	var rdr io.Reader
	if body != nil {
		var b []byte
		switch v := body.(type) {
		case []byte:
			b = v
		default:
			var err error
			if b, err = json.Marshal(v); err != nil {
				return 0, &GateError{Reason: "cannot encode request"}
			}
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, rdr)
	if err != nil {
		return 0, &GateError{Reason: "cannot build request"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := g.hc.Do(req)
	if err != nil {
		reason := "unreachable"
		if errors.Is(err, context.DeadlineExceeded) {
			reason = "timeout"
		}
		return 0, &GateError{Reason: reason}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
		return resp.StatusCode, &GateError{Status: resp.StatusCode, Reason: http.StatusText(resp.StatusCode)}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(out); err != nil {
		return resp.StatusCode, &GateError{Status: resp.StatusCode, Reason: "unreadable response"}
	}
	return resp.StatusCode, nil
}

func (g *HTTPGate) Application(ctx context.Context, app string) (bool, string, error) {
	var out struct {
		Attributes struct {
			Description string `json:"description"`
		} `json:"attributes"`
	}
	code, err := g.do(ctx, http.MethodGet, "/applications/"+url.PathEscape(app), nil, &out)
	if code == http.StatusNotFound {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, out.Attributes.Description, nil
}

func (g *HTTPGate) CreateApplication(ctx context.Context, app, description string) error {
	task := map[string]any{
		"application": app, "description": "create application",
		"job": []any{map[string]any{
			"type": "createApplication", "user": gateUser,
			"application": map[string]any{"name": app, "email": "platform@example.com", "cloudProviders": "kubernetes", "description": description},
		}},
	}
	_, err := g.do(ctx, http.MethodPost, "/tasks", task, nil)
	return err
}

func (g *HTTPGate) PipelineID(ctx context.Context, app, name string) (string, error) {
	var out []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if _, err := g.do(ctx, http.MethodGet, "/applications/"+url.PathEscape(app)+"/pipelineConfigs", nil, &out); err != nil {
		return "", err
	}
	for _, p := range out {
		if p.Name == name {
			return p.ID, nil
		}
	}
	return "", nil
}

func (g *HTTPGate) SavePipeline(ctx context.Context, id string, pipelineJSON []byte) error {
	if id == "" {
		_, err := g.do(ctx, http.MethodPost, "/pipelines", pipelineJSON, nil)
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(pipelineJSON, &m); err != nil {
		return &GateError{Reason: "invalid pipeline JSON"}
	}
	m["id"] = id
	_, err := g.do(ctx, http.MethodPut, "/pipelines/"+url.PathEscape(id), m, nil)
	return err
}

func (g *HTTPGate) Trigger(ctx context.Context, app, pipeline string, params map[string]string) (string, error) {
	var out struct {
		Ref string `json:"ref"`
	}
	body := map[string]any{"type": "manual", "user": gateUser, "parameters": params}
	if _, err := g.do(ctx, http.MethodPost, "/pipelines/"+url.PathEscape(app)+"/"+url.PathEscape(pipeline), body, &out); err != nil {
		return "", err
	}
	id := out.Ref[strings.LastIndex(out.Ref, "/")+1:]
	if !ValidExecutionID(id) {
		return "", &GateError{Status: http.StatusOK, Reason: "unexpected execution reference"}
	}
	return id, nil
}

func (g *HTTPGate) ExecutionStatus(ctx context.Context, id string) (string, error) {
	var out struct {
		Status string `json:"status"`
	}
	if _, err := g.do(ctx, http.MethodGet, "/pipelines/"+url.PathEscape(id), nil, &out); err != nil {
		return "", err
	}
	return out.Status, nil
}
