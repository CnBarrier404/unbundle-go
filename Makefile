.PHONY: build fmt test vet

build:
	go build -o bin/ ./src/cmd/unbundle

fmt:
	go fmt ./...
	npx --no-install prettier --write .

test:
	go test ./...

vet:
	go vet ./...

