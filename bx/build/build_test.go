// build_test.go
package build

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	// Go-Git imports pour le repo local de test
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// --- Mocks & Helpers ---

// MockSecretFetcher pour les tests unitaires
type MockSecretFetcher struct {
	Secrets map[string]string
	Err     error
}

func (m *MockSecretFetcher) GetSecret(ctx context.Context, source string) (string, error) {
	if m.Err != nil {
		return "", m.Err
	}
	val, ok := m.Secrets[source]
	if !ok {
		return "", fmt.Errorf("secret source '%s' not found in mock", source)
	}
	return val, nil
}

// Helper pour créer un fichier temporaire
func createTempFile(t *testing.T, dir, filename, content string) string {
	t.Helper()
	path := filepath.Join(dir, filename)
	err := os.WriteFile(path, []byte(content), 0644)
	require.NoError(t, err)
	return path
}

// Helper pour créer un répertoire temporaire
func createTempDir(t *testing.T, parent, name string) string {
	t.Helper()
	path := filepath.Join(parent, name)
	err := os.MkdirAll(path, 0755)
	require.NoError(t, err)
	return path
}

// Helper pour vérifier si une image Docker existe
func dockerImageExists(t *testing.T, cli *client.Client, imageRef string) bool {
	t.Helper()
	_, _, err := cli.ImageInspectWithRaw(context.Background(), imageRef)
	return err == nil
}

// Helper pour supprimer une image Docker (avec force)
func removeDockerImage(t *testing.T, cli *client.Client, imageRef string) {
	t.Helper()
	if !dockerImageExists(t, cli, imageRef) {
		return // N'existe pas déjà
	}
	_, err := cli.ImageRemove(context.Background(), imageRef, image.RemoveOptions{Force: true, PruneChildren: true})
	// Ne pas faire échouer le test si le remove échoue (peut arriver dans certains cas), juste logguer.
	if err != nil {
		t.Logf("Warning: failed to remove docker image %s: %v", imageRef, err)
	} else {
		t.Logf("Successfully removed docker image %s", imageRef)
	}
}

// Helper pour initialiser un dépôt Git local pour les tests
func setupLocalGitRepo(t *testing.T, dir string, files map[string]string) (string, string) {
	t.Helper()
	repoDir := filepath.Join(dir, "test-repo.git")
	err := os.MkdirAll(repoDir, 0755)
	require.NoError(t, err)

	// Initialiser le dépôt bare pour simuler un remote
	_, err = git.PlainInit(repoDir, true)
	require.NoError(t, err)

	// Cloner ce dépôt bare dans un répertoire de travail temporaire
	workDir := filepath.Join(dir, "test-repo-work")
	repo, err := git.PlainClone(workDir, false, &git.CloneOptions{
		URL: repoDir, // Cloner depuis le bare repo local
	})
	require.NoError(t, err)

	// Ajouter des fichiers et commiter
	w, err := repo.Worktree()
	require.NoError(t, err)

	for name, content := range files {
		filename := filepath.Join(workDir, name)
		err = os.WriteFile(filename, []byte(content), 0644)
		require.NoError(t, err)
		_, err = w.Add(name)
		require.NoError(t, err)
	}

	commit, err := w.Commit("Initial commit for testing", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test Author",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	require.NoError(t, err)

	// Push vers le dépôt bare (qui sert de 'remote')
	err = repo.Push(&git.PushOptions{})
	// Ignorer "already up-to-date" car on vient de commiter
	if err != nil && err != git.NoErrAlreadyUpToDate {
		require.NoError(t, err)
	}

	// Retourner l'URL du dépôt bare et le hash du commit
	return "file://" + repoDir, commit.String()
}

// --- Tests Unitaires ---

func TestLoadBuildSpec_ValidYAML(t *testing.T) {
	yamlData := `
name: my-app
version: 1.0.1
codebases:
  - name: backend
    source_type: git
    source: https://github.com/test/backend.git
    branch: main
build_config:
  dockerfile: Dockerfile.backend
  tags: ["app:latest", "app:1.0.1"]
env:
  API_URL: /api/v1
env_files:
  - .env.prod
secrets:
  - name: DB_PASSWORD
    source: backend/db/password
resources:
  - url: http://example.com/data.zip
    target_path: backend/data.zip
    extract: true
run_config_def:
  generate: true
  artifact_storage: local
`
	spec, err := LoadBuildSpecFromBytes([]byte(yamlData), ".yaml")
	require.NoError(t, err)
	require.NotNil(t, spec)

	assert.Equal(t, "my-app", spec.Name)
	assert.Equal(t, "1.0.1", spec.Version)
	require.Len(t, spec.Codebases, 1)
	assert.Equal(t, "backend", spec.Codebases[0].Name)
	assert.Equal(t, "git", spec.Codebases[0].SourceType)
	assert.Equal(t, "Dockerfile.backend", spec.BuildConfig.Dockerfile)
	assert.Contains(t, spec.BuildConfig.Tags, "app:latest")
	assert.Equal(t, "/api/v1", spec.Env["API_URL"])
	assert.Contains(t, spec.EnvFiles, ".env.prod")
	require.Len(t, spec.Secrets, 1)
	assert.Equal(t, "DB_PASSWORD", spec.Secrets[0].Name)
	assert.Equal(t, "backend/db/password", spec.Secrets[0].Source)
	require.Len(t, spec.Resources, 1)
	assert.Equal(t, "http://example.com/data.zip", spec.Resources[0].URL)
	assert.True(t, spec.Resources[0].Extract)
	assert.True(t, spec.RunConfigDef.Generate)
	assert.Equal(t, "local", spec.RunConfigDef.ArtifactStorage)
}

func TestLoadBuildSpec_InvalidFormat(t *testing.T) {
	invalidData := `{"name": "test",` // Invalid JSON
	_, err := LoadBuildSpecFromBytes([]byte(invalidData), ".json")
	require.Error(t, err)
}

func TestLoadBuildSpec_MissingRequiredFields(t *testing.T) {
	yamlData := `version: 1.0.0` // Missing name
	_, err := LoadBuildSpecFromBytes([]byte(yamlData), ".yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "name' et 'version' sont requis")

	yamlData = `name: test` // Missing version
	_, err = LoadBuildSpecFromBytes([]byte(yamlData), ".yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "name' et 'version' sont requis")

	yamlData = `name: test
version: 1.0.0` // Missing codebases/dockerfile/compose
	_, err = LoadBuildSpecFromBytes([]byte(yamlData), ".yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "aucune codebase, build_step, dockerfile ou compose_file")
}

func TestLoadComposeFile_DirectYAML(t *testing.T) {
	composeData := `
version: '3.8'
services:
  web:
    build:
      context: ./frontend
      dockerfile: Dockerfile.prod
      args:
        NODE_ENV: production
    ports:
      - "8080:80"
    environment:
      API_HOST: api_service
      TZ: UTC
  api:
    image: my-api:latest
    environment:
      DB_HOST: db
`
	project, err := LoadComposeFile([]byte(composeData))
	require.NoError(t, err)
	require.NotNil(t, project)

	require.Contains(t, project.Services, "web")
	require.Contains(t, project.Services, "api")

	webService := project.Services["web"]
	require.NotNil(t, webService.Build)
	assert.Equal(t, "./frontend", webService.Build.Context)
	assert.Equal(t, "Dockerfile.prod", webService.Build.Dockerfile)
	require.NotNil(t, webService.Build.Args)
	require.Contains(t, webService.Build.Args, "NODE_ENV")
	assert.Equal(t, "production", *webService.Build.Args["NODE_ENV"])
	require.Len(t, webService.Ports, 1)
	assert.Equal(t, "8080:80", webService.Ports[0]) // Note: compose-go parses ports into a struct
	require.NotNil(t, webService.Environment)
	assert.Equal(t, "api_service", *webService.Environment["API_HOST"])

	apiService := project.Services["api"]
	assert.Nil(t, apiService.Build)
	assert.Equal(t, "my-api:latest", apiService.Image)
	require.NotNil(t, apiService.Environment)
	assert.Equal(t, "db", *apiService.Environment["DB_HOST"])
}

func TestGenerateRunYAML_SimpleDocker(t *testing.T) {
	spec := &BuildSpec{
		Name:    "my-app",
		Version: "1.1.0",
		BuildConfig: BuildConfig{
			Tags: []string{"test/app:1.1", "test/app:latest"},
		},
		RunConfigDef: RunConfigDef{
			Generate:        true,
			ArtifactStorage: "docker", // Important pour la référence d'image
		},
		Env: map[string]string{"GLOBAL_VAR": "global_value"},
		Secrets: []SecretSpec{
			{Name: "MY_SECRET", Source: "secret/path"},
		},
	}
	result := &BuildResult{
		Success:  true,
		ImageID:  "sha256:abcdef123456",
		ImageIDs: map[string]string{"my-app": "sha256:abcdef123456"}, // Simuler le résultat
	}
	runtimeEnv := map[string]string{
		"GLOBAL_VAR": "global_value",
		"MY_SECRET":  "secret_value", // Simuler le secret récupéré
	}
	finalImageTags := map[string][]string{
		"my-app": {"test/app:1.1", "test/app:latest"},
	}

	// Service (qui n'existe pas ici, mais passons quand même un mock)
	mockFetcher := &MockSecretFetcher{Secrets: map[string]string{"secret/path": "secret_value"}}
	service, err := NewBuildService(t.TempDir(), true, mockFetcher)
	require.NoError(t, err)

	var composeProject *ComposeProject = nil

	runYAML, err := service.generateRunYAML(context.Background(), spec, result, runtimeEnv, finalImageTags, composeProject)
	require.NoError(t, err)
	require.NotNil(t, runYAML)

	assert.Equal(t, "1.0", runYAML.Version)
	require.Contains(t, runYAML.Services, "my-app")

	runService := runYAML.Services["my-app"]
	assert.Equal(t, "test/app:1.1", runService.Image) // Devrait utiliser le premier tag pour Docker storage
	require.NotNil(t, runService.Environment)
	assert.Equal(t, "global_value", runService.Environment["GLOBAL_VAR"])
	assert.Equal(t, "secret_value", runService.Environment["MY_SECRET"])
}

func TestGenerateRunYAML_ComposeLocal(t *testing.T) {
	spec := &BuildSpec{
		Name:    "compose-proj",
		Version: "dev",
		BuildConfig: BuildConfig{
			ComposeFile: "docker-compose.yml", // Important pour déclencher la logique compose
		},
		RunConfigDef: RunConfigDef{
			Generate:        true,
			ArtifactStorage: "local", // Important pour la référence d'image
		},
	}
	// Simuler un résultat de build compose
	result := &BuildResult{
		Success: true,
		ImageIDs: map[string]string{
			"web": "sha256:web123",
			"api": "sha256:api456",
		},
		LocalImagePaths: map[string]string{ // Important pour artifactStorage=local
			"web": "/output/compose-proj_web.tar",
			"api": "/output/compose-proj_api.tar",
		},
		ServiceOutputs: map[string]ServiceOutput{
			"web": {ImageID: "sha256:web123"},
			"api": {ImageID: "sha256:api456"},
		},
	}
	runtimeEnv := map[string]string{"GLOBAL": "on"}
	finalImageTags := map[string][]string{ // Tags générés par défaut par buildComposeProject
		"web": {"compose-proj_web:latest"},
		"api": {"compose-proj_api:latest"},
	}

	// Créer un faux fichier compose pour que generateRunYAML puisse le relire (simplifié)
	tempDir := t.TempDir()
	composeContent := `
services:
  web:
    build: ./web
    ports: ["80:80"]
    environment: { WEB_VAR: web_val }
    depends_on: [api]
  api:
    build: ./api
    environment: { API_VAR: api_val }
`
	createTempFile(t, tempDir, "docker-compose.yml", composeContent)
	spec.BuildConfig.ComposeFile = "docker-compose.yml" // Assurer que le chemin est relatif

	// Simuler un service (pas besoin de fetcher ici)
	// On doit mettre le workDir pour que la relecture du compose file fonctionne
	service, err := NewBuildService(tempDir, true, nil)
	require.NoError(t, err)
	// Simuler le buildDir qui aurait été créé par Build()
	buildSpecificDir, err := os.MkdirTemp(tempDir, fmt.Sprintf("%s-%s-%d", spec.Name, spec.Version, time.Now().UnixNano()))
	require.Nil(t, err)
	createTempFile(t, buildSpecificDir, spec.BuildConfig.ComposeFile, composeContent)

	parsedComposeProject, err := LoadComposeFile([]byte(composeContent))
	require.Nil(t, err)

	runYAML, err := service.generateRunYAML(context.Background(), spec, result, runtimeEnv, finalImageTags, parsedComposeProject)
	require.NoError(t, err)
	require.NotNil(t, runYAML)

	require.Len(t, runYAML.Services, 2)
	require.Contains(t, runYAML.Services, "web")
	require.Contains(t, runYAML.Services, "api")

	webSvc := runYAML.Services["web"]
	assert.Equal(t, "compose-proj_web.tar", webSvc.Image) // Référence locale
	assert.Equal(t, "web_val", webSvc.Environment["WEB_VAR"])
	assert.Equal(t, "on", webSvc.Environment["GLOBAL"]) // Variable globale héritée
	assert.Contains(t, webSvc.Ports, "80:80")
	assert.Contains(t, webSvc.DependsOn, "api")

	apiSvc := runYAML.Services["api"]
	assert.Equal(t, "compose-proj_api.tar", apiSvc.Image)
	assert.Equal(t, "api_val", apiSvc.Environment["API_VAR"])
	assert.Equal(t, "on", apiSvc.Environment["GLOBAL"])
}

// Helper pour créer une archive tar.gz en mémoire (Alternative)
func createTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()

	// 1. Créer l'archive TAR dans un buffer
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)

	for name, content := range files {
		hdr := &tar.Header{
			Name:    name,
			Mode:    0644,
			Size:    int64(len(content)),
			ModTime: time.Now(), // Ajouter un ModTime peut aider dans certains cas
		}
		err := tw.WriteHeader(hdr)
		require.NoError(t, err, "Failed to write tar header for %s", name)
		_, err = tw.Write([]byte(content))
		require.NoError(t, err, "Failed to write tar content for %s", name)
	}

	// Fermer le writer TAR est crucial pour écrire les blocs de fin
	err := tw.Close()
	require.NoError(t, err, "Failed to close tar writer")

	// 2. Compresser le buffer TAR résultant en Gzip
	var gzBuf bytes.Buffer
	gzw := gzip.NewWriter(&gzBuf)
	_, err = gzw.Write(tarBuf.Bytes())
	require.NoError(t, err, "Failed to write tar data to gzip writer")

	// Fermer le writer Gzip est crucial pour écrire le footer gzip
	err = gzw.Close()
	require.NoError(t, err, "Failed to close gzip writer")

	return gzBuf.Bytes()
}

// Modifier aussi l'appel dans TestExtractTarGz pour être sûr
func TestExtractTarGz(t *testing.T) {
	files := map[string]string{
		"file1.txt":           "hello",
		"dir1/file2.txt":      "world",
		"dir1/dir2/file3.txt": "nested",
	}
	tarGzData := createTarGz(t, files)
	require.NotEmpty(t, tarGzData, "tar.gz data should not be empty") // Vérif simple
	tempDir := t.TempDir()

	// Créer les lecteurs
	gzr, err := gzip.NewReader(bytes.NewReader(tarGzData))
	require.NoError(t, err, "Failed to create gzip reader")
	defer gzr.Close() // Bonne pratique de fermer le gzip reader

	tr := tar.NewReader(gzr)

	// Appeler extractTar (la fonction à tester)
	err = extractTar(tr, tempDir) // Note: extractTar n'est pas défini dans build_test.go, il utilise celui de build.go
	require.NoError(t, err, "extractTar failed")

	// Vérifier que les fichiers existent (assertions originales)
	// ... (assertions inchangées) ...
	content1, err := os.ReadFile(filepath.Join(tempDir, "file1.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(content1))

	content2, err := os.ReadFile(filepath.Join(tempDir, "dir1/file2.txt"))
	require.NoError(t, err)
	assert.Equal(t, "world", string(content2))

	content3, err := os.ReadFile(filepath.Join(tempDir, "dir1/dir2/file3.txt"))
	require.NoError(t, err)
	assert.Equal(t, "nested", string(content3))
}

// --- Tests d'Intégration (nécessitent Docker) ---

// Fonction pour skipper les tests d'intégration si Docker n'est pas dispo
func skipWithoutDocker(t *testing.T) {
	t.Helper()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Skipf("Skipping integration test: Docker client could not be initialized: %v", err)
	}
	_, err = cli.Ping(context.Background())
	if err != nil {
		t.Skipf("Skipping integration test: Docker daemon is not responding: %v", err)
	}
	// Fermer le client ping pour ne pas laisser de connexions ouvertes
	cli.Close()
}

func TestIntegration_BuildSimpleDockerfile_LocalOutput(t *testing.T) {
	// //go:build integration
	skipWithoutDocker(t)
	t.Parallel() // Peut être exécuté en parallèle avec d'autres tests d'intégration

	tempDir := t.TempDir()
	mockFetcher := &MockSecretFetcher{Secrets: map[string]string{"my/secret": "verysecret"}}

	// Créer le service de build
	service, err := NewBuildService(tempDir, false, mockFetcher) // Utiliser le système de fichiers
	require.NoError(t, err)

	// Créer une codebase locale simple
	codeDir := createTempDir(t, tempDir, "mycode")
	dockerfileContent := `
ARG BASE_IMG=alpine:latest
FROM ${BASE_IMG}
ARG my_arg=default_val
ENV MY_ARG_ENV=$my_arg
ENV MY_OTHER_ENV=static_value
COPY content.txt /app/
WORKDIR /app
CMD ["cat", "content.txt"]
`
	createTempFile(t, codeDir, "Dockerfile", dockerfileContent)
	createTempFile(t, codeDir, "content.txt", "Hello Docker Build!")

	// Définir le BuildSpec
	spec := &BuildSpec{
		Name:    "integ-test-simple",
		Version: fmt.Sprintf("0.1.0-%d", time.Now().Unix()),
		Codebases: []CodebaseConfig{
			{Name: "main", SourceType: "local", Source: codeDir},
		},
		BuildConfig: BuildConfig{
			Dockerfile:   "main/Dockerfile", // Chemin relatif au buildDir implicite
			Tags:         []string{fmt.Sprintf("integ-test-simple:%s", fmt.Sprintf("0.1.0-%d", time.Now().Unix()))},
			Args:         map[string]string{"my_arg": "override_val"},
			OutputTarget: "local", // Tester la sortie locale
		},
		RunConfigDef: RunConfigDef{Generate: true, ArtifactStorage: "local"},
		Env:          map[string]string{"RUN_ENV": "run_value"},
		Secrets:      []SecretSpec{{Name: "SECRET_ENV", Source: "my/secret"}},
	}
	imageTag := spec.BuildConfig.Tags[0] // Tag principal

	// S'assurer que l'image est nettoyée à la fin
	cli, _ := client.NewClientWithOpts(client.FromEnv) // Récupérer un client pour le cleanup
	t.Cleanup(func() {
		removeDockerImage(t, cli, imageTag)
		cli.Close()
		// Le tempDir est nettoyé automatiquement par Go
	})

	// Exécuter le build
	ctx := context.Background()
	result, err := service.Build(ctx, spec)

	// Assertions
	require.NoError(t, err, "Build error message: %s", result.ErrorMessage) // Afficher le message d'erreur si le test échoue
	require.True(t, result.Success, "Build should be successful")
	require.NotEmpty(t, result.ImageIDs[spec.Name], "Image ID should not be empty")
	assert.True(t, result.ImageSizes[spec.Name] > 0, "Image size should be positive")
	assert.Contains(t, result.Logs, "Successfully built", "Build logs should indicate success")
	assert.Contains(t, result.Logs, result.ImageIDs[spec.Name][:12], "Build logs should contain image ID") // Vérifier le short ID

	// Vérifier la sortie locale
	require.NotEmpty(t, result.LocalImagePaths[spec.Name], "Local image path should be set")
	localTarPath := result.LocalImagePaths[spec.Name]
	// Vérifier que le chemin est relatif au workDir du service
	assert.True(t, strings.HasPrefix(localTarPath, service.workDir), "Local path should be inside workDir")
	_, err = os.Stat(localTarPath)
	assert.NoError(t, err, "Local image tar file should exist at %s", localTarPath)

	// Vérifier le fichier run.yml
	require.NotEmpty(t, result.RunConfigPath, "Run config path should be set")
	assert.True(t, strings.HasPrefix(result.RunConfigPath, service.workDir), "Run config path should be inside workDir")
	_, err = os.Stat(result.RunConfigPath)
	assert.NoError(t, err, "run.yml file should exist at %s", result.RunConfigPath)

	// Lire et vérifier le contenu de run.yml
	runData, err := os.ReadFile(result.RunConfigPath)
	require.NoError(t, err)
	var runContent RunYAML
	err = yaml.Unmarshal(runData, &runContent)
	require.NoError(t, err)
	require.Contains(t, runContent.Services, spec.Name)
	runService := runContent.Services[spec.Name]
	assert.Equal(t, filepath.Base(localTarPath), runService.Image) // Doit référencer le fichier tar local
	assert.Equal(t, "run_value", runService.Environment["RUN_ENV"])
	assert.Equal(t, "verysecret", runService.Environment["SECRET_ENV"]) // Secret injecté

	// Vérifier que l'image existe dans Docker (même si sortie locale, elle est buildée)
	assert.True(t, dockerImageExists(t, cli, result.ImageIDs[spec.Name]), "Docker image should exist by ID")
	assert.True(t, dockerImageExists(t, cli, imageTag), "Docker image should exist by Tag")

	// Optionnel: Charger l'image locale et vérifier son contenu
	// _, err = service.dockerClient.ImageLoad(ctx, bytes.NewReader(localTarData)) ...
}

func TestIntegration_BuildCompose_DockerOutput(t *testing.T) {
	// //go:build integration
	skipWithoutDocker(t)
	t.Parallel()

	tempDir := t.TempDir()
	service, err := NewBuildService(tempDir, false, nil) // Pas besoin de secrets ici
	require.NoError(t, err)

	// Créer les codebases locales pour les services
	webDir := createTempDir(t, tempDir, "frontend")
	createTempFile(t, webDir, "Dockerfile", "FROM alpine:latest\nCMD echo web")
	apiDir := createTempDir(t, tempDir, "backend")
	createTempFile(t, apiDir, "Dockerfile", "FROM alpine:latest\nCMD echo api")

	// Créer le fichier docker-compose.yml
	composeContent := `
version: '3.8'
services:
  web:
    build: ./frontend # Chemin relatif au fichier compose
    ports: ["8081:80"]
  api:
    build: ./backend
  nginx:
    image: nginx:alpine # Service sans build
    ports: ["80:80"]
`
	composePath := createTempFile(t, tempDir, "docker-compose.test.yml", composeContent)

	// Définir le BuildSpec
	spec := &BuildSpec{
		Name:    "integ-compose",
		Version: fmt.Sprintf("ci-%d", time.Now().Unix()),
		// Pas de Codebases ici car build.context est dans compose
		BuildConfig: BuildConfig{
			ComposeFile:  filepath.Base(composePath), // Doit être relatif au workDir du service
			OutputTarget: "docker",                   // Sortie dans Docker daemon
		},
		RunConfigDef: RunConfigDef{Generate: true, ArtifactStorage: "docker"},
	}
	webImageTag := fmt.Sprintf("%s_web:latest", spec.Name)
	apiImageTag := fmt.Sprintf("%s_api:latest", spec.Name)
	nginxImage := "nginx:alpine" // Image qui sera pull

	cli, _ := client.NewClientWithOpts(client.FromEnv)
	t.Cleanup(func() {
		removeDockerImage(t, cli, webImageTag)
		removeDockerImage(t, cli, apiImageTag)
		// Ne pas supprimer nginx:alpine, c'est une image publique
		cli.Close()
	})

	// Exécuter le build
	ctx := context.Background()
	result, err := service.Build(ctx, spec)

	// Assertions
	require.NoError(t, err, "Build error message: %s", result.ErrorMessage)
	require.True(t, result.Success, "Build should be successful")

	// Vérifier les résultats par service
	require.Len(t, result.ServiceOutputs, 3, "Should have results for web, api, and nginx") // Nginx est inclus même si pas buildé

	require.Contains(t, result.ImageIDs, "web")
	require.Contains(t, result.ImageIDs, "api")
	assert.NotEmpty(t, result.ImageIDs["web"])
	assert.NotEmpty(t, result.ImageIDs["api"])
	// nginx n'a pas d'ID de build, mais l'image devrait exister
	assert.Contains(t, result.Logs, "Pulling image 'nginx:alpine'...")

	assert.True(t, result.ImageSizes["web"] > 0)
	assert.True(t, result.ImageSizes["api"] > 0)

	// Vérifier que les images existent dans Docker avec les tags par défaut
	assert.True(t, dockerImageExists(t, cli, webImageTag), "Web image tag should exist")
	assert.True(t, dockerImageExists(t, cli, apiImageTag), "API image tag should exist")
	assert.True(t, dockerImageExists(t, cli, nginxImage), "Nginx image should exist (pulled)")

	// Vérifier run.yml
	require.NotEmpty(t, result.RunConfigPath)
	_, err = os.Stat(result.RunConfigPath)
	require.NoError(t, err)
	runData, err := os.ReadFile(result.RunConfigPath)
	require.NoError(t, err)
	var runContent RunYAML
	err = yaml.Unmarshal(runData, &runContent)
	require.NoError(t, err)

	require.Len(t, runContent.Services, 3)
	assert.Equal(t, webImageTag, runContent.Services["web"].Image)
	assert.Equal(t, apiImageTag, runContent.Services["api"].Image)
	assert.Equal(t, nginxImage, runContent.Services["nginx"].Image) // Image directe du compose
	assert.Contains(t, runContent.Services["web"].Ports, "8081:80")
}

func TestIntegration_BuildGitRepo_GoGit(t *testing.T) {
	// //go:build integration
	skipWithoutDocker(t)
	t.Parallel()

	tempDir := t.TempDir()
	service, err := NewBuildService(tempDir, false, nil)
	require.NoError(t, err)

	// Créer un repo Git local avec un Dockerfile
	dockerfileContent := "FROM alpine:latest\nRUN echo 'Built from Git!' > /app/git.txt\nCMD cat /app/git.txt"
	repoFiles := map[string]string{"Dockerfile": dockerfileContent, "README.md": "Test repo"}
	repoURL, commitHash := setupLocalGitRepo(t, tempDir, repoFiles)
	t.Logf("Created local git repo at %s with commit %s", repoURL, commitHash)

	// Définir le BuildSpec pour cloner et builder
	spec := &BuildSpec{
		Name:    "integ-git",
		Version: fmt.Sprintf("git-%s", commitHash[:7]),
		Codebases: []CodebaseConfig{
			{
				Name:       "app",
				SourceType: "git",
				Source:     repoURL,    // Utiliser l'URL file://
				Commit:     commitHash, // Tester le checkout par commit
				// TargetInHost: ".", // Mettre à la racine du buildDir
			},
		},
		BuildConfig: BuildConfig{
			Dockerfile:   "app/Dockerfile", // Doit trouver le Dockerfile dans le sous-dossier 'app'
			Tags:         []string{fmt.Sprintf("integ-git-test:%s", commitHash[:7])},
			OutputTarget: "docker",
		},
		RunConfigDef: RunConfigDef{Generate: false}, // Pas besoin de run.yml ici
	}
	imageTag := spec.BuildConfig.Tags[0]

	cli, _ := client.NewClientWithOpts(client.FromEnv)
	t.Cleanup(func() {
		removeDockerImage(t, cli, imageTag)
		cli.Close()
	})

	// Exécuter le build
	ctx := context.Background()
	result, err := service.Build(ctx, spec)

	// Assertions
	require.NoError(t, err, "Build error message: %s", result.ErrorMessage)
	require.True(t, result.Success, "Build should be successful")
	require.NotEmpty(t, result.ImageIDs[spec.Name])
	assert.True(t, result.ImageSizes[spec.Name] > 0)
	assert.Contains(t, result.Logs, "Cloning repository "+repoURL)
	assert.Contains(t, result.Logs, "Checking out commit "+commitHash)
	assert.Contains(t, result.Logs, "Successfully built")

	// Vérifier l'image dans Docker
	assert.True(t, dockerImageExists(t, cli, imageTag), "Docker image from Git build should exist by tag")
}

func TestIntegration_BuildWithResource(t *testing.T) {
	// //go:build integration
	skipWithoutDocker(t)
	t.Parallel()

	// Créer un serveur HTTP mock pour la ressource
	resourceContent := "This is the downloaded resource."
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/resource.txt" {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, resourceContent)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockServer.Close()

	tempDir := t.TempDir()
	service, err := NewBuildService(tempDir, false, nil)
	require.NoError(t, err)

	// Codebase simple qui utilise la ressource
	codeDir := createTempDir(t, tempDir, "res-app")
	dockerfileContent := `FROM alpine:latest
COPY data/resource.txt /app/
CMD cat /app/resource.txt
`
	createTempFile(t, codeDir, "Dockerfile", dockerfileContent)

	spec := &BuildSpec{
		Name:    "integ-res",
		Version: "1.0",
		Codebases: []CodebaseConfig{
			{Name: "app", SourceType: "local", Source: codeDir},
		},
		Resources: []ResourceConfig{
			{
				URL:        mockServer.URL + "/resource.txt",
				TargetPath: "app/data/resource.txt", // Placer dans la codebase avant build
			},
		},
		BuildConfig: BuildConfig{
			Dockerfile:   "app/Dockerfile",
			Tags:         []string{"integ-res-test:latest"},
			OutputTarget: "docker",
		},
		RunConfigDef: RunConfigDef{Generate: false},
	}
	imageTag := spec.BuildConfig.Tags[0]

	cli, _ := client.NewClientWithOpts(client.FromEnv)
	t.Cleanup(func() {
		removeDockerImage(t, cli, imageTag)
		cli.Close()
	})

	// Exécuter le build
	ctx := context.Background()
	result, err := service.Build(ctx, spec)

	// Assertions
	require.NoError(t, err, "Build error message: %s", result.ErrorMessage)
	require.True(t, result.Success)
	require.NotEmpty(t, result.ImageIDs[spec.Name])

	// Vérifier que la ressource a été téléchargée dans les logs
	assert.Contains(t, result.Logs, "Downloading "+mockServer.URL+"/resource.txt")
	// Vérifier que le fichier existe dans le buildDir temporaire avant le build (difficile à vérifier après cleanup)
	// On se fie au succès du build Docker qui dépendait de la présence du fichier

	// Vérifier l'image
	assert.True(t, dockerImageExists(t, cli, imageTag))

	// Optionnel : vérifier le contenu de l'image
	// output, err := service.ExecuteInContainer(ctx, result.ImageID, nil, nil)
	// require.NoError(t, err)
	// assert.Equal(t, resourceContent, output)
}

// TODO: Ajouter TestIntegration_BuildWithSteps (plus complexe à mettre en place)
