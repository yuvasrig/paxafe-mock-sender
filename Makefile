.PHONY: test lint run build

test:
	go vet ./... && go test ./...

lint:
	@command -v golangci-lint >/dev/null || { echo "golangci-lint not installed: https://golangci-lint.run/welcome/install/"; exit 1; }
	golangci-lint run

build:
	go build -o bin/sender ./cmd/sender

# Web UI against a local API: make run API=http://localhost:8080 KEY=pxk_test_...
run:
	API_BASE_URL=$(API) SENDER_API_KEY=$(KEY) go run ./cmd/sender serve
