package delivery

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

func TestRenderPipelineGolden(t *testing.T) {
	got, err := RenderPipeline("demo", "orders", "https://github.com/ithaca-lab-org/sample-orders-api", "deploy/base")
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "pipeline.golden.json")
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("pipeline differs from %s (run with -update after reviewing):\n%s", golden, got)
	}
}

func TestRenderPipelineShape(t *testing.T) {
	b, err := RenderPipeline("demo", "orders", "https://github.com/ithaca-lab-org/sample-orders-api", "k8s/x")
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Application string `json:"application"`
		Name        string `json:"name"`
		Stages      []struct {
			Type                     string `json:"type"`
			Account                  string `json:"account"`
			Namespace                string `json:"namespace"`
			NamespaceOverride        string `json:"namespaceOverride"`
			KustomizeFilePath        string `json:"kustomizeFilePath"`
			SkipExpressionEvaluation bool   `json:"skipExpressionEvaluation"`
			Moniker                  struct{ App string }
			InputArtifact            struct {
				Artifact struct{ Reference, Version string }
			} `json:"inputArtifact"`
		} `json:"stages"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	if p.Application != "demo-orders" || p.Name != "deploy" || len(p.Stages) != 2 {
		t.Fatalf("pipeline = %+v", p)
	}
	bake, deploy := p.Stages[0], p.Stages[1]
	if bake.Type != "bakeManifest" || bake.Namespace != "demo" || bake.KustomizeFilePath != "k8s/x/kustomization.yaml" ||
		bake.InputArtifact.Artifact.Version != "${parameters.ref}" {
		t.Errorf("bake = %+v", bake)
	}
	if !deploy.SkipExpressionEvaluation {
		t.Error("the Deploy stage must skip SpEL evaluation (tenant manifests are not trusted)")
	}
	if deploy.Type != "deployManifest" || deploy.Account != "demo" || deploy.NamespaceOverride != "demo" || deploy.Moniker.App != "demo-orders" {
		t.Errorf("deploy = %+v", deploy)
	}
}

// Values go through the json function: a hostile value stays inside its string.
func TestRenderPipelineEscapes(t *testing.T) {
	b, err := RenderPipeline("demo", "orders", `https://github.com/o/r","evil":"1`, "a\"b")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["evil"]; ok {
		t.Error("value escaped its string")
	}
	if _, err := RenderPipeline("1bad", "orders", "r", "p"); err == nil {
		t.Error("invalid app name accepted")
	}
}
