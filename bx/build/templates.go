package build


// dockerfileTemplates mappe un identifiant d'écosystème à son template Dockerfile.
// La clé est généralement "Language-PackageManager" ou "Language-Ecosystem".
var DockerfileTemplates = map[string]string{
	// --- Go ---
	"Go-go": `
# --- Build Stage ---
# Utiliser une image Go spécifique (ajuster la version au besoin)
# ARG GOLANG_VERSION=1.21
# FROM golang:${GOLANG_VERSION}-alpine AS builder
FROM golang:1.21-alpine AS builder

# Définir le répertoire de travail
WORKDIR /app

# Installer les outils nécessaires (optionnel, ex: pour CGO)
# RUN apk add --no-cache gcc libc-dev

# Télécharger les dépendances séparément pour profiter du cache Docker
# Copier go.mod et go.sum (et go.work/go.work.sum si pertinent)
COPY go.* ./
# RUN go work sync # Décommenter si go.work est utilisé
RUN go mod download

# Copier le reste du code source
COPY . .

# Compiler l'application
# Utiliser -ldflags="-w -s" pour réduire la taille du binaire final (optionnel)
# Utiliser CGO_ENABLED=0 pour une compilation statique si possible (pas de dépendances C)
RUN CGO_ENABLED=0 go build -ldflags="-w -s" -o /app/main .

# --- Final Stage ---
# Utiliser une image minimale (alpine est petite, distroless est encore plus minimal)
# FROM gcr.io/distroless/static-debian11 AS final # Pour binaire statique (CGO_ENABLED=0)
FROM alpine:latest AS final

# Créer un utilisateur non-root pour la sécurité
RUN addgroup -S appgroup && adduser -S appuser -G appgroup
USER appuser

WORKDIR /app

# Copier le binaire compilé depuis l'étape de build
COPY --from=builder /app/main .

# Copier les assets statiques ou fichiers de configuration si nécessaire
# COPY --from=builder /app/templates ./templates
# COPY --from=builder /app/static ./static
# COPY config.yaml .

# Port exposé par l'application (ajuster si nécessaire)
EXPOSE 8080

# Commande pour lancer l'application
CMD ["./main"]

# Note: N'oubliez pas de créer un fichier .dockerignore efficace !
# Exclure .git, tmp/, *.log, .vscode/, etc. et potentiellement le binaire 'main' local.
`,

	// --- Node.js (NPM) ---
	"JavaScript-npm": `
# --- Build Stage ---
# Utiliser une image Node spécifique (ajuster la version LTS ou autre)
# ARG NODE_VERSION=18
# FROM node:${NODE_VERSION}-alpine AS builder
FROM node:18-alpine AS builder

WORKDIR /app

# Copier package.json et package-lock.json (ou npm-shrinkwrap.json)
COPY package*.json ./

# Installer les dépendances (npm ci est recommandé pour la reproductibilité)
# Utilisation du cache mount de BuildKit pour accélérer les installs répétés
RUN --mount=type=cache,target=/root/.npm \
    npm ci --only=production --ignore-scripts --prefer-offline --no-audit

# Copier le reste du code source de l'application
COPY . .

# Optionnel: Exécuter le script de build (ex: pour TypeScript, React, Vue, etc.)
# Assurez-vous que les devDependencies sont installées si nécessaire pour le build
# Si besoin de devDependencies:
# RUN --mount=type=cache,target=/root/.npm npm ci --ignore-scripts --prefer-offline --no-audit
# RUN npm run build

# --- Final Stage ---
FROM node:18-alpine AS final

WORKDIR /app

# Créer un utilisateur non-root
RUN addgroup -S appgroup && adduser -S appuser -G appgroup

# Copier les dépendances installées et le code source depuis le builder
# Important: Assurer que les permissions sont correctes pour l'utilisateur non-root
COPY --from=builder --chown=appuser:appgroup /app /app

USER appuser

# Port exposé par l'application
EXPOSE 3000

# Commande pour lancer l'application (ajuster selon votre point d'entrée)
CMD ["node", "votre-fichier-main.js"] # ou "server.js", "dist/main.js", etc.

# Note: Utilisez un .dockerignore ! Excluez node_modules, .git, *.log, dist/, build/ etc.
`,

	// --- Node.js (Yarn) ---
	"JavaScript-yarn": `
# --- Build Stage ---
# ARG NODE_VERSION=18
# FROM node:${NODE_VERSION}-alpine AS builder
FROM node:18-alpine AS builder

WORKDIR /app

# Copier package.json et yarn.lock
COPY package.json yarn.lock ./

# Installer les dépendances (yarn install --frozen-lockfile est recommandé)
# Utilisation du cache mount de BuildKit pour Yarn v1 (cache par défaut) ou v2+ (ajuster le target)
# Pour Yarn v1: /usr/local/share/.cache/yarn/v6
# Pour Yarn v2+ (PnP/node_modules): .yarn/cache ou node_modules/.yarn-cache
# Vérifiez votre configuration Yarn Berry. Ici on suppose Yarn v1 ou v2+ avec node_modules linker.
RUN --mount=type=cache,target=/usr/local/share/.cache/yarn/v6 \
    yarn install --frozen-lockfile --production --ignore-scripts --prefer-offline

# Copier le reste du code source
COPY . .

# Optionnel: Exécuter le script de build
# Si besoin de devDependencies:
# RUN --mount=type=cache,target=/usr/local/share/.cache/yarn/v6 yarn install --frozen-lockfile --ignore-scripts --prefer-offline
# RUN yarn build

# --- Final Stage ---
FROM node:18-alpine AS final
WORKDIR /app
RUN addgroup -S appgroup && adduser -S appuser -G appgroup
COPY --from=builder --chown=appuser:appgroup /app /app
USER appuser
EXPOSE 3000
CMD ["node", "votre-fichier-main.js"]
# Note: Utilisez un .dockerignore ! (node_modules, .yarn/, .git, *.log, etc.)
`,

	// --- Node.js (PNPM) ---
	"JavaScript-pnpm": `
# --- Build Stage ---
# ARG NODE_VERSION=18
# FROM node:${NODE_VERSION}-alpine AS builder
FROM node:18-alpine AS builder

# Installer pnpm globalement dans l'image de build
RUN npm install -g pnpm

WORKDIR /app

# Copier les fichiers de dépendances
COPY package.json pnpm-lock.yaml ./
# Copier .npmrc s'il existe (peut contenir des configurations de registry)
# COPY .npmrc .

# Installer les dépendances (--frozen-lockfile est implicite avec pnpm-lock.yaml)
# Utilisation du cache mount de BuildKit pour le store pnpm (par défaut ~/.pnpm-store)
RUN --mount=type=cache,target=/root/.pnpm-store \
    pnpm install --prod --prefer-offline --ignore-scripts

# Copier le reste du code source
COPY . .

# Optionnel: Exécuter le script de build
# Si besoin de devDependencies:
# RUN --mount=type=cache,target=/root/.pnpm-store pnpm install --prefer-offline --ignore-scripts
# RUN pnpm build

# --- Final Stage ---
# Il est crucial de copier correctement le store pnpm ou les node_modules
# Stratégie 1: Copier tout le répertoire /app (simple mais peut être gros)
FROM node:18-alpine AS final
WORKDIR /app
RUN addgroup -S appgroup && adduser -S appuser -G appgroup
COPY --from=builder --chown=appuser:appgroup /app /app
USER appuser
EXPOSE 3000
CMD ["node", "votre-fichier-main.js"]

# Stratégie 2 (plus complexe, pour optimiser la taille): Utiliser 'pnpm deploy'
# FROM node:18-alpine AS builder
# ... (installations comme avant) ...
# RUN pnpm build # Si nécessaire
# RUN pnpm prune --prod # Optionnel, supprime les devDeps si elles ont été installées
# RUN pnpm deploy /prod_app --prod # Crée un répertoire avec seulement les deps de prod
#
# FROM node:18-alpine AS final
# WORKDIR /app
# RUN addgroup -S appgroup && adduser -S appuser -G appgroup
# COPY --from=builder --chown=appuser:appgroup /prod_app /app # Copier le résultat de deploy
# USER appuser
# EXPOSE 3000
# CMD ["node", "votre-fichier-main.js"]

# Note: Utilisez un .dockerignore ! (node_modules, .git, *.log, etc.)
`,

	// --- Rust (Cargo) ---
	"Rust-cargo": `
# --- Build Stage (Planner) ---
# Utiliser l'image Rust officielle (ajuster version/toolchain)
# FROM rust:1.70-slim AS planner
FROM rust:1.70-slim AS planner

WORKDIR /app

# Copier uniquement les manifestes Cargo
COPY Cargo.toml Cargo.lock* ./
# Copier les manifestes des workspaces membres si nécessaire
# COPY members/*/Cargo.toml ./members/*/

# Créer un projet factice pour pré-compiler les dépendances
# Cela évite de recompiler les dépendances si seul le code src/ change
RUN mkdir src && echo "fn main() {}" > src/main.rs
# Compiler uniquement les dépendances (sans cache mount pour cette étape simple)
RUN cargo build --release --locked

# --- Build Stage (Builder) ---
# FROM rust:1.70-slim AS builder
FROM rust:1.70-slim AS builder
WORKDIR /app

# Copier les dépendances pré-compilées du planner
COPY --from=planner /app/target ./target
COPY --from=planner /usr/local/cargo/registry /usr/local/cargo/registry
COPY Cargo.toml Cargo.lock* ./
# COPY members/*/Cargo.toml ./members/*/

# Copier le code source réel
COPY src ./src
# COPY members/*/src ./members/*/

# Compiler le projet final
# Utilisation du cache mount de BuildKit pour le cache de compilation incrémentale
RUN --mount=type=cache,target=/app/target \
    --mount=type=cache,target=/usr/local/cargo/registry \
    cargo build --release --locked

# --- Final Stage ---
# Utiliser une image minimale. Debian slim est un bon compromis.
# Alpine peut nécessiter musl-tools si vous avez des dépendances C.
FROM debian:bullseye-slim AS final
# FROM alpine:latest AS final # Si compatible musl
# RUN apk add --no-cache musl-tools # Si Alpine et besoin de C

WORKDIR /app

# Créer un utilisateur non-root
RUN groupadd -r appgroup && useradd --no-log-init -r -g appgroup appuser
USER appuser

# Copier le binaire compilé
COPY --from=builder /app/target/release/your_binary_name ./ # Remplacez your_binary_name !

# Port exposé (ajuster)
EXPOSE 8000

# Commande de lancement
CMD ["./your_binary_name"]

# Note: .dockerignore est crucial ! (target/, .git, etc.)
`,

	// --- Python (Pip) ---
	"Python-Pip": `
# --- Build Stage ---
# Utiliser une image Python officielle (ajuster version)
# ARG PYTHON_VERSION=3.11
# FROM python:${PYTHON_VERSION}-slim AS builder
FROM python:3.11-slim AS builder

WORKDIR /app

# Installer les dépendances système si nécessaire (ex: pour psycopg2, Pillow)
# RUN apt-get update && apt-get install -y --no-install-recommends \
#     build-essential libpq-dev \
#     && rm -rf /var/lib/apt/lists/*

# Créer un environnement virtuel
RUN python -m venv /opt/venv
ENV PATH="/opt/venv/bin:$PATH"

# Mettre à jour pip et installer wheel
RUN pip install --upgrade pip wheel

# Copier le fichier de dépendances
COPY requirements.txt .

# Installer les dépendances dans l'environnement virtuel
# Utilisation du cache mount de BuildKit pour le cache pip
RUN --mount=type=cache,target=/root/.cache/pip \
    pip install --no-cache-dir -r requirements.txt

# Copier le reste du code source
COPY . .

# --- Final Stage ---
# FROM python:${PYTHON_VERSION}-slim AS final
FROM python:3.11-slim AS final

WORKDIR /app

# Créer un utilisateur non-root
RUN groupadd -r appgroup && useradd --no-log-init -r -g appgroup appuser

# Copier l'environnement virtuel créé dans l'étape de build
COPY --from=builder /opt/venv /opt/venv

# Copier le code de l'application
COPY --chown=appuser:appgroup . /app

# Définir le PATH pour inclure l'environnement virtuel
ENV PATH="/opt/venv/bin:$PATH"
# Empêcher Python d'écrire des fichiers .pyc
ENV PYTHONDONTWRITEBYTECODE 1
# Assurer que Python tourne en mode non-bufferisé (bon pour les logs)
ENV PYTHONUNBUFFERED 1

USER appuser

# Port exposé (ajuster)
EXPOSE 8000

# Commande de lancement (ajuster selon votre application: gunicorn, uvicorn, python main.py)
# CMD ["gunicorn", "-b", "0.0.0.0:8000", "your_project.wsgi:application"]
CMD ["python", "your_main_script.py"]

# Note: .dockerignore (venv/, __pycache__/, .git, *.log, *.db, etc.)
`,

	// --- Java (Maven) ---
	"Java-Maven": `
# --- Build Stage ---
# Utiliser une image Maven avec un JDK spécifique (ajuster versions)
# ARG MAVEN_VERSION=3.8
# ARG JDK_VERSION=17
# FROM maven:${MAVEN_VERSION}-eclipse-temurin-${JDK_VERSION}-alpine AS builder
FROM maven:3.8-eclipse-temurin-17-alpine AS builder

WORKDIR /app

# Copier le fichier pom.xml
COPY pom.xml .

# Télécharger les dépendances Maven
# Utilisation du cache mount de BuildKit pour le dépôt local Maven (.m2)
RUN --mount=type=cache,target=/root/.m2 \
    mvn dependency:go-offline -B

# Copier le code source
COPY src ./src

# Compiler et packager l'application (ex: en JAR ou WAR)
# Le cache mount ici accélère la compilation si les sources n'ont pas changé
RUN --mount=type=cache,target=/root/.m2 \
    mvn package -B -DskipTests

# --- Final Stage ---
# Utiliser une image JRE minimale (ajuster version et distribution)
# FROM eclipse-temurin:${JDK_VERSION}-jre-alpine AS final
FROM eclipse-temurin:17-jre-alpine AS final

WORKDIR /app

# Créer un utilisateur non-root
RUN addgroup -S appgroup && adduser -S appuser -G appgroup
USER appuser

# Copier l'artefact buildé (JAR/WAR) depuis l'étape de build
# Ajuster le chemin du JAR/WAR selon la configuration de votre pom.xml
COPY --from=builder /app/target/*.jar ./app.jar
# COPY --from=builder /app/target/*.war ./app.war

# Port exposé (ajuster)
EXPOSE 8080

# Commande de lancement (ajuster)
# Pour un JAR exécutable:
CMD ["java", "-jar", "app.jar"]
# Pour un WAR (nécessite un serveur d'application comme Tomcat, non inclus ici)
# CMD ["catalina.sh", "run"] # Si l'image de base était Tomcat

# Note: .dockerignore (target/, .git, .mvn/, *.log, etc.)
`,

	// Ajouter d'autres templates ici (Gradle, PHP/Composer, Ruby/Bundler, etc.)
}