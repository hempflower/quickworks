.PHONY: test build fmt ui-install ui-build ui-watch ui-check

test:
	$(MAKE) ui-build
	CGO_ENABLED=0 go test ./...

build:
	$(MAKE) ui-build
	CGO_ENABLED=0 go build ./cmd/quickworks

fmt:
	gofmt -w $$(find cmd internal -name '*.go')

ui-install:
	npm --prefix web install

ui-build:
	npm --prefix web run build

ui-watch:
	npm --prefix web run watch

ui-check: ui-build
