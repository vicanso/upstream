.PHONY: default test test-cover dev

bench:
	go test -bench=. ./...

# for test
test:
	go test -race -cover ./...

test-cover:
	go test -race -coverprofile=test.out ./... && go tool cover --html=test.out
