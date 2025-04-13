package services

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/sirupsen/logrus"
)

// SecurityConfig définit les limites de sécurité
type SecurityConfig struct {
	MaxCPUs     int64  `json:"max_cpus"`
	MaxMemoryMB int64  `json:"max_memory_mb"`
	MaxExecTime int    `json:"max_exec_time_seconds"`
	WorkingDir  string `json:"working_dir"`
	StorageKey  []byte `json:"storage_key"` // Clé pour chiffrer les algorithmes stockés
}

// AlgorithmMetadata contient les informations sur l'algorithme fourni
type AlgorithmMetadata struct {
	Language    string            `json:"language"`
	BuildCmd    string            `json:"build_cmd"`
	RunCmd      string            `json:"run_cmd"`
	EntryPoints map[string]string `json:"entry_points"` // "encrypt" et "decrypt"
}

// ValidationResult contient les résultats de la validation d'un algorithme
type ValidationResult struct {
	IsValid     bool     `json:"is_valid"`
	Errors      []string `json:"errors"`
	Warnings    []string `json:"warnings"`
	Performance struct {
		EncryptionTime time.Duration `json:"encryption_time"`
		DecryptionTime time.Duration `json:"decryption_time"`
		MemoryUsage    int64         `json:"memory_usage"`
	} `json:"performance"`
}

// SecureEncryptionService est une version sécurisée du service de chiffrement
type SecureEncryptionService struct {
	docker     *client.Client
	config     SecurityConfig
	storageDir string
	keyStorage *KeyStorage
}

// KeyStorage gère le stockage sécurisé des clés
type KeyStorage struct {
	storageKey []byte
	storageDir string
}

// Logger global du service
var logger *logrus.Logger

// NewSecureEncryptionService crée une nouvelle instance du service sécurisé
func NewSecureEncryptionService(config SecurityConfig) (*SecureEncryptionService, error) {
	docker, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	keyStorage, err := NewKeyStorage(config.StorageKey, filepath.Join(config.WorkingDir, "keys"))
	if err != nil {
		return nil, fmt.Errorf("failed to initialize key storage: %w", err)
	}

	return &SecureEncryptionService{
		docker:     docker,
		config:     config,
		storageDir: config.WorkingDir,
		keyStorage: keyStorage,
	}, nil
}

// StoreAlgorithm stocke et valide un nouvel algorithme
func (s *SecureEncryptionService) StoreAlgorithm(ctx context.Context, userID string, files map[string][]byte, metadata AlgorithmMetadata) error {
	// Valider l'algorithme avant de le stocker
	result, err := s.ValidateAlgorithm(ctx, files, metadata)
	if err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}
	if !result.IsValid {
		return fmt.Errorf("invalid algorithm: %v", result.Errors)
	}

	// Chiffrer les fichiers de l'algorithme
	encryptedFiles := make(map[string][]byte)
	for name, content := range files {
		encrypted, err := s.encryptData(content)
		if err != nil {
			return fmt.Errorf("failed to encrypt file %s: %w", name, err)
		}
		encryptedFiles[name] = encrypted
	}

	// Stocker les fichiers chiffrés
	algoPath := filepath.Join(s.storageDir, "algorithms", userID)
	if err := os.MkdirAll(algoPath, 0700); err != nil {
		return fmt.Errorf("failed to create algorithm directory: %w", err)
	}

	for name, content := range encryptedFiles {
		if err := os.WriteFile(filepath.Join(algoPath, name), content, 0600); err != nil {
			return fmt.Errorf("failed to write file %s: %w", name, err)
		}
	}

	return nil
}

// ValidateAlgorithm vérifie qu'un algorithme respecte toutes les contraintes
func (s *SecureEncryptionService) ValidateAlgorithm(ctx context.Context, files map[string][]byte, metadata AlgorithmMetadata) (*ValidationResult, error) {
	result := &ValidationResult{IsValid: true}

	// Vérifier les fichiers requis
	if _, ok := files["metadata.json"]; !ok {
		result.Errors = append(result.Errors, "missing metadata.json")
		result.IsValid = false
	}

	// Créer un conteneur temporaire pour les tests
	containerConfig := &container.Config{
		Image:      s.getDockerImageForLanguage(metadata.Language),
		Cmd:        []string{"sh", "-c", metadata.BuildCmd},
		WorkingDir: "/app",
	}

	hostConfig := &container.HostConfig{
		Resources: container.Resources{
			Memory:   s.config.MaxMemoryMB * 1024 * 1024,
			NanoCPUs: s.config.MaxCPUs * 1e9,
		},
	}

	// Tester la compilation
	if _, err := s.runInContainer(ctx, containerConfig, hostConfig, files); err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("build failed: %v", err))
		result.IsValid = false
	}

	// Tester les performances et la conformité
	if result.IsValid {
		testData := []byte("test data for validation")
		startTime := time.Now()

		if err := s.runAlgorithmTest(ctx, "encrypt", testData, metadata); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("encryption test failed: %v", err))
			result.IsValid = false
		}

		result.Performance.EncryptionTime = time.Since(startTime)
	}

	return result, nil
}

// Encrypt chiffre les données de manière sécurisée
func (s *SecureEncryptionService) Encrypt(ctx context.Context, userID string, data []byte) ([]byte, error) {
	return s.runSecureOperation(ctx, userID, "encrypt", data)
}

// Decrypt déchiffre les données de manière sécurisée
func (s *SecureEncryptionService) Decrypt(ctx context.Context, userID string, data []byte) ([]byte, error) {
	return s.runSecureOperation(ctx, userID, "decrypt", data)
}

// runSecureOperation exécute une opération dans un conteneur isolé
func (s *SecureEncryptionService) runSecureOperation(ctx context.Context, userID, operation string, data []byte) ([]byte, error) {
	// Créer un contexte avec timeout
	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.config.MaxExecTime)*time.Second)
	defer cancel()

	// Récupérer et déchiffrer l'algorithme
	algoPath := filepath.Join(s.storageDir, "algorithms", userID)
	files, err := s.loadAndDecryptAlgorithm(algoPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load algorithm: %w", err)
	}

	var metadata AlgorithmMetadata
	if err := json.Unmarshal(files["metadata.json"], &metadata); err != nil {
		return nil, fmt.Errorf("failed to parse metadata: %w", err)
	}

	// Configurer le conteneur
	containerConfig := &container.Config{
		Image:      s.getDockerImageForLanguage(metadata.Language),
		Cmd:        []string{"sh", "-c", fmt.Sprintf(metadata.RunCmd, metadata.EntryPoints[operation])},
		WorkingDir: "/app",
	}

	hostConfig := &container.HostConfig{
		Resources: container.Resources{
			Memory:   s.config.MaxMemoryMB * 1024 * 1024,
			NanoCPUs: s.config.MaxCPUs * 1e9,
		},
	}

	// Exécuter l'opération
	outputChan := make(chan []byte, 1)
	errChan := make(chan error, 1)

	go func() {
		output, err := s.runInContainer(ctx, containerConfig, hostConfig, map[string][]byte{
			"input": data,
		})
		if err != nil {
			errChan <- err
			return
		}
		outputChan <- output
	}()

	// Attendre le résultat ou le timeout
	select {
	case output := <-outputChan:
		return output, nil
	case err := <-errChan:
		return nil, err
	case <-ctx.Done():
		return nil, fmt.Errorf("operation timed out")
	}
}

// Méthodes utilitaires pour le chiffrement du stockage
func (s *SecureEncryptionService) encryptData(data []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.config.StorageKey)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	return gcm.Seal(nonce, nonce, data, nil), nil
}

func (s *SecureEncryptionService) decryptData(data []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.config.StorageKey)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	return gcm.Open(nil, nonce, ciphertext, nil)
}

func (s *SecureEncryptionService) getDockerImageForLanguage(language string) string {
	// Map des images Docker sécurisées pour chaque langage supporté
	images := map[string]string{
		"python": "python:3.9-slim",
		"node":   "node:16-slim",
		"ruby":   "ruby:3.0-slim",
		"go":     "golang:1.17-alpine",
		"java":   "openjdk:11-jre-slim",
	}
	return images[language]
}

func NewKeyStorage(storageKey []byte, storageDir string) (*KeyStorage, error) {
	if err := os.MkdirAll(storageDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create key storage directory: %w", err)
	}

	return &KeyStorage{
		storageKey: storageKey,
		storageDir: storageDir,
	}, nil
}

// loadAndDecryptAlgorithm charge et déchiffre les fichiers d'un algorithme
func (s *SecureEncryptionService) loadAndDecryptAlgorithm(algoPath string) (map[string][]byte, error) {
	files := make(map[string][]byte)

	// Lire tous les fichiers du répertoire
	entries, err := os.ReadDir(algoPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read algorithm directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		// Lire le fichier chiffré
		encryptedData, err := os.ReadFile(filepath.Join(algoPath, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("failed to read file %s: %w", entry.Name(), err)
		}

		// Déchiffrer le contenu
		decryptedData, err := s.decryptData(encryptedData)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt file %s: %w", entry.Name(), err)
		}

		files[entry.Name()] = decryptedData
	}

	return files, nil
}

// runInContainer exécute une commande dans un conteneur Docker
func (s *SecureEncryptionService) runInContainer(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, files map[string][]byte) ([]byte, error) {
	// Créer le conteneur
	resp, err := s.docker.ContainerCreate(ctx, config, hostConfig, nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("failed to create container: %w", err)
	}
	defer s.docker.ContainerRemove(context.Background(), resp.ID, container.RemoveOptions{Force: true})

	// Copier les fichiers dans le conteneur
	for name, content := range files {
		err = s.copyToContainer(ctx, resp.ID, filepath.Join("/app", name), content)
		if err != nil {
			return nil, fmt.Errorf("failed to copy file to container: %w", err)
		}
	}

	// Démarrer le conteneur
	if err := s.docker.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("failed to start container: %w", err)
	}

	// Attendre la fin de l'exécution
	statusCh, errCh := s.docker.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		return nil, fmt.Errorf("error waiting for container: %w", err)
	case status := <-statusCh:
		if status.StatusCode != 0 {
			return nil, fmt.Errorf("container exited with status code %d", status.StatusCode)
		}
	}

	// Récupérer la sortie
	out, err := s.docker.ContainerLogs(ctx, resp.ID, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return nil, fmt.Errorf("failed to get container logs: %w", err)
	}
	defer out.Close()

	return io.ReadAll(out)
}

// copyToContainer copie un fichier dans un conteneur
func (s *SecureEncryptionService) copyToContainer(ctx context.Context, containerID, path string, content []byte) error {
	// Créer une archive tar contenant le fichier
	tarContent, err := createTarArchive(path, content)
	if err != nil {
		return fmt.Errorf("failed to create tar archive: %w", err)
	}

	// Copier l'archive dans le conteneur
	return s.docker.CopyToContainer(ctx, containerID, "/", tarContent, container.CopyToContainerOptions{})
}

// runAlgorithmTest teste l'exécution d'un algorithme
func (s *SecureEncryptionService) runAlgorithmTest(ctx context.Context, operation string, testData []byte, metadata AlgorithmMetadata) error {
	containerConfig := &container.Config{
		Image:      s.getDockerImageForLanguage(metadata.Language),
		Cmd:        []string{"sh", "-c", fmt.Sprintf(metadata.RunCmd, metadata.EntryPoints[operation])},
		WorkingDir: "/app",
	}

	hostConfig := &container.HostConfig{
		Resources: container.Resources{
			Memory:   s.config.MaxMemoryMB * 1024 * 1024,
			NanoCPUs: s.config.MaxCPUs * 1e9,
		},
	}

	_, err := s.runInContainer(ctx, containerConfig, hostConfig, map[string][]byte{
		"input": testData,
	})
	return err
}

// createTarArchive crée une archive tar contenant un fichier
func createTarArchive(path string, content []byte) (io.Reader, error) {
	// Créer un buffer pour l'archive
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	// Créer l'en-tête du fichier
	header := &tar.Header{
		Name:    path,
		Size:    int64(len(content)),
		Mode:    0644,
		ModTime: time.Now(),
	}

	// Écrire l'en-tête
	if err := tw.WriteHeader(header); err != nil {
		return nil, err
	}

	// Écrire le contenu
	if _, err := tw.Write(content); err != nil {
		return nil, err
	}

	// Fermer l'archive
	if err := tw.Close(); err != nil {
		return nil, err
	}

	return &buf, nil
}

// ServiceOptions contient toutes les options de configuration du service
type ServiceOptions struct {
	// Configuration Docker
	DockerHost      string
	DockerTLSVerify bool
	DockerCertPath  string

	// Limites de ressources
	MaxCPUs     int64
	MaxMemoryMB int64
	MaxExecTime int

	// Chemins et stockage
	WorkingDir    string
	StorageKey    []byte
	LogPath       string
	LogLevel      string
	EnableMetrics bool
}

// InitService initialise le service de chiffrement sécurisé
func InitService(opts *ServiceOptions) (*SecureEncryptionService, error) {
	// Initialisation du logger
	logger = initLogger(opts.LogPath, opts.LogLevel)

	// Validation des options
	if err := validateOptions(opts); err != nil {
		return nil, fmt.Errorf("invalid options: %w", err)
	}

	// Configuration des variables d'environnement Docker
	setDockerEnv(opts)

	// Création du client Docker
	dockerClient, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithHost(opts.DockerHost),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	// Test de la connexion Docker
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := dockerClient.Ping(ctx); err != nil {
		return nil, fmt.Errorf("failed to connect to Docker daemon: %w", err)
	}

	// Création de la configuration du service
	config := SecurityConfig{
		MaxCPUs:     opts.MaxCPUs,
		MaxMemoryMB: opts.MaxMemoryMB,
		MaxExecTime: opts.MaxExecTime,
		WorkingDir:  opts.WorkingDir,
		StorageKey:  opts.StorageKey,
	}

	// Initialisation du service
	service, err := NewSecureEncryptionService(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create secure encryption service: %w", err)
	}

	// Initialisation des métriques si activées
	if opts.EnableMetrics {
		initMetrics()
	}

	logger.Info("Secure encryption service initialized successfully")
	return service, nil
}

// initLogger initialise le système de journalisation
func initLogger(logPath, logLevel string) *logrus.Logger {
	logger := logrus.New()

	// Configuration du niveau de log
	level, err := logrus.ParseLevel(logLevel)
	if err != nil {
		level = logrus.InfoLevel
	}
	logger.SetLevel(level)

	// Configuration du format
	logger.SetFormatter(&logrus.JSONFormatter{
		TimestampFormat: time.RFC3339,
	})

	// Configuration de la sortie
	if logPath != "" {
		if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
			log.Printf("Failed to create log directory: %v", err)
			return logger
		}

		file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.Printf("Failed to open log file: %v", err)
			return logger
		}
		logger.SetOutput(file)
	}

	return logger
}

// DefaultServiceOptions retourne des options par défaut sécurisées
func DefaultServiceOptions() *ServiceOptions {
	// Générer une clé de stockage aléatoire
	storageKey := make([]byte, 32)
	if _, err := rand.Read(storageKey); err != nil {
		panic(fmt.Sprintf("failed to generate storage key: %v", err))
	}

	socketPath := os.Getenv("DOCKER_SOCKET_PATH")
	if socketPath == "" {
		socketPath = "/var/run/docker.sock" // fallback par défaut
	}

	return &ServiceOptions{
		DockerHost:      "unix://" + socketPath,
		DockerTLSVerify: true,
		MaxCPUs:         2,
		MaxMemoryMB:     512,
		MaxExecTime:     30,
		WorkingDir:      "../.encryption-service",
		StorageKey:      storageKey,
		LogPath:         "../.encryption-service/service.log",
		LogLevel:        "info",
		EnableMetrics:   true,
	}
}

// validateOptions vérifie la validité des options
func validateOptions(opts *ServiceOptions) error {
	if opts.MaxCPUs <= 0 {
		return fmt.Errorf("MaxCPUs must be positive")
	}
	if opts.MaxMemoryMB <= 0 {
		return fmt.Errorf("MaxMemoryMB must be positive")
	}
	if opts.MaxExecTime <= 0 {
		return fmt.Errorf("MaxExecTime must be positive")
	}
	if opts.WorkingDir == "" {
		return fmt.Errorf("WorkingDir must not be empty")
	}
	if len(opts.StorageKey) != 32 {
		return fmt.Errorf("StorageKey must be 32 bytes")
	}
	return nil
}

// setDockerEnv configure les variables d'environnement Docker
func setDockerEnv(opts *ServiceOptions) {
	os.Setenv("DOCKER_HOST", opts.DockerHost)
	if opts.DockerTLSVerify {
		os.Setenv("DOCKER_TLS_VERIFY", "1")
	}
	if opts.DockerCertPath != "" {
		os.Setenv("DOCKER_CERT_PATH", opts.DockerCertPath)
	}
}

// Métriques Prometheus (à implémenter selon vos besoins)
func initMetrics() {
	// TODO: Initialiser les métriques Prometheus
	// Exemple de métriques à suivre :
	// - Nombre d'opérations de chiffrement/déchiffrement
	// - Temps d'exécution des opérations
	// - Utilisation des ressources
	// - Taux d'erreur
}
