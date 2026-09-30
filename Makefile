.PHONY: bin build test vet race mocks mocks-check fakes-check arch perf-check check

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

# Regenerate from scratch so mocks of removed interfaces disappear too.
mocks:
	@test -x '$(MOCKERY)' || { echo 'mockery not found or not executable: $(MOCKERY)' >&2; exit 1; }
	rm -rf internal/mocks
	find internal -name '*_mock_test.go' -delete
	$(MOCKERY)

# Checksum of all generated mocks (paths and contents); empty set is valid.
MOCKSUM = { find internal -type f \( -path 'internal/mocks/*' -o -name '*_mock_test.go' \) | LC_ALL=C sort | xargs -r sha256sum; } | sha256sum
MOCKS := internal/mocks ':(glob)internal/**/*_mock_test.go'
# Regenerate and compare checksums (independent of committed state), then also
# require the mocks to be tracked and clean.
mocks-check:
	@before=$$($(MOCKSUM)); \
	$(MAKE) --no-print-directory mocks || exit $$?; \
	after=$$($(MOCKSUM)); \
	[ "$$before" = "$$after" ] || { echo 'generated mocks were stale: review and commit the regenerated files' >&2; exit 1; }
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
# Steady-state allocation guards. Extend PERF_TESTS (regexp, exact names) and
# PERF_PKGS as encoder/wayland tests appear. The preflight fails if no listed
# test matches, so the guard cannot pass vacuously.
PERF_TESTS := ^TestFrameAllocations$$
PERF_PKGS := ./internal/ports
perf-check:
	@CGO_ENABLED=0 go test -list '$(PERF_TESTS)' $(PERF_PKGS) | grep -Eq '$(PERF_TESTS)' || { echo 'perf-check: no test matches $(PERF_TESTS) in $(PERF_PKGS)' >&2; exit 1; }
	CGO_ENABLED=0 go test $(PERF_PKGS) -run '$(PERF_TESTS)' -count=1
check: vet test arch fakes-check perf-check
