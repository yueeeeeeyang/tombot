.PHONY: fmt lint check test test-docker-engine test-host-tools test-docker-node test-image-preparation test-process-groups test-shared-browser computer-image test-computer-image build release verify-release database-migrate

GO ?= go

fmt:
	gofmt -w cmd internal web deploy

lint:
	test -z "$$(gofmt -l cmd internal web deploy)"
	$(GO) vet ./...

check:
	$(GO) build ./...

test:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts -p 'test_*.py'
	$(GO) test -race ./...

test-docker-engine:
	$(GO) test -race -tags=dockerintegration -run TestLiveDockerEngine -count=1 -v ./internal/dockerbootstrap

test-host-tools:
	test "$$($(GO) env GOOS)" = linux
	$(GO) test -race -tags=hostintegration -run '^TestLiveHostPackages$$' -count=1 -v ./internal/dockerbootstrap

test-docker-node:
	$(GO) test -race -tags=dockerintegration -run TestLiveDockerLifecycle -count=1 -v ./internal/dockernode

test-image-preparation:
	$(GO) test -race -tags=dockerintegration -run '^TestLiveImagePreparation$$' -count=1 -v ./internal/dockernode

test-process-groups:
	$(GO) test -race -tags=cgroupintegration -run '^TestKernelServiceGroups$$' -count=1 -v ./internal/processgroup

test-shared-browser:
	../admin/node_modules/.bin/playwright test --config verification/playwright.config.mjs

computer-image:
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/build_computer_image.py $(IMAGE_ARGS)

test-computer-image:
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/verify_computer_image.py $(IMAGE_TEST_ARGS)

build:
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/tomd ./cmd/tomd
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/tomdb ./cmd/tomdb
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/tomdocker-bootstrap ./cmd/tomdocker-bootstrap
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/tomdeploy-check ./cmd/tomdeploy-check
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/tomdeploy-config ./cmd/tomdeploy-config
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/tomnode-register ./cmd/tomnode-register
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/tomimage-prepare ./cmd/tomimage-prepare
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/tomnode ./cmd/tomnode
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/computer-task ./cmd/computer-task
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/computer-probe ./cmd/computer-probe
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/computer-agent ./cmd/computer-agent
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/bot-agent ./cmd/bot-agent

release:
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/release_bundle.py build $(RELEASE_ARGS)

verify-release:
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/release_bundle.py verify $(RELEASE_VERIFY_ARGS)

database-migrate:
	$(GO) run ./cmd/tomdb
