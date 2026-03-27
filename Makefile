.PHONY: build run test docker-up docker-down clean help

APP_NAME = exchange_server
MAIN_FILE = cmd/exchange/main.go
BUILD_DIR = bin

help: ## Show this help
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build the application
	@echo "=> Building $(APP_NAME)..."
	@go build -ldflags="-w -s" -o $(BUILD_DIR)/$(APP_NAME) $(MAIN_FILE)
	@echo "=> Build successful!"

run: build ## Run the exchange server locally
	@echo "=> Starting server..."
	@./$(BUILD_DIR)/$(APP_NAME)

dev: ## Run with hot-reloading (if you have air installed)
	@air -c .air.toml || go run $(MAIN_FILE)

test: ## Run tests
	@echo "=> Running tests..."
	@go test -v -race ./...

docker-up: ## Start the full stack using Docker Compose
	docker-compose up -d --build

docker-down: ## Stop Docker Compose
	docker-compose down -v

clean: ## Clean build artifacts
	@echo "=> Cleaning..."
	@rm -rf $(BUILD_DIR)
	@go clean
