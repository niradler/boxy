IMAGE_REPO?=boxydev
TAG?=dev
DEV_TAG?=e2e

.PHONY: test lint docker-build docker-build-dev test-dev-image kind-load kind-load-dev e2e e2e-go e2e-scripts fmt

test:
	go test ./...

e2e-go:
	@[ -n "$$BOXY_E2E_BASE_URL" ] && [ -n "$$BOXY_E2E_ROUTER_TOKEN" ] || (echo "set BOXY_E2E_BASE_URL and BOXY_E2E_ROUTER_TOKEN"; exit 1)
	go test -v -count=1 -tags=e2e ./test/e2e/... -timeout=15m

e2e-scripts:
	@[ -n "$$BASE_URL" ] && [ -n "$$ROUTER_TOKEN" ] || (echo "set BASE_URL, ROUTER_TOKEN, NAMESPACE"; exit 1)
	bash test/e2e/scripts/run-all.sh

lint:
	go vet ./...
	@command -v staticcheck >/dev/null && staticcheck ./... || true

fmt:
	go fmt ./...

docker-build:
	docker build -f Dockerfile.router -t $(IMAGE_REPO)/boxy-router:$(TAG) .
	docker build -f Dockerfile.controller -t $(IMAGE_REPO)/boxy-controller:$(TAG) .
	docker build -f Dockerfile.operator -t $(IMAGE_REPO)/boxy-operator:$(TAG) .

kind-load: docker-build
	kind load docker-image $(IMAGE_REPO)/boxy-router:$(TAG)
	kind load docker-image $(IMAGE_REPO)/boxy-controller:$(TAG)
	kind load docker-image $(IMAGE_REPO)/boxy-operator:$(TAG)

docker-build-dev: _docker-build-dev-image test-dev-image

_docker-build-dev-image:
	docker build -f Dockerfile.controller.dev -t $(IMAGE_REPO)/boxy-controller-dev:$(DEV_TAG) .

test-dev-image:
	bash test/image/test-dev-image.sh $(IMAGE_REPO)/boxy-controller-dev:$(DEV_TAG)

kind-load-dev: docker-build-dev
	kind load docker-image $(IMAGE_REPO)/boxy-controller-dev:$(DEV_TAG)

e2e:
	bash local/kind-e2e.sh
