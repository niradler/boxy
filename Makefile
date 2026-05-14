IMAGE_REPO?=boxydev
TAG?=dev

.PHONY: test lint docker-build kind-load e2e fmt e2e-go

test:
	go test ./...

e2e-go:
	@[ -n "$$BOXY_E2E_BASE_URL" ] && [ -n "$$BOXY_E2E_ROUTER_TOKEN" ] || (echo "set BOXY_E2E_BASE_URL and BOXY_E2E_ROUTER_TOKEN"; exit 1)
	go test -v -count=1 -tags=e2e ./test/e2e/... -timeout=15m

lint:
	go vet ./...
	@command -v staticcheck >/dev/null && staticcheck ./... || true

fmt:
	go fmt ./...

docker-build:
	docker build -f Dockerfile.router -t $(IMAGE_REPO)/boxy-router:$(TAG) .
	docker build -f Dockerfile.controller -t $(IMAGE_REPO)/boxy-controller:$(TAG) .

kind-load: docker-build
	kind load docker-image $(IMAGE_REPO)/boxy-router:$(TAG)
	kind load docker-image $(IMAGE_REPO)/boxy-controller:$(TAG)

e2e:
	bash local/kind-e2e.sh
