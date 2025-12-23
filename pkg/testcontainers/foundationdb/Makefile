.PHONY: test test-unit test-integration fmt vet clean

test: test-unit test-integration

test-unit:
	@echo "Running unit tests for FoundationDB testcontainer"
	go test -short -race -v ./...

test-integration:
	@echo "Running integration tests for FoundationDB testcontainer"
	go test -race -v -timeout 10m ./...

fmt:
	@echo "Formatting FoundationDB testcontainer code"
	go fmt ./...

vet:
	@echo "Vetting FoundationDB testcontainer code"
	go vet ./...

clean:
	@echo "Cleaning up test cache"
	go clean -testcache
	
test-quick:
	@echo "Running quick smoke test"
	go test -short -timeout 2m -run TestFoundationDBContainerStartup