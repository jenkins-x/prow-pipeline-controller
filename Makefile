SHELL := /bin/bash
NAME := pipeline
BUILD_TARGET = build
SRC_FILES = ./cmd/pipeline/*.go

CGO_ENABLED = 0
GO := GO111MODULE=on go

REPORTS_DIR=$(BUILD_TARGET)/reports
COVER_OUT:=$(REPORTS_DIR)/cover.out
COVERFLAGS=-coverprofile=$(COVER_OUT) --covermode=count --coverpkg=./...

GOTEST := $(GO) test
# If available, use gotestsum which provides more comprehensive output
ifneq (, $(shell which gotestsum 2> /dev/null))
GOTESTSUM_FORMAT ?= standard-quiet
GOTEST := GO111MODULE=on gotestsum --junitfile $(REPORTS_DIR)/integration.junit.xml --format $(GOTESTSUM_FORMAT) --
endif

build: 
	CGO_ENABLED=$(CGO_ENABLED) $(GO) $(BUILD_TARGET) $(BUILDFLAGS) -o build/$(NAME) $(SRC_FILES)

test: make-reports-dir
	CGO_ENABLED=$(CGO_ENABLED) $(GOTEST) -count=1 $(COVERFLAGS) -failfast -short ./...

make-reports-dir:
	mkdir -p $(REPORTS_DIR)