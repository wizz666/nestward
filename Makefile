VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/wizz666/nestward/internal/version.Version=$(VERSION)

.PHONY: test build cross docker run clean

test:
	go vet ./...
	go test ./...

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/nestward ./cmd/nestward

# Static binaries for the two server platforms Nestward targets.
cross:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/nestward-linux-amd64 ./cmd/nestward
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/nestward-linux-arm64 ./cmd/nestward

docker:
	docker build --build-arg VERSION=$(VERSION) -t nestward:$(VERSION) .

run: build
	./dist/nestward serve

clean:
	rm -rf dist data
