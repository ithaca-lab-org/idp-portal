package delivery

import (
	"bytes"
	"encoding/json"
	"fmt"
	"text/template"
)

// pipelineTmpl is the one pipeline every tenant application gets (the shape of
// idp-infra/scripts/spinnaker-demo-pipeline.sh, which ran on GKE): bake the
// Kustomize base from the git repo, then deploy it into the namespace through
// the Clouddriver account named like the namespace. Namespace, workload,
// repository and path are baked in by the controller; the only run-time input is
// the commit, passed as the pipeline parameter "ref" (40 hex, checked before
// the trigger).
//
// The pipeline adds nothing to the manifests: serviceAccountName=<workload> and
// the pod label idp.ithaca-idp.com/workload=<workload> come from the app repo's
// Kustomize base, and the admission policy idp-spinnaker-scope refuses a
// Deployment from Clouddriver that lacks them or lets them differ. The image is
// named by digest in that base. The Deploy stage skips SpEL evaluation, so
// expressions inside the tenant's baked manifests are never evaluated by Orca
// (they could call #fromUrl and make Orca send requests inside the cluster). The
// Bake stage cannot skip it: it needs ${parameters.ref}; it only sees values this
// template and the validated ref put there. All values go through the json function, so none
// can break out of its string.
const pipelineTmpl = `{
  "application": {{json .App}},
  "name": {{json .Name}},
  "description": {{json .Description}},
  "keepWaitingPipelines": false,
  "limitConcurrent": true,
  "expectedArtifacts": [],
  "triggers": [],
  "parameterConfig": [
    {"name": "ref", "label": "Git commit SHA", "required": true, "hasOptions": false, "options": []}
  ],
  "stages": [
    {
      "refId": "1",
      "requisiteStageRefIds": [],
      "type": "bakeManifest",
      "name": "Bake kustomize",
      "templateRenderer": "KUSTOMIZE",
      "kustomizeFilePath": {{json .KustomizationFile}},
      "namespace": {{json .Namespace}},
      "outputName": "app",
      "expectedArtifacts": [
        {
          "id": "baked-app",
          "displayName": "baked-app",
          "usePriorArtifact": false,
          "useDefaultArtifact": false,
          "matchArtifact": {"type": "embedded/base64", "name": "app", "artifactAccount": "embedded-artifact"}
        }
      ],
      "inputArtifact": {
        "account": "public-git",
        "artifact": {"type": "git/repo", "artifactAccount": "public-git", "reference": {{json .Repo}}, "version": "${parameters.ref}"}
      }
    },
    {
      "refId": "2",
      "requisiteStageRefIds": ["1"],
      "type": "deployManifest",
      "name": "Deploy",
      "account": {{json .Namespace}},
      "cloudProvider": "kubernetes",
      "moniker": {"app": {{json .App}}},
      "source": "artifact",
      "manifestArtifactAccount": "embedded-artifact",
      "manifestArtifactId": "baked-app",
      "namespaceOverride": {{json .Namespace}},
      "skipExpressionEvaluation": true
    }
  ]
}
`

var tmpl = template.Must(template.New("pipeline").Funcs(template.FuncMap{
	"json": func(v string) (string, error) {
		b, err := json.Marshal(v)
		return string(b), err
	},
}).Parse(pipelineTmpl))

type pipelineData struct {
	App, Name, Description, Namespace, Repo, KustomizationFile string
}

// RenderPipeline returns the pipeline JSON for a validated namespace, workload,
// repository and Kustomize path (see ValidateSpec).
func RenderPipeline(namespace, workload, repo, path string) ([]byte, error) {
	app, err := AppName(namespace, workload)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	err = tmpl.Execute(&buf, pipelineData{
		App: app, Name: PipelineName, Description: AppDescription(namespace, workload),
		Namespace: namespace, Repo: repo, KustomizationFile: path + "/kustomization.yaml",
	})
	if err != nil {
		return nil, err
	}
	if !json.Valid(buf.Bytes()) {
		return nil, fmt.Errorf("rendered pipeline is not valid JSON")
	}
	return buf.Bytes(), nil
}
