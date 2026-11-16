.PHONY: admin-build fmt lint check test test-docker-engine test-host-tools test-docker-node test-image-preparation test-offline-image-import test-process-groups test-time-namespaces test-shared-browser test-server-https-browser computer-image test-computer-image build release verify-release node-release verify-node-release

GO ?= go
NPM ?= npm

admin-build:
	cd ../admin && $(NPM) ci --no-audit --no-fund
	cd ../admin && $(NPM) run build

fmt:
	gofmt -w cmd internal web deploy

lint: admin-build
	test -z "$$(gofmt -l cmd internal web deploy)"
	$(GO) vet ./...

check: admin-build
	$(GO) build ./...

test: admin-build
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

test-offline-image-import:
	$(GO) test -race -tags=dockerintegration -run '^TestLiveOfflineImageImport$$' -count=1 -v ./internal/dockernode

test-process-groups:
	$(GO) test -race -tags=cgroupintegration -run '^TestKernelServiceGroups$$' -count=1 -v ./internal/processgroup

test-time-namespaces:
	$(GO) test -race -tags=timensintegration -run '^TestKernelTimeNamespaceDeadline$$' -count=1 -v ./internal/agentchannel

test-shared-browser:
	../admin/node_modules/.bin/playwright test --config verification/playwright.config.mjs

test-server-https-browser:
	TOM_TEST_PLAYWRIGHT_MODULE=$(abspath ../admin/node_modules/playwright/index.mjs) $(GO) test -race -tags=browserintegration -run '^TestServerHTTPSBrowserHandshake$$' -count=1 -v ./internal/deployment

computer-image:
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/build_computer_image.py $(IMAGE_ARGS)

test-computer-image:
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/verify_computer_image.py $(IMAGE_TEST_ARGS)

build: admin-build
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/tomd ./cmd/tomd
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/tomnode ./cmd/tomnode
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/computer-task ./cmd/computer-task
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/computer-probe ./cmd/computer-probe
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/computer-agent ./cmd/computer-agent
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/bot-agent ./cmd/bot-agent

release:
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/release_bundle.py build $(RELEASE_ARGS)

verify-release:
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/release_bundle.py verify $(RELEASE_VERIFY_ARGS)

node-release:
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/node_release.py build $(NODE_RELEASE_ARGS)

verify-node-release:
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/node_release.py verify $(NODE_RELEASE_VERIFY_ARGS)
