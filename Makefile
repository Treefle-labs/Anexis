# Nom de l'application
APP_NAME := cloud_beast

# Répertoires des fichiers
SRC_DIR := ./cmd
BIN_DIR := ./bin
BUILD_DIR := ./build
TAILWIND_DIR := ./assets/css

# Commandes externes
GO := go
GOROOT := $(shell go env GOROOT)
GOPATH := $(shell go env GOPATH)
GOW := $(GOPATH)/bin/gow
PNPM := pnpm

# Commande pour le build
build: install-deps
	@echo "Building $(APP_NAME)..."
	$(PNPM) tailwind
	$(GO) build -o $(BIN_DIR)/$(APP_NAME) $(SRC_DIR)/main.go

# Commande pour nettoyer les fichiers générés
clean:
	@echo "Cleaning up..."
	rm -f $(BIN_DIR)/$(APP_NAME)
	rm -rf $(BUILD_DIR)
	@echo "Clean complete!"

clean-ts:
	@echo "Cleaning TypeScript builds..."
	rm -rf $(BUILD_DIR)/ts
	rm -rf $(TAILWIND_DIR)/dist

# Commande pour exécuter l'application
run: build
	@echo "Running $(APP_NAME)..."
	$(BIN_DIR)/$(APP_NAME)

# Commande pour effectuer les migrations de base de données
migrate:
	@echo "Running database migrations..."
	atlas migrate diff --env gorm

# Commande pour tester le code
test:
	@echo "Running tests..."
	DOCKER_SOCKET_PATH=$(DOCKER_SOCKET_PATH) $(GO) test ./...

# Commande pour afficher les variables d'environnement
env:
	@echo "Listing environment variables..."
	env

lint:
	@echo "Running the linter... Good luck! 😃"
	golangci-lint run --fix

# Installation des dépendances
install-deps:
	@echo "Installing dependencies..."
	$(PNPM) install

# Build des fichiers TypeScript
build-ts:
	@echo "Building TypeScript files..."
	$(GO) run $(SRC_DIR)/build/main.go --build-only

# Watch mode pour le développement TypeScript
watch-ts:
	@echo "Watching TypeScript files..."
	$(GO) run $(SRC_DIR)/watch/main.go

# Watch mode pour le développement (général)
dev: install-deps
	@echo "Starting development mode..."
	$(GOW) run $(SRC_DIR)/main.go

# Génération de la version de l'application
version:
	@echo "Version: $(shell git describe --tags --always)"

# Watch combiné pour TypeScript, Tailwind et application
watch:
	@echo "Starting combined watch mode..."
	$(GOW) run $(SRC_DIR)/main.go

# Commande par défaut
.PHONY: build clean clean-ts run migrate test env install-deps build-ts watch-ts dev version watch
