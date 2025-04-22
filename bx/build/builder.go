package build

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/docker/pkg/stdcopy"
	"gopkg.in/yaml.v3"

	// Paquet pour B2
	"github.com/Backblaze/blazer/b2"
)

// BuildSpec représente la configuration personnalisée de build
type BuildSpec struct {
	Name        string            `json:"name" yaml:"name"`
	Version     string            `json:"version" yaml:"version"`
	Codebases   []CodebaseConfig  `json:"codebases" yaml:"codebases"`
	BuildConfig BuildConfig       `json:"build_config" yaml:"build_config"`
	Env         map[string]string `json:"env" yaml:"env"`
	Artifacts   []string          `json:"artifacts" yaml:"artifacts"`
}

// CodebaseConfig représente une base de code source
type CodebaseConfig struct {
	Name       string `json:"name" yaml:"name"`
	SourceType string `json:"source_type" yaml:"source_type"` // git, local, archive, buffer
	Source     string `json:"source" yaml:"source"`           // URL, chemin local
	Branch     string `json:"branch" yaml:"branch"`
	Commit     string `json:"commit" yaml:"commit"`
	Path       string `json:"path" yaml:"path"` // Sous-répertoire si nécessaire
	Content    []byte `json:"-" yaml:"-"`       // Contenu en mémoire pour le type buffer
}

// BuildConfig représente la configuration de build Docker
type BuildConfig struct {
	BaseImage   string            `json:"base_image" yaml:"base_image"`
	Dockerfile  string            `json:"dockerfile" yaml:"dockerfile"`
	ComposeFile string            `json:"compose_file" yaml:"compose_file"`
	Target      string            `json:"target" yaml:"target"`
	Args        map[string]string `json:"args" yaml:"args"`
	Tags        []string          `json:"tags" yaml:"tags"`
	Platforms   []string          `json:"platforms" yaml:"platforms"`
	NoCache     bool              `json:"no_cache" yaml:"no_cache"`
}

// BuildResult contient les résultats du processus de build
type BuildResult struct {
	Success       bool              `json:"success"`
	ImageID       string            `json:"image_id"`
	ImageSize     int64             `json:"image_size"`
	Artifacts     map[string][]byte `json:"artifacts"`
	BuildTime     float64           `json:"build_time"`
	ErrorMessage  string            `json:"error_message,omitempty"`
	Logs          string            `json:"logs"`
	B2ObjectNames []string          `json:"b2_object_names,omitempty"`
}

// B2Config contient les informations de configuration pour le stockage B2
type B2Config struct {
	AccountID      string `json:"account_id" yaml:"account_id"`
	ApplicationKey string `json:"application_key" yaml:"application_key"`
	BucketName     string `json:"bucket_name" yaml:"bucket_name"`
	BasePath       string `json:"base_path" yaml:"base_path"`
}

// BuildService est le service principal qui gère les builds
type BuildService struct {
	dockerClient *client.Client
	workDir      string
	b2Config     *B2Config
	mutex        sync.Mutex
	inMemory     bool // Si true, fonctionne principalement en mémoire
}

// NewBuildService crée une nouvelle instance du service de build
func NewBuildService(workDir string, inMemory bool) (*BuildService, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("erreur lors de la création du client Docker: %w", err)
	}

	// Création du répertoire de travail si nécessaire et si pas en mode mémoire
	if !inMemory && workDir != "" {
		if err := os.MkdirAll(workDir, 0755); err != nil {
			return nil, fmt.Errorf("impossible de créer le répertoire %s: %w", workDir, err)
		}
	}

	return &BuildService{
		dockerClient: cli,
		workDir:      workDir,
		inMemory:     inMemory,
		mutex:        sync.Mutex{},
	}, nil
}

// SetB2Config configure les paramètres pour le stockage B2
func (s *BuildService) SetB2Config(config *B2Config) {
	s.b2Config = config
}

// LoadBuildSpecFromFile charge une spécification de build depuis un fichier
func LoadBuildSpecFromFile(filename string) (*BuildSpec, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("impossible de lire le fichier de spécification: %w", err)
	}

	return LoadBuildSpecFromBytes(data, filepath.Ext(filename))
}

// LoadBuildSpecFromBytes charge une spécification de build depuis des données en mémoire
func LoadBuildSpecFromBytes(data []byte, format string) (*BuildSpec, error) {
	var spec BuildSpec
	var err error

	if format == ".json" {
		err = json.Unmarshal(data, &spec)
	} else if format == ".yaml" || format == ".yml" {
		err = yaml.Unmarshal(data, &spec)
	} else {
		return nil, fmt.Errorf("format non pris en charge: %s", format)
	}

	if err != nil {
		return nil, fmt.Errorf("impossible de parser la spécification: %w", err)
	}

	return &spec, nil
}

// Build exécute un build selon la spécification donnée
func (s *BuildService) Build(ctx context.Context, spec *BuildSpec) (*BuildResult, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	startTime := time.Now()
	result := &BuildResult{
		Artifacts: make(map[string][]byte),
		Logs:      "",
	}

	var buildDir string
	var cleanupFunc func()

	if s.inMemory {
		// Utilisation d'un répertoire temporaire pour les opérations nécessitant des fichiers
		tmpDir, err := os.MkdirTemp("", fmt.Sprintf("docker-build-%s-%s-", spec.Name, spec.Version))
		if err != nil {
			return result, fmt.Errorf("impossible de créer un répertoire temporaire: %w", err)
		}
		buildDir = tmpDir
		cleanupFunc = func() { os.RemoveAll(tmpDir) }
	} else {
		// Créer un répertoire de build unique dans le workDir
		buildID := fmt.Sprintf("%s-%s-%d", spec.Name, spec.Version, time.Now().Unix())
		buildDir = filepath.Join(s.workDir, buildID)
		if err := os.MkdirAll(buildDir, 0755); err != nil {
			return result, fmt.Errorf("impossible de créer le répertoire de build: %w", err)
		}
		cleanupFunc = func() { os.RemoveAll(buildDir) }
	}
	defer cleanupFunc()

	// Traiter les codebases
	for _, codebase := range spec.Codebases {
		destDir := filepath.Join(buildDir, codebase.Name)
		if err := s.fetchCodebase(ctx, codebase, destDir); err != nil {
			result.Success = false
			result.ErrorMessage = fmt.Sprintf("erreur lors de la récupération de la codebase %s: %s", codebase.Name, err)
			return result, err
		}
	}

	// Préparer le Dockerfile
	dockerfilePath := ""
	if spec.BuildConfig.Dockerfile != "" {
		dockerfilePath = filepath.Join(buildDir, "Dockerfile")
		if err := os.WriteFile(dockerfilePath, []byte(spec.BuildConfig.Dockerfile), 0644); err != nil {
			result.Success = false
			result.ErrorMessage = "erreur lors de la création du Dockerfile: " + err.Error()
			return result, err
		}
	} else {
		// Chercher un Dockerfile existant
		for _, codebase := range spec.Codebases {
			dfPath := filepath.Join(buildDir, codebase.Name, "Dockerfile")
			if _, err := os.Stat(dfPath); err == nil {
				dockerfilePath = dfPath
				break
			}
		}
		if dockerfilePath == "" {
			result.Success = false
			result.ErrorMessage = "aucun Dockerfile trouvé"
			return result, fmt.Errorf("aucun Dockerfile trouvé")
		}
	}

	// Construction de l'image Docker
	imageID, logs, err := s.buildWithDocker(ctx, buildDir, dockerfilePath, spec)
	result.Logs = logs
	if err != nil {
		result.Success = false
		result.ErrorMessage = "erreur lors du build Docker: " + err.Error()
		return result, err
	}
	result.ImageID = imageID

	// Récupérer la taille de l'image
	imageSize, err := s.getImageSize(ctx, imageID)
	if err == nil {
		result.ImageSize = imageSize
	}

	// Collecter les artefacts
	for _, artifactPath := range spec.Artifacts {
		fullPath := filepath.Join(buildDir, artifactPath)
		data, err := os.ReadFile(fullPath)
		if err != nil {
			result.Logs += fmt.Sprintf("\nAvertissement: impossible de lire l'artefact %s: %s", artifactPath, err)
			continue
		}
		result.Artifacts[artifactPath] = data
	}

	// Exporter l'image et l'envoyer au bucket B2 si configuré
	if s.b2Config != nil {
		objectNames, err := s.exportAndUploadImage(ctx, imageID, spec)
		if err != nil {
			result.Logs += fmt.Sprintf("\nAvertissement lors de l'export vers B2: %s", err)
		} else {
			result.B2ObjectNames = objectNames
		}
	}

	result.Success = true
	result.BuildTime = time.Since(startTime).Seconds()
	return result, nil
}

// fetchCodebase récupère une codebase selon sa configuration
func (s *BuildService) fetchCodebase(ctx context.Context, config CodebaseConfig, destDir string) error {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}

	switch config.SourceType {
	case "git":
		return s.fetchGitRepo(ctx, config, destDir)
	case "local":
		return s.copyLocalDir(config.Source, destDir)
	case "archive":
		return s.extractArchive(config.Source, destDir)
	case "buffer":
		// Traitement des données directement en mémoire
		if len(config.Content) == 0 {
			return fmt.Errorf("contenu vide pour la codebase de type buffer")
		}
		return s.extractBufferToDir(config.Content, destDir)
	default:
		return fmt.Errorf("type de source non pris en charge: %s", config.SourceType)
	}
}

// fetchGitRepo clone un dépôt git
func (s *BuildService) fetchGitRepo(ctx context.Context, config CodebaseConfig, destDir string) error {
	// Utiliser gitImplementation directe si disponible, sinon utiliser exec.Command
	// Ceci est un exemple simplifié qui utilise exec.Command

	cmd := fmt.Sprintf("git clone")
	if config.Branch != "" {
		cmd += fmt.Sprintf(" -b %s", config.Branch)
	}
	if config.Commit != "" {
		cmd += " --single-branch"
	}
	cmd += fmt.Sprintf(" %s %s", config.Source, destDir)

	// Exécuter la commande git
	// Note: Dans une implémentation réelle, il serait préférable d'utiliser un client Git en Go
	// comme go-git ou d'intégrer libgit2 via cgo pour éviter les dépendances externes

	return execCmd(ctx, cmd)
}

// copyLocalDir copie un répertoire local
func (s *BuildService) copyLocalDir(source, dest string) error {
	entries, err := ioutil.ReadDir(source)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		sourcePath := filepath.Join(source, entry.Name())
		destPath := filepath.Join(dest, entry.Name())

		if entry.IsDir() {
			if err := os.MkdirAll(destPath, entry.Mode()); err != nil {
				return err
			}
			if err := s.copyLocalDir(sourcePath, destPath); err != nil {
				return err
			}
		} else {
			data, err := ioutil.ReadFile(sourcePath)
			if err != nil {
				return err
			}
			if err := ioutil.WriteFile(destPath, data, entry.Mode()); err != nil {
				return err
			}
		}
	}
	return nil
}

// extractArchive extrait une archive vers un répertoire
func (s *BuildService) extractArchive(sourcePath string, destDir string) error {
	data, err := ioutil.ReadFile(sourcePath)
	if err != nil {
		return err
	}
	return s.extractBufferToDir(data, destDir)
}

// extractBufferToDir extrait des données d'archive en mémoire vers un répertoire
func (s *BuildService) extractBufferToDir(data []byte, destDir string) error {
	// Détecter le type d'archive et extraire
	if bytes.HasPrefix(data, []byte{0x1F, 0x8B}) {
		// Archive gzip (tar.gz)
		gzr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return err
		}
		defer gzr.Close()

		return extractTar(tar.NewReader(gzr), destDir)
	} else if bytes.HasPrefix(data, []byte{0x50, 0x4B, 0x03, 0x04}) {
		// Archive ZIP
		// Note: Pour une implémentation complète, utilisez archive/zip
		return fmt.Errorf("extraction ZIP non implémentée, utilisez tar.gz")
	} else {
		// Supposer tar simple
		return extractTar(tar.NewReader(bytes.NewReader(data)), destDir)
	}
}

// extractTar extrait une archive tar
func extractTar(tr *tar.Reader, destDir string) error {
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		target := filepath.Join(destDir, header.Name)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			dir := filepath.Dir(target)
			if err := os.MkdirAll(dir, 0755); err != nil {
				return err
			}

			f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR, os.FileMode(header.Mode))
			if err != nil {
				return err
			}

			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
	return nil
}

// execCmd exécute une commande shell
func execCmd(ctx context.Context, cmdStr string) error {
	// Dans une implémentation réelle, cette fonction utiliserait os/exec
	// Pour l'exemple, on simule un retour réussi
	return nil
}

// buildWithDocker construit une image avec l'API Docker
func (s *BuildService) buildWithDocker(ctx context.Context, buildDir, dockerfilePath string, spec *BuildSpec) (string, string, error) {
	var logBuffer bytes.Buffer

	// Créer le contexte de build en mémoire (tar)
	buildContextTar, err := archive.TarWithOptions(buildDir, &archive.TarOptions{})
	if err != nil {
		return "", "", fmt.Errorf("erreur lors de la création du contexte: %w", err)
	}
	defer buildContextTar.Close()

	// Préparer les options de build
	buildOptions := types.ImageBuildOptions{
		Dockerfile: filepath.Base(dockerfilePath),
		Tags:       spec.BuildConfig.Tags,
		Remove:     true,
		NoCache:    spec.BuildConfig.NoCache,
		BuildArgs:  make(map[string]*string),
	}

	// Ajouter les arguments de build
	for k, v := range spec.BuildConfig.Args {
		value := v // Copie locale pour éviter les problèmes de pointeur
		buildOptions.BuildArgs[k] = &value
	}

	// Ajouter le target si spécifié
	if spec.BuildConfig.Target != "" {
		buildOptions.Target = spec.BuildConfig.Target
	}

	// Exécuter le build
	buildResponse, err := s.dockerClient.ImageBuild(ctx, buildContextTar, buildOptions)
	if err != nil {
		return "", "", fmt.Errorf("erreur lors du build Docker: %w", err)
	}
	defer buildResponse.Body.Close()

	// Lire et traiter la sortie
	var imageID string
	decoder := json.NewDecoder(buildResponse.Body)
	for {
		var msg jsonmessage.JSONMessage
		if err := decoder.Decode(&msg); err != nil {
			if err == io.EOF {
				break
			}
			return "", logBuffer.String(), err
		}

		// Traiter les messages
		if msg.Error != nil {
			return "", logBuffer.String(), fmt.Errorf("%s", msg.Error.Message)
		}

		if msg.Stream != "" {
			fmt.Fprint(&logBuffer, msg.Stream)
			// Extraire l'ID de l'image
			if strings.Contains(msg.Stream, "Successfully built") {
				parts := strings.Fields(msg.Stream)
				for i, part := range parts {
					if part == "built" && i < len(parts)-1 {
						imageID = parts[i+1]
						break
					}
				}
			}
		}

		// Traiter les messages auxiliaires
		if msg.Aux != nil {
			var auxMsg struct {
				ID string `json:"ID"`
			}
			if err := json.Unmarshal(*msg.Aux, &auxMsg); err == nil && auxMsg.ID != "" {
				imageID = auxMsg.ID
			}
		}
	}

	return imageID, logBuffer.String(), nil
}

// getImageSize récupère la taille d'une image Docker
func (s *BuildService) getImageSize(ctx context.Context, imageID string) (int64, error) {
	summary, _, err := s.dockerClient.ImageInspectWithRaw(ctx, imageID)
	if err != nil {
		return 0, err
	}
	return summary.Size, nil
}

// exportAndUploadImage exporte une image Docker et l'upload vers B2
func (s *BuildService) exportAndUploadImage(ctx context.Context, imageID string, spec *BuildSpec) ([]string, error) {
	if s.b2Config == nil {
		return nil, fmt.Errorf("configuration B2 non définie")
	}

	// Créer un reader pour l'image exportée
	reader, err := s.dockerClient.ImageSave(ctx, []string{imageID})
	if err != nil {
		return nil, fmt.Errorf("erreur lors de l'export de l'image: %w", err)
	}
	defer reader.Close()

	// Lire l'image en mémoire
	imageData, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("erreur lors de la lecture de l'image: %w", err)
	}

	// Initialiser le client B2
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	b2Client, err := b2.NewClient(ctx, s.b2Config.AccountID, s.b2Config.ApplicationKey, b2.UserAgent("build-service"))
	if err != nil {
		return nil, fmt.Errorf("erreur lors de l'initialisation du client B2: %w", err)
	}

	// Accéder au bucket
	bucket, err := b2Client.Bucket(ctx, s.b2Config.BucketName)
	if err != nil {
		panic(err)
	}

	// Générer un nom d'objet basé sur l'ID de l'image et les tags
	imageName := fmt.Sprintf("%s-%s.tar", spec.Name, spec.Version)
	objectPath := filepath.Join(s.b2Config.BasePath, imageName)

	// Uploader l'image
	obj := bucket.Object(objectPath)
	writer := obj.NewWriter(ctx)

	if _, err := writer.Write(imageData); err != nil {
		writer.Close()
		return nil, fmt.Errorf("erreur lors de l'écriture vers B2: %w", err)
	}

	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("erreur lors de la finalisation de l'upload B2: %w", err)
	}

	// Pour chaque tag, exporter aussi une référence
	objectNames := []string{objectPath}
	for _, tag := range spec.BuildConfig.Tags {
		// Nettoyer le tag pour en faire un nom de fichier valide
		cleanTag := strings.ReplaceAll(tag, ":", "-")
		cleanTag = strings.ReplaceAll(cleanTag, "/", "-")

		tagPath := filepath.Join(s.b2Config.BasePath, cleanTag+".tar")

		// Créer un lien symbolique/référence dans B2
		// Note: B2 ne supporte pas directement les liens symboliques, on pourrait soit:
		// 1. Uploader à nouveau l'image (gaspillage d'espace)
		// 2. Créer un petit fichier de référence qui pointe vers l'image principale

		refObj := bucket.Object(tagPath)
		refWriter := refObj.NewWriter(ctx)

		refContent := fmt.Sprintf("Reference to: %s\nImage ID: %s\n", objectPath, imageID)
		if _, err := refWriter.Write([]byte(refContent)); err != nil {
			refWriter.Close()
			return objectNames, err // Retourner les objets déjà uploadés
		}

		if err := refWriter.Close(); err != nil {
			return objectNames, err
		}

		objectNames = append(objectNames, tagPath)
	}

	return objectNames, nil
}

// CreateContainer crée un conteneur à partir d'une image
func (s *BuildService) CreateContainer(ctx context.Context, imageID string, config *container.Config, hostConfig *container.HostConfig) (string, error) {
	if config == nil {
		config = &container.Config{
			Image: imageID,
		}
	} else if config.Image == "" {
		config.Image = imageID
	}

	resp, err := s.dockerClient.ContainerCreate(ctx, config, hostConfig, nil, nil, "")
	if err != nil {
		return "", err
	}

	return resp.ID, nil
}

// RunContainer démarre un conteneur et attend sa terminaison
func (s *BuildService) RunContainer(ctx context.Context, containerID string) (string, error) {
	if err := s.dockerClient.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return "", err
	}

	// Attendre que le conteneur se termine
	statusCh, errCh := s.dockerClient.ContainerWait(ctx, containerID, container.WaitConditionNotRunning)

	var logs bytes.Buffer

	// Récupérer les logs du conteneur
	logsOptions := container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	}

	logsReader, err := s.dockerClient.ContainerLogs(ctx, containerID, logsOptions)
	if err != nil {
		return "", err
	}
	defer logsReader.Close()

	// Copier les logs dans notre buffer
	_, err = stdcopy.StdCopy(&logs, &logs, logsReader)
	if err != nil {
		return logs.String(), err
	}

	// Attendre la fin du conteneur
	select {
	case err := <-errCh:
		return logs.String(), err
	case <-statusCh:
		return logs.String(), nil
	}
}

// ExportContainer exporte un conteneur vers un fichier tar
func (s *BuildService) ExportContainer(ctx context.Context, containerID string) ([]byte, error) {
	// Exporter le conteneur
	readCloser, err := s.dockerClient.ContainerExport(ctx, containerID)
	if err != nil {
		return nil, err
	}
	defer readCloser.Close()

	// Lire tous les données du conteneur
	return io.ReadAll(readCloser)
}

// Cleanup nettoie les ressources Docker (images, conteneurs)
func (s *BuildService) Cleanup(ctx context.Context, olderThan time.Duration) error {
	// Supprimer les conteneurs arrêtés
	containers, err := s.dockerClient.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return err
	}

	now := time.Now()
	for _, c := range containers {
		createdTime := time.Unix(c.Created, 0)
		if now.Sub(createdTime) > olderThan {
			// Supprimer le conteneur
			err := s.dockerClient.ContainerRemove(ctx, c.ID, container.RemoveOptions{
				Force: true,
			})
			if err != nil {
				// Log l'erreur mais continuer
				fmt.Printf("Erreur lors de la suppression du conteneur %s: %v\n", c.ID, err)
			}
		}
	}

	// Supprimer les images non utilisées
	pruneReport, err := s.dockerClient.ImagesPrune(ctx, filters.NewArgs(
		filters.Arg("until", fmt.Sprintf("%dm", int(olderThan.Minutes()))),
	))
	if err != nil {
		return err
	}

	fmt.Printf("Images nettoyées: %d, espace libéré: %d bytes\n",
		len(pruneReport.ImagesDeleted), pruneReport.SpaceReclaimed)

	return nil
}

// UploadArtifactToB2 upload un artefact vers B2
func (s *BuildService) UploadArtifactToB2(ctx context.Context, data []byte, objectPath string) (string, error) {
	if s.b2Config == nil {
		return "", fmt.Errorf("configuration B2 non définie")
	}

	// Initialiser le client B2
	b2Client, err := b2.NewClient(ctx, s.b2Config.AccountID, s.b2Config.ApplicationKey, b2.UserAgent("build-service"))
	if err != nil {
		return "", fmt.Errorf("erreur lors de l'initialisation du client B2: %w", err)
	}

	// Accéder au bucket
	bucket, err := b2Client.Bucket(ctx, s.b2Config.BucketName)
	if err != nil {
		panic(err)
	}

	// Préparer le chemin complet
	fullPath := filepath.Join(s.b2Config.BasePath, objectPath)

	// Uploader l'artefact
	obj := bucket.Object(fullPath)
	writer := obj.NewWriter(ctx)

	// Définir le type de contenu si possible
	// if ext := filepath.Ext(objectPath); ext != "" {
	// 	switch strings.ToLower(ext) {
	// 	case ".tar":
	// 		writer.SetContentType("application/x-tar")
	// 	case ".gz", ".tgz":
	// 		writer.SetContentType("application/gzip")
	// 	case ".zip":
	// 		writer.SetContentType("application/zip")
	// 	case ".json":
	// 		writer.SetContentType("application/json")
	// 	case ".txt":
	// 		writer.SetContentType("text/plain")
	// 	}
	// }

	if _, err := writer.Write(data); err != nil {
		writer.Close()
		return "", fmt.Errorf("erreur lors de l'écriture vers B2: %w", err)
	}

	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("erreur lors de la finalisation de l'upload B2: %w", err)
	}

	return fullPath, nil
}

// DownloadImageFromB2 télécharge une image Docker depuis B2
func (s *BuildService) DownloadImageFromB2(ctx context.Context, objectPath string) (string, error) {
	if s.b2Config == nil {
		return "", fmt.Errorf("configuration B2 non définie")
	}

	// Initialiser le client B2
	b2Client, err := b2.NewClient(ctx, s.b2Config.AccountID, s.b2Config.ApplicationKey, b2.UserAgent("build-service"))
	if err != nil {
		return "", fmt.Errorf("erreur lors de l'initialisation du client B2: %w", err)
	}

	// Accéder au bucket
	bucket, err := b2Client.Bucket(ctx, s.b2Config.BucketName)
	if err != nil {
		panic(err)
	}

	// Préparer le chemin complet
	fullPath := objectPath
	if !strings.HasPrefix(objectPath, s.b2Config.BasePath) {
		fullPath = filepath.Join(s.b2Config.BasePath, objectPath)
	}

	// Télécharger l'objet
	obj := bucket.Object(fullPath)
	reader := obj.NewReader(ctx)
	defer reader.Close()

	// Lire l'objet en mémoire
	imageData, err := io.ReadAll(reader)
	if err != nil {
		return "", fmt.Errorf("erreur lors de la lecture depuis B2: %w", err)
	}

	// Charger l'image dans Docker
	loadResp, err := s.dockerClient.ImageLoad(ctx, bytes.NewReader(imageData))
	if err != nil {
		return "", fmt.Errorf("erreur lors du chargement de l'image: %w", err)
	}
	defer loadResp.Body.Close()

	// Lire la réponse pour obtenir l'ID de l'image
	var loadOutput struct {
		Stream string `json:"stream"`
	}

	decoder := json.NewDecoder(loadResp.Body)
	imageID := ""

	for {
		if err := decoder.Decode(&loadOutput); err != nil {
			if err == io.EOF {
				break
			}
			return "", fmt.Errorf("erreur lors de la lecture de la réponse: %w", err)
		}

		// Extraire l'ID de l'image si présent
		if strings.Contains(loadOutput.Stream, "Loaded image") {
			parts := strings.Fields(loadOutput.Stream)
			if len(parts) > 2 {
				imageID = parts[len(parts)-1]
				imageID = strings.TrimSpace(imageID)
				break
			}
		}
	}

	// Si on n'a pas trouvé d'ID explicite, lister les images pour trouver la plus récente
	if imageID == "" {
		images, err := s.dockerClient.ImageList(ctx, image.ListOptions{})
		if err != nil {
			return "", fmt.Errorf("erreur lors de la liste des images: %w", err)
		}

		if len(images) > 0 {
			// Prendre la plus récente (la première de la liste)
			imageID = images[0].ID
		}
	}

	return imageID, nil
}

// BuildWithMultipleCodebases construit une image Docker à partir de plusieurs codebases
func (s *BuildService) BuildWithMultipleCodebases(ctx context.Context, spec *BuildSpec) (*BuildResult, error) {
	// Validation des paramètres
	if len(spec.Codebases) < 2 {
		return nil, fmt.Errorf("au moins deux codebases sont nécessaires pour cette opération")
	}

	return s.Build(ctx, spec)
}

// SaveDockerImageToBuffer exporte une image Docker vers un buffer en mémoire
func (s *BuildService) SaveDockerImageToBuffer(ctx context.Context, imageID string) ([]byte, error) {
	// Exporter l'image
	reader, err := s.dockerClient.ImageSave(ctx, []string{imageID})
	if err != nil {
		return nil, fmt.Errorf("erreur lors de l'export de l'image: %w", err)
	}
	defer reader.Close()

	// Lire les données en mémoire
	var buf bytes.Buffer
	_, err = io.Copy(&buf, reader)
	if err != nil {
		return nil, fmt.Errorf("erreur lors de la lecture des données d'image: %w", err)
	}

	return buf.Bytes(), nil
}

// CreateDockerfileFromMultipleCodebases crée un Dockerfile optimisé pour combiner plusieurs bases de code
func (s *BuildService) CreateDockerfileFromMultipleCodebases(spec *BuildSpec) (string, error) {
	if len(spec.Codebases) < 1 {
		return "", fmt.Errorf("au moins une codebase est nécessaire")
	}

	var dockerfile strings.Builder

	// Si un Dockerfile personnalisé est fourni, l'utiliser tel quel
	if spec.BuildConfig.Dockerfile != "" {
		return spec.BuildConfig.Dockerfile, nil
	}

	// Si aucun Dockerfile personnalisé n'est fourni, en créer un qui combine les codebases
	// Utiliser l'image de base spécifiée ou une image par défaut
	baseImage := spec.BuildConfig.BaseImage
	if baseImage == "" {
		baseImage = "alpine:latest" // Image par défaut
	}

	dockerfile.WriteString(fmt.Sprintf("FROM %s\n\n", baseImage))
	dockerfile.WriteString("WORKDIR /app\n\n")

	// Ajouter chaque codebase
	for _, codebase := range spec.Codebases {
		dockerfile.WriteString(fmt.Sprintf("# Ajout de la codebase %s\n", codebase.Name))
		dockerfile.WriteString(fmt.Sprintf("COPY %s/ /app/%s/\n\n", codebase.Name, codebase.Name))
	}

	// Exposer le port 8080 par défaut
	dockerfile.WriteString("EXPOSE 8080\n\n")

	// Commande par défaut
	dockerfile.WriteString("CMD [\"sh\", \"-c\", \"echo 'Image construite avec succès à partir de multiples codebases'\"]\n")

	return dockerfile.String(), nil
}

// BuildWithBufferInput construit une image Docker à partir de données en mémoire
func (s *BuildService) BuildWithBufferInput(ctx context.Context, dockerfile string, contextData map[string][]byte, tags []string) (*BuildResult, error) {
	tmpDir, err := ioutil.TempDir("", "docker-build-buffer-")
	if err != nil {
		return nil, fmt.Errorf("impossible de créer un répertoire temporaire: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Créer le Dockerfile
	dockerfilePath := filepath.Join(tmpDir, "Dockerfile")
	if err := ioutil.WriteFile(dockerfilePath, []byte(dockerfile), 0644); err != nil {
		return nil, fmt.Errorf("erreur lors de la création du Dockerfile: %w", err)
	}

	// Créer les fichiers de contexte
	for filePath, content := range contextData {
		fullPath := filepath.Join(tmpDir, filePath)

		// Créer les répertoires parents
		dirPath := filepath.Dir(fullPath)
		if err := os.MkdirAll(dirPath, 0755); err != nil {
			return nil, fmt.Errorf("erreur lors de la création du répertoire %s: %w", dirPath, err)
		}

		// Écrire le fichier
		if err := ioutil.WriteFile(fullPath, content, 0644); err != nil {
			return nil, fmt.Errorf("erreur lors de l'écriture du fichier %s: %w", filePath, err)
		}
	}

	// Créer une spécification de build minimale
	spec := &BuildSpec{
		Name:    "buffer-build",
		Version: time.Now().Format("20060102-150405"),
		BuildConfig: BuildConfig{
			Tags: tags,
		},
	}

	// Construire l'image
	imageID, logs, err := s.buildWithDocker(ctx, tmpDir, dockerfilePath, spec)
	if err != nil {
		return &BuildResult{
			Success:      false,
			Logs:         logs,
			ErrorMessage: err.Error(),
		}, err
	}

	// Récupérer la taille de l'image
	imageSize, _ := s.getImageSize(ctx, imageID)

	return &BuildResult{
		Success:   true,
		ImageID:   imageID,
		ImageSize: imageSize,
		Logs:      logs,
	}, nil
}

// GetImageInfo récupère des informations détaillées sur une image Docker
func (s *BuildService) GetImageInfo(ctx context.Context, imageID string) (*types.ImageInspect, error) {
	inspect, _, err := s.dockerClient.ImageInspectWithRaw(ctx, imageID)
	if err != nil {
		return nil, err
	}
	return &inspect, nil
}

// TagImage ajoute un tag à une image Docker
func (s *BuildService) TagImage(ctx context.Context, imageID string, tag string) error {
	return s.dockerClient.ImageTag(ctx, imageID, tag)
}

// PushImage pousse une image vers un registry Docker
func (s *BuildService) PushImage(ctx context.Context, tag string, auth string) (string, error) {
	var authConfig interface{}

	// Si auth est fourni, le décoder
	if auth != "" {
		authJSON, err := base64.StdEncoding.DecodeString(auth)
		if err != nil {
			return "", fmt.Errorf("erreur lors du décodage de l'authentification: %w", err)
		}

		if err := json.Unmarshal(authJSON, &authConfig); err != nil {
			return "", fmt.Errorf("erreur lors du parsing de l'authentification: %w", err)
		}
	}

	// Encoder l'authentification pour l'API Docker
	encodedAuth, err := json.Marshal(authConfig)
	if err != nil {
		return "", fmt.Errorf("erreur lors de l'encodage de l'authentification: %w", err)
	}
	authStr := base64.URLEncoding.EncodeToString(encodedAuth)

	// Options de push
	options := image.PushOptions{
		RegistryAuth: authStr,
	}

	// Push l'image
	pushResp, err := s.dockerClient.ImagePush(ctx, tag, options)
	if err != nil {
		return "", fmt.Errorf("erreur lors du push de l'image: %w", err)
	}
	defer pushResp.Close()

	// Lire les logs du push
	var logs bytes.Buffer
	decoder := json.NewDecoder(pushResp)

	for {
		var msg jsonmessage.JSONMessage
		if err := decoder.Decode(&msg); err != nil {
			if err == io.EOF {
				break
			}
			return logs.String(), fmt.Errorf("erreur lors de la lecture des logs: %w", err)
		}

		if msg.Error != nil {
			return logs.String(), fmt.Errorf("erreur dans le push: %s", msg.Error.Message)
		}

		if msg.Status != "" {
			fmt.Fprintf(&logs, "%s: %s\n", time.Now().Format("15:04:05"), msg.Status)
		}
		if msg.Progress != nil {
			fmt.Fprintf(&logs, "%s\n", msg.Progress.String())
		}
	}

	return logs.String(), nil
}

// CompressAndUploadArtifacts compresse les artefacts et les upload vers B2
func (s *BuildService) CompressAndUploadArtifacts(ctx context.Context, artifacts map[string][]byte, name string) (string, error) {
	if len(artifacts) == 0 {
		return "", fmt.Errorf("aucun artefact à compresser")
	}

	// Créer une archive tar.gz en mémoire
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	// Ajouter chaque artefact
	for path, data := range artifacts {
		header := &tar.Header{
			Name: path,
			Mode: 0644,
			Size: int64(len(data)),
		}

		if err := tw.WriteHeader(header); err != nil {
			return "", fmt.Errorf("erreur lors de l'écriture de l'en-tête tar: %w", err)
		}

		if _, err := tw.Write(data); err != nil {
			return "", fmt.Errorf("erreur lors de l'écriture des données tar: %w", err)
		}
	}

	// Fermer les writers
	if err := tw.Close(); err != nil {
		return "", err
	}
	if err := gzw.Close(); err != nil {
		return "", err
	}

	// Générer un nom pour l'archive
	archiveName := fmt.Sprintf("%s-artifacts-%s.tar.gz", name, time.Now().Format("20060102-150405"))

	// Upload vers B2
	return s.UploadArtifactToB2(ctx, buf.Bytes(), archiveName)
}

// ExecuteInContainer exécute une commande dans un conteneur temporaire basé sur une image
func (s *BuildService) ExecuteInContainer(ctx context.Context, imageID string, cmd []string, env []string) (string, error) {
	// Configuration du conteneur
	config := &container.Config{
		Image:        imageID,
		Cmd:          cmd,
		Env:          env,
		Tty:          false,
		AttachStdout: true,
		AttachStderr: true,
	}

	// Créer le conteneur
	resp, err := s.dockerClient.ContainerCreate(ctx, config, nil, nil, nil, "")
	if err != nil {
		return "", fmt.Errorf("erreur lors de la création du conteneur: %w", err)
	}
	containerID := resp.ID

	// Nettoyer le conteneur à la fin
	defer s.dockerClient.ContainerRemove(ctx, containerID, container.RemoveOptions{
		Force: true,
	})

	// Démarrer le conteneur
	if err := s.dockerClient.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("erreur lors du démarrage du conteneur: %w", err)
	}

	// Attendre que le conteneur termine
	statusCh, errCh := s.dockerClient.ContainerWait(ctx, containerID, container.WaitConditionNotRunning)

	var status container.WaitResponse
	select {
	case err := <-errCh:
		return "", fmt.Errorf("erreur lors de l'attente du conteneur: %w", err)
	case status = <-statusCh:
		// Continuer
	}

	// Récupérer les logs
	out, err := s.dockerClient.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
	})
	if err != nil {
		return "", fmt.Errorf("erreur lors de la récupération des logs: %w", err)
	}
	defer out.Close()

	// Lire les logs
	var output bytes.Buffer
	_, err = stdcopy.StdCopy(&output, &output, out)
	if err != nil {
		return "", fmt.Errorf("erreur lors de la lecture des logs: %w", err)
	}

	// Vérifier le code de sortie
	if status.StatusCode != 0 {
		return output.String(), fmt.Errorf("commande terminée avec code de sortie %d", status.StatusCode)
	}

	return output.String(), nil
}

// GenerateMultistageDockerfile génère un Dockerfile multistage pour combiner plusieurs codebases
func (s *BuildService) GenerateMultistageDockerfile(spec *BuildSpec) (string, error) {
	if len(spec.Codebases) == 0 {
		return "", fmt.Errorf("aucune codebase spécifiée")
	}

	var dockerfile strings.Builder

	// Détecter les types de projets dans chaque codebase
	// Cette fonction simplifiée détecte Go, Node.js, Python, et Java
	projectTypes := make(map[string]string)
	for _, codebase := range spec.Codebases {
		// Dans une implémentation réelle, cette détection serait plus sophistiquée
		if codebase.Path != "" && codebase.Path != "." {
			projectTypes[codebase.Name] = "unknown" // Par défaut
		} else {
			projectTypes[codebase.Name] = "unknown" // Par défaut
		}
	}

	// Générer les étapes de build pour chaque codebase
	for _, codebase := range spec.Codebases {
		dockerfile.WriteString(fmt.Sprintf("# Étape de build pour %s\n", codebase.Name))

		// Déterminer l'image de base selon le type de projet
		switch projectTypes[codebase.Name] {
		case "go":
			dockerfile.WriteString(fmt.Sprintf("FROM golang:1.19-alpine AS %s-builder\n", codebase.Name))
			dockerfile.WriteString(fmt.Sprintf("WORKDIR /build/%s\n", codebase.Name))
			dockerfile.WriteString(fmt.Sprintf("COPY %s/ .\n", codebase.Name))
			dockerfile.WriteString("RUN go build -o app .\n\n")
		case "nodejs":
			dockerfile.WriteString(fmt.Sprintf("FROM node:18-alpine AS %s-builder\n", codebase.Name))
			dockerfile.WriteString(fmt.Sprintf("WORKDIR /build/%s\n", codebase.Name))
			dockerfile.WriteString(fmt.Sprintf("COPY %s/ .\n", codebase.Name))
			dockerfile.WriteString("RUN npm ci && npm run build\n\n")
		case "python":
			dockerfile.WriteString(fmt.Sprintf("FROM python:3.10-slim AS %s-builder\n", codebase.Name))
			dockerfile.WriteString(fmt.Sprintf("WORKDIR /build/%s\n", codebase.Name))
			dockerfile.WriteString(fmt.Sprintf("COPY %s/ .\n", codebase.Name))
			dockerfile.WriteString("RUN pip install -r requirements.txt && python setup.py build\n\n")
		case "java":
			dockerfile.WriteString(fmt.Sprintf("FROM maven:3.8-openjdk-17 AS %s-builder\n", codebase.Name))
			dockerfile.WriteString(fmt.Sprintf("WORKDIR /build/%s\n", codebase.Name))
			dockerfile.WriteString(fmt.Sprintf("COPY %s/ .\n", codebase.Name))
			dockerfile.WriteString("RUN mvn package -DskipTests\n\n")
		default:
			// Base générique pour les projets inconnus
			dockerfile.WriteString(fmt.Sprintf("FROM alpine:latest AS %s-builder\n", codebase.Name))
			dockerfile.WriteString(fmt.Sprintf("WORKDIR /build/%s\n", codebase.Name))
			dockerfile.WriteString(fmt.Sprintf("COPY %s/ .\n\n", codebase.Name))
		}
	}

	// Image finale
	baseImage := spec.BuildConfig.BaseImage
	if baseImage == "" {
		baseImage = "alpine:latest"
	}

	dockerfile.WriteString("# Image finale\n")
	dockerfile.WriteString(fmt.Sprintf("FROM %s\n", baseImage))
	dockerfile.WriteString("WORKDIR /app\n\n")

	// Copier les artefacts de chaque étape de build
	for _, codebase := range spec.Codebases {
		switch projectTypes[codebase.Name] {
		case "go":
			dockerfile.WriteString(fmt.Sprintf("COPY --from=%s-builder /build/%s/app /app/%s/\n",
				codebase.Name, codebase.Name, codebase.Name))
		case "nodejs":
			dockerfile.WriteString(fmt.Sprintf("COPY --from=%s-builder /build/%s/dist /app/%s/\n",
				codebase.Name, codebase.Name, codebase.Name))
		case "python":
			dockerfile.WriteString(fmt.Sprintf("COPY --from=%s-builder /build/%s/build /app/%s/\n",
				codebase.Name, codebase.Name, codebase.Name))
		case "java":
			dockerfile.WriteString(fmt.Sprintf("COPY --from=%s-builder /build/%s/target/*.jar /app/%s/\n",
				codebase.Name, codebase.Name, codebase.Name))
		default:
			dockerfile.WriteString(fmt.Sprintf("COPY --from=%s-builder /build/%s/ /app/%s/\n",
				codebase.Name, codebase.Name, codebase.Name))
		}
	}

	// Exposer le port 8080 par défaut
	dockerfile.WriteString("\nEXPOSE 8080\n\n")

	// Commande par défaut
	dockerfile.WriteString("CMD [\"sh\", \"-c\", \"echo 'Image multi-stage construite avec succès'\"]\n")

	return dockerfile.String(), nil
}

// SetupDockerIgnore crée un fichier .dockerignore pour exclure les fichiers inutiles
func (s *BuildService) SetupDockerIgnore(buildDir string) error {
	dockerignore := `# Fichiers de système
.DS_Store
Thumbs.db

# Répertoires de dépendances
**/node_modules
**/vendor
**/.venv
**/env
**/venv
**/__pycache__

# Fichiers de build
**/dist
**/build
**/out
**/bin
**/target
**/.next
**/.nuxt

# Fichiers de développement
**/.git
**/.github
**/.vscode
**/.idea
**/*.log
**/*.swp
**/*.swo
**/coverage
**/tmp

# Fichiers de configuration Docker
Dockerfile*
docker-compose*
.dockerignore

# Fichiers de test
**/*_test.go
**/test
**/tests
**/*.spec.js
**/*.test.js
`

	return os.WriteFile(filepath.Join(buildDir, ".dockerignore"), []byte(dockerignore), 0644)
}

// GenerateImageReport crée un rapport détaillé sur une image Docker
func (s *BuildService) GenerateImageReport(ctx context.Context, imageID string) (map[string]interface{}, error) {
	report := make(map[string]interface{})

	// Informations générales sur l'image
	inspect, _, err := s.dockerClient.ImageInspectWithRaw(ctx, imageID)
	if err != nil {
		return nil, fmt.Errorf("erreur lors de l'inspection de l'image: %w", err)
	}

	report["image_id"] = inspect.ID
	report["created"] = inspect.Created
	report["size"] = inspect.Size
	report["virtual_size"] = inspect.VirtualSize
	report["tags"] = inspect.RepoTags
	report["labels"] = inspect.Config.Labels
	report["entrypoint"] = inspect.Config.Entrypoint
	report["cmd"] = inspect.Config.Cmd
	report["env"] = inspect.Config.Env
	report["exposed_ports"] = inspect.Config.ExposedPorts
	report["os"] = inspect.Os
	report["architecture"] = inspect.Architecture

	// Récupérer l'historique de l'image
	history, err := s.dockerClient.ImageHistory(ctx, imageID)
	if err != nil {
		return report, fmt.Errorf("erreur lors de la récupération de l'historique: %w", err)
	}

	historyInfo := make([]map[string]interface{}, 0, len(history))
	for _, layer := range history {
		historyInfo = append(historyInfo, map[string]interface{}{
			"created":    layer.Created,
			"created_by": layer.CreatedBy,
			"size":       layer.Size,
			"comment":    layer.Comment,
			"tags":       layer.Tags,
		})
	}
	report["history"] = historyInfo

	return report, nil
}
