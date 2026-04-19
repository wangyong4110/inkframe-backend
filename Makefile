.PHONY: all build run test clean docker-build docker-run lint fmt vet deps

# Variables
APP_NAME := inkframe-backend
BUILD_DIR := ./bin
MAIN_PATH := ./cmd/server
DOCKER_IMAGE := inkframe/$(APP_NAME):latest

# Go commands
GOCMD := go
GOBUILD := $(GOCMD) build
GOTEST := $(GOCMD) test
GOCLEAN := $(GOCMD) clean
GOGET := $(GOCMD) get
GOMOD := $(GOCMD) mod
GOFMT := gofmt
GOVET := go vet

# Build flags
LDFLAGS := -ldflags "-s -w"

all: deps build

# Download dependencies
deps:
	$(GOMOD) download
	$(GOMOD) tidy

# Build the application
build:
	mkdir -p $(BUILD_DIR)
	$(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(APP_NAME) $(MAIN_PATH)

# Run the application
run: build
	$(BUILD_DIR)/$(APP_NAME)

# Run tests
test:
	$(GOTEST) -v -race -cover ./...

# Run tests with coverage
test-coverage:
	$(GOTEST) -v -race -coverprofile=coverage.out ./...
	$(GOCMD) tool cover -html=coverage.out -o coverage.html

# Clean build artifacts
clean:
	$(GOCLEAN)
	rm -rf $(BUILD_DIR)
	rm -f coverage.out coverage.html

# Format code
fmt:
	$(GOFMT) -s -w .

# Vet code
vet:
	$(GOVET) ./...

# Lint code
lint:
	golangci-lint run

# Build Docker image
docker-build:
	docker build -t $(DOCKER_IMAGE) -f deployments/Dockerfile .

# Run Docker container
docker-run: docker-build
	docker run -p 8080:8080 --rm $(DOCKER_IMAGE)

# Build for Linux (cross-compile)
build-linux:
	GOOS=linux GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(APP_NAME)-linux $(MAIN_PATH)

# Build for Windows (cross-compile)
build-windows:
	GOOS=windows GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(APP_NAME).exe $(MAIN_PATH)

# Generate mocks (requires mockgen)
mocks:
	mockgen -source=internal/repository/repository.go -destination=internal/repository/mocks/mock_repository.go

# Generate API docs (requires swag)
docs:
	swag init -g cmd/server/main.go -o docs/swagger

# Database migration (requires migrate)
migrate-up:
	migrate -path ./migrations -database "mysql://root:password@localhost:3306/inkframe?sslmode=disable" up

migrate-down:
	migrate -path ./migrations -database "mysql://root:password@localhost:3306/inkframe?sslmode=disable" down

# Development helpers
dev:
	reflex -r '\.go$' -R '_test.go' -s -- sh -c 'go build -o /tmp/inkframe && /tmp/inkframe'

# Help
help:
	@echo "Available targets:"
	@echo "  all           - Download deps and build"
	@echo "  deps          - Download Go dependencies"
	@echo "  build         - Build the application"
	@echo "  run           - Build and run the application"
	@echo "  test          - Run tests"
	@echo "  test-coverage - Run tests with coverage report"
	@echo "  clean         - Clean build artifacts"
	@echo "  fmt           - Format code"
	@echo "  vet           - Vet code"
	@echo "  lint          - Run linter"
	@echo "  docker-build  - Build Docker image"
	@echo "  docker-run    - Run Docker container"
	@echo "  build-linux   - Cross-compile for Linux"
	@echo "  build-windows - Cross-compile for Windows"
	@echo "  migrate-up    - Run database migrations"
	@echo "  migrate-down  - Rollback database migrations"
	@echo "  docs          - Generate API documentation"
	@echo "  help          - Show this help message"
