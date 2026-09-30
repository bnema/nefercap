.PHONY: bin build test vet race mod-check mocks mocks-check fakes-check arch perf-check check

GOBIN_DIR := $(or $(shell go env GOBIN),$(firstword $(subst :, ,$(shell go env GOPATH)))/bin)
MOCKERY ?= $(GOBIN_DIR)/mockery
HEXCHECK ?= $(GOBIN_DIR)/hexcheck

# Build a fresh binary.
bin:
	CGO_ENABLED=0 go build -o bin/nefercap ./cmd/nefercap
build:
	CGO_ENABLED=0 go build ./...
test:
	CGO_ENABLED=0 go test ./...
vet:
	CGO_ENABLED=0 go vet ./...
race:
	# cgo is enabled only for the race test binary.
	CGO_ENABLED=1 go test -race ./...
# go.mod and go.sum must be tidy (prints the diff and fails otherwise).
mod-check:
	go mod tidy -diff

# Regenerate from scratch so mocks of removed interfaces disappear too.
mocks:
	@test -x '$(MOCKERY)' || { echo 'mockery not found or not executable: $(MOCKERY)' >&2; exit 1; }
	rm -rf internal/mocks
	find internal -name '*_mock_test.go' -delete
	$(MOCKERY)

MOCKS := internal/mocks ':(glob)internal/**/*_mock_test.go'
# Regenerate, then require the mocks to be tracked and unchanged.
mocks-check: mocks
	git diff --exit-code -- $(MOCKS)
	@test -z "$$(git ls-files --others --exclude-standard -- $(MOCKS))" || { echo 'untracked generated mocks: commit them' >&2; exit 1; }

# Test doubles come from Mockery only: no handwritten fake/stub/spy types.
# grep exit 1 (no match) passes; 0 (match) and 2 (error) fail.
FAKES := ^\s*type\s+\w*(fake|stub|spy|dummy|mock)\w*|^\s*\w*(fake|stub|spy|dummy|mock)\w*(\[[^]]*\])?\s+(struct|interface)\b
fakes-check:
	@dirs=$$(ls -d internal cmd 2>/dev/null); [ -n "$$dirs" ] || exit 0; \
	rc=0; grep -rniE '$(FAKES)' --include='*_test.go' --exclude='*_mock_test.go' $$dirs || rc=$$?; \
	[ $$rc -eq 1 ] || { [ $$rc -eq 0 ] && echo 'handwritten test double: add an interface and a Mockery entry (see AGENTS.md)' >&2; exit 1; }
arch:
	$(HEXCHECK) -hexcheck.config .hexcheck.yaml -hexcheck.root . ./...
# Steady-state allocation guards. PERF_TESTS lists exact test names and PERF_PKGS
# the packages holding them. Every named test must exist, so the guard cannot
# pass vacuously when a frame-path test is removed or renamed.
PERF_TESTS := TestFrameAllocations TestSelectionAllocations TestPickerAllocations TestMonitorGridAllocations TestMonitorGridPhysicalAllocations TestGridAllocations TestGridModeAllocations TestSelectorAllocations TestRefreshAllocations TestVideoRowsAllocations TestWriteSteadyStateAllocations TestResolveOutputAllocations TestCaptureAllocations
PERF_PKGS := ./internal/ports ./internal/core ./internal/adapters/selection ./internal/adapters/indicator ./internal/adapters/ffmpeg ./internal/app ./internal/adapters/wayland
perf_empty :=
perf_space := $(perf_empty) $(perf_empty)
PERF_RE := ^($(subst $(perf_space),|,$(strip $(PERF_TESTS))))$$
perf-check:
	@out=$$(CGO_ENABLED=0 go test -list '$(PERF_RE)' $(PERF_PKGS)) || { echo "$$out" >&2; exit 1; }; \
	for t in $(PERF_TESTS); do \
		printf '%s\n' "$$out" | grep -qx "$$t" || { echo "perf-check: test $$t not found in $(PERF_PKGS)" >&2; exit 1; }; \
	done
	CGO_ENABLED=0 go test $(PERF_PKGS) -run '$(PERF_RE)' -count=1
check: mod-check vet test arch fakes-check perf-check
