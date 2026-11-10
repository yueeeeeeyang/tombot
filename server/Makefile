.PHONY: fmt lint check test build release doctor

GO ?= go
FLUTTER ?= flutter

fmt:
	gofmt -w cmd internal web
	cd client && dart format lib test integration_test

lint:
	test -z "$$(gofmt -l cmd internal web)"
	$(GO) vet ./...
	cd client && dart format --output=none --set-exit-if-changed lib test integration_test
	cd client && $(FLUTTER) analyze

check:
	$(GO) build ./...
	cd client && $(FLUTTER) analyze

test:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts -p 'test_*.py'
	$(GO) test -race ./...
	cd client && $(FLUTTER) test

build:
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/tomd ./cmd/tomd

release:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -o bin/tomd-linux-amd64 ./cmd/tomd
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -o bin/tomd-linux-arm64 ./cmd/tomd

doctor: build
	./bin/tomd doctor
