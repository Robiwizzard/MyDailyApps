# make build   UI + executable
# make test    Go tests (host and kit)
# make dev     run the host; use `cd ui && npm run dev` for live UI reloads
.PHONY: build ui test dev
build:
	./scripts/build.sh
ui:
	cd ui && npm install && npm run build && : > dist/.keep
test:
	go test ./...
	cd kit && go test ./...
dev:
	MYDAILYAPPS_OPEN_BROWSER=false go run ./cmd/mydailyapps
