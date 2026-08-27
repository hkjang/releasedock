SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

.PHONY: help test build package clean

help:
	@echo "make test     Run backend, runner, and web tests"
	@echo "make build    Build production binaries and web assets"
	@echo "make package  Create the offline release archive"
	@echo "make clean    Remove generated outputs"

test:
	cd backend && go test ./...
	cd runner && go test ./...
	cd web && npm test -- --run

build:
	./scripts/build.sh

package:
	./scripts/package-release.sh

clean:
	rm -rf dist release web/dist
