.PHONY: test integration fuzz check demo
test:
	go test ./...
integration:
	REQUIRE_DB_TESTS=1 go test -race -count=1 ./...
fuzz:
	go test ./internal/service -run '^$$' -fuzz '^FuzzVerifySignature$$' -fuzztime=10s
	go test ./internal/service -run '^$$' -fuzz '^FuzzDecodeNormalize$$' -fuzztime=10s
check:
	test -z "$$(gofmt -l cmd internal)"
	go vet ./...
	go test -race ./...
demo:
	python3 scripts/demo.py
