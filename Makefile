BIN := serialtap
GO := go

.PHONY: build test cover lint fmt vet clean

# internal/testutil is test-only helpers; keep it out of the coverage denominator.
PKGS := $(shell $(GO) list ./... | grep -v /internal/testutil)

build:
	$(GO) build -o $(BIN) .

test:
	$(GO) test -race -count=1 ./...

cover:
	$(GO) test -count=1 -covermode=atomic -coverprofile=cover.out $(PKGS)
	@$(GO) tool cover -func cover.out | tail -1
	@rm -f cover.out

lint:
	golangci-lint run

fmt:
	gofmt -l -w $(wildcard *.go)

vet:
	$(GO) vet ./...

clean:
	rm -f $(BIN) cover.out
