.PHONY: check
check: ## vet and test the API
	cd api && go vet ./... && go test ./...

.PHONY: run
run: ## run the API locally (see README for the env vars)
	cd api && go run .

.PHONY: web
web: ## build the web UI into the API's embed directory (restore the placeholder with: git checkout api/internal/webui/dist)
	cd web && npm ci --ignore-scripts && npm run build
	rm -rf api/internal/webui/dist && cp -R web/dist api/internal/webui/dist
	@echo "built; do not commit api/internal/webui/dist/index.html (restore: git checkout api/internal/webui/dist)"

.PHONY: web-check
web-check: ## typecheck and test the web UI
	cd web && npm run typecheck && npm test
