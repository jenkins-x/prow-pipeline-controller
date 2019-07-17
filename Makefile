SHELL := /bin/bash
OS := $(shell uname | tr '[:upper:]' '[:lower:]')

GO_VARS := GO111MODULE=on GO15VENDOREXPERIMENT=1 CGO_ENABLED=0
BUILDFLAGS := ''

APP_NAME := pipeline
MAIN := cmd/pipeline/main.go

BUILD_DIR=build
PACKAGE_DIRS := $(shell go list ./...)
PKGS := $(subst  :,_,$(PACKAGE_DIRS))
PLATFORMS := windows linux darwin
os = $(word 1, $@)

DOCKER_REGISTRY ?= docker.io

GOMMIT_START_SHA ?= bd117413980c6f62ef9fa3361d532a164da8ac2a

FGT := $(GOPATH)/bin/fgt
GOLINT := $(GOPATH)/bin/golint
GOMMIT := $(GOPATH)/bin/gommit

.PHONY : all
all: linux test check ## Compiles, test and verifies source

.PHONY: $(PLATFORMS)
$(PLATFORMS):
	$(GO_VARS) GOOS=$(os) GOARCH=amd64 go build -ldflags $(BUILDFLAGS) -o $(BUILD_DIR)/$(APP_NAME) $(MAIN)

.PHONY : test
test: ## Runs unit tests
	$(GO_VARS) go test -v ./...

.PHONY : fmt
fmt: ## Re-formates Go source files according to standard
	@$(GO_VARS) go fmt ./...

.PHONY : clean
clean: ## Deletes the build directory with all generated artefacts
	rm -rf $(BUILD_DIR)

check: $(GOLINT) $(FGT) $(GOMMIT)
	@echo "LINTING"
	@$(FGT) $(GOLINT) ./...
	@echo "VETTING"
	@$(GO_VARS) $(FGT) go vet ./...
	@echo "CONVENTIONAL COMMIT CHECK"
	@$(GOMMIT) check range $(GOMMIT_START_SHA) $$(git log --pretty=format:'%H' -n 1)

.PHONY : run
run: $(OS) ## Runs the app locally
	$(BUILD_DIR)/$(APP_NAME)

.PHONY: skaffold-build
skaffold-build: linux ## Runs 'skaffold build'
	DOCKER_REGISTRY=$(DOCKER_REGISTRY) VERSION=$(VERSION) skaffold build -f skaffold.yaml

.PHONY: skaffold-run
skaffold-run: linux ## Runs 'skaffold run'
	DOCKER_REGISTRY=$(DOCKER_REGISTRY) VERSION=$(VERSION) skaffold run -f skaffold.yaml -p dev

.PHONY: help
help: ## Prints this help
	@grep -E '^[^.]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-40s\033[0m %s\n", $$1, $$2}'

.PHONY: release
release: linux test check next-version skaffold-build tag-release ## Creates a release
	jx step changelog --version v$(VERSION) -p $$(git merge-base $$(git for-each-ref --sort=-creatordate --format='%(objectname)' refs/tags | sed -n 2p) master) -r $$(git merge-base $$(git for-each-ref --sort=-creatordate --format='%(objectname)' refs/tags | sed -n 1p) master)

.PHONY: next-version
next-version:  ## Creates release tag and pushes release
	jx step next-version --use-git-tag-only --tag=false

.PHONY: tag-release
tag-release:  ## Creates release tag and pushes release
	git checkout $$(git rev-parse HEAD)
	git add --all
	git commit -m "release $(VERSION)" --allow-empty
	git tag -fa v$(VERSION) -m "release version $(VERSION)"
	#git push origin HEAD v$(VERSION)

# Targets to get some Go tools
$(FGT):
	@$(GO_VARS) go get github.com/GeertJohan/fgt

$(GOLINT):
	@$(GO_VARS) go get golang.org/x/lint/golint

$(GOMMIT):
	@$(GO_VARS) go get github.com/antham/gommit
