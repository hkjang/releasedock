SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

.PHONY: help vet test build package clean start stop restart status logs doctor

help:
	@echo "make vet      Check Go formatting and run go vet on backend and runner"
	@echo "make test     Run make vet, then backend, runner, and web tests"
	@echo "make build    Build production binaries and web assets"
	@echo "make package  Create the offline release archive"
	@echo "make clean    Remove generated outputs"
	@echo ""
	@echo "Standalone control (no systemd, uses ./dist):"
	@echo "make start    Start the API server and Runner"
	@echo "make stop     Stop them"
	@echo "make restart  Restart them"
	@echo "make status   Show what is running"
	@echo "make logs     Tail the API server log"
	@echo "make doctor   Diagnose configuration and runtime problems"

vet:
	@unformatted="$$(gofmt -l backend runner)"; \
	if [[ -n "$$unformatted" ]]; then \
		echo "gofmt: the following files are not formatted:" >&2; \
		echo "$$unformatted" >&2; \
		exit 1; \
	fi
	cd backend && go vet ./...
	cd runner && go vet ./...

test: vet
	cd backend && go test ./...
	cd runner && go test ./...
	cd web && npm test -- --run

build:
	./scripts/build.sh

package:
	./scripts/package-release.sh

clean:
	rm -rf dist release web/dist

start:
	./deploy/releasedock.sh start

stop:
	./deploy/releasedock.sh stop

restart:
	./deploy/releasedock.sh restart

status:
	./deploy/releasedock.sh status

logs:
	./deploy/releasedock.sh logs server -f

doctor:
	./deploy/releasedock.sh doctor
