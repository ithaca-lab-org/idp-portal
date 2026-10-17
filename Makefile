.PHONY: check
check: ## vet and test the API
	cd api && go vet ./... && go test ./...

.PHONY: run
run: ## run the API locally (see README for the env vars)
	cd api && go run .
