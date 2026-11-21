package delivery

import (
	"context"
	"encoding/json"
	"sync"
)

// fakeGate is an in-memory Gate. Err, when set, fails every call.
type fakeGate struct {
	mu sync.Mutex

	Err error
	// Apps maps application name to description.
	Apps map[string]string
	// Pipelines maps application name to pipeline JSON.
	Pipelines map[string][]byte
	// CreateDelay makes CreateApplication a no-op for that many calls (the task is async).
	// A created application only shows up once ready.
	pending map[string]string
	// HidePipeline makes a saved pipeline invisible to PipelineID.
	HidePipeline bool

	ExecApp    string
	Statuses   map[string]string
	TriggerID  string
	TriggerErr error

	Calls    []string
	Triggers []map[string]string
}

func newFakeGate() *fakeGate {
	return &fakeGate{Apps: map[string]string{}, Pipelines: map[string][]byte{}, pending: map[string]string{},
		Statuses: map[string]string{}, TriggerID: "01EXECUTIONID", ExecApp: "demo-orders"}
}

func (g *fakeGate) call(s string) { g.Calls = append(g.Calls, s) }

func (g *fakeGate) Application(_ context.Context, app string) (bool, string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.call("app:" + app)
	if g.Err != nil {
		return false, "", g.Err
	}
	// A created application appears on the next look.
	if d, ok := g.pending[app]; ok {
		g.Apps[app] = d
		delete(g.pending, app)
	}
	d, ok := g.Apps[app]
	return ok, d, nil
}

func (g *fakeGate) CreateApplication(_ context.Context, app, description string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.call("create:" + app)
	if g.Err != nil {
		return g.Err
	}
	g.pending[app] = description
	return nil
}

func (g *fakeGate) PipelineID(_ context.Context, app, name string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.call("pipeline:" + app + "/" + name)
	if g.Err != nil {
		return "", g.Err
	}
	if _, ok := g.Pipelines[app]; ok && !g.HidePipeline {
		return "pid-" + app, nil
	}
	return "", nil
}

func (g *fakeGate) SavePipeline(_ context.Context, id string, body []byte) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.call("save:" + id)
	if g.Err != nil {
		return g.Err
	}
	var p struct {
		Application string `json:"application"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return err
	}
	g.Pipelines[p.Application] = body
	return nil
}

func (g *fakeGate) Trigger(_ context.Context, app, pipeline string, params map[string]string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.call("trigger:" + app + "/" + pipeline)
	if g.Err != nil {
		return "", g.Err
	}
	if g.TriggerErr != nil {
		return "", g.TriggerErr
	}
	g.Triggers = append(g.Triggers, params)
	return g.TriggerID, nil
}

func (g *fakeGate) SetApplicationDescription(_ context.Context, app, description string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.call("stamp:" + app)
	if g.Err != nil {
		return g.Err
	}
	g.pending[app] = description
	return nil
}

func (g *fakeGate) ExecutionStatus(_ context.Context, id string) (string, string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.call("status:" + id)
	if g.Err != nil {
		return "", "", g.Err
	}
	if s, ok := g.Statuses[id]; ok {
		return s, g.ExecApp, nil
	}
	return "RUNNING", g.ExecApp, nil
}
