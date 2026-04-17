.PHONY: web-build web-watch build test clean all

# Build the web frontend bundle
web-build:
	cd web && npm ci && npm run build

# Watch mode for development
web-watch:
	cd web && npm ci && npm run watch

# Build the Go server
build:
	go build ./...

# Build everything (web + server)
all: web-build build

# Run tests
test:
	go test ./...

# Clean build artifacts
clean:
	rm -f web/app.bundle.js web/app.bundle.js.map
	rm -f server