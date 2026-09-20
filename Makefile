BIN := serialtap
GO := go

.PHONY: build test cover lint fmt vet clean

build:
	$(GO) build -o $(BIN) .

test:
	$(GO) test -race -count=1 ./...

cover:
	$(GO) test -count=1 -coverpkg=$$(go list ./... | grep -v internal/testutil | tr "\n" ",") -covermode=atomic -coverprofile=cover.out ./...
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
