GO       ?= go
VERSION  ?= 0.2.0-dev
BIN      := bin/linux-amd64
LDFLAGS  := -s -w -X main.version=$(VERSION)
GOBUILD  := CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags="$(LDFLAGS)"

.PHONY: all build test vet clean controller-iso slave-iso isos

all: vet test build

build:
	$(GOBUILD) -o $(BIN)/aiclusterd      ./controller/cmd/aiclusterd
	$(GOBUILD) -o $(BIN)/aicluster       ./controller/cmd/aicluster
	$(GOBUILD) -o $(BIN)/aicluster-agent ./slave-agent/cmd/aicluster-agent

vet:
	$(GO) vet ./...
	GOOS=linux $(GO) vet ./...

test:
	$(GO) test ./... -count=1

clean:
	rm -rf bin build out

# Installer ISOs (Ubuntu 24.04 autoinstall). See iso/build.sh for options;
# without xorriso on the host it builds inside Docker (iso/Dockerfile).
controller-iso slave-iso: build
	VERSION=$(VERSION) bash iso/build.sh $(@:-iso=)

isos: controller-iso slave-iso
