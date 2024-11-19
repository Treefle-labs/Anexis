# Nom de l'application
APP_NAME := cloud_beast

# Répertoire des fichiers sources
SRC_DIR := ./cmd
BIN_DIR := ./bin
BUILD_DIR := ./build

# Commande pour le build
build:
	@echo "Building $(APP_NAME)..."
	go build -o $(BIN_DIR)/$(APP_NAME) $(SRC_DIR)/main.go

# Commande pour nettoyer les fichiers générés
clean:
	@echo "Cleaning up..."
	rm -f $(BIN_DIR)/$(APP_NAME)
	rm -rf $(BUILD_DIR)

# Commande pour exécuter l'application
run: build
	@echo "Running $(APP_NAME)..."
	$(BIN_DIR)/$(APP_NAME)

# Commande pour effectuer les migrations de base de données
migrate:
	@echo "Running database migrations..."
	# Remplacez cette ligne par le code de migration de votre choix
	# Exemple avec `migrate` :
	# migrate -path ./migrations -database "your_database_url" up

# Commande pour tester le code
test:
	@echo "Running tests..."
	go test ./...

# Commande pour afficher les variables d'environnement
env:
	@echo "Listing environment variables..."
	env

# Commande pour afficher les informations de la version
version:
	@echo "Version: $(shell git describe --tags --always)"

# Commande par défaut
.PHONY: build clean run migrate test env version
