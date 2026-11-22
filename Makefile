.PHONY: build fmt test vet

build:
	go build -o bin/ ./src/cmd/unbundle

fmt:
	go fmt ./...

test:
	go test ./...

vet:
	go vet ./...

