package build

import (
	// ... autres imports ...
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log" // Pour les logs internes
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	// Importer le package socket (ajuster le chemin si nécessaire)
	"github.com/Treefle-labs/Anexis/socket"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/pkg/jsonmessage" // Nécessaire pour le writer de logs
	"github.com/moby/go-archive"
	// ...
)

// Assurer que BuildService implémente les interfaces requises par le serveur socket
var _ socket.BuildTriggerer = (*BuildService)(nil)
var _ socket.SecretFetcher = (*BuildService)(nil)

// --- Implémentation de socket.SecretFetcher ---

// GetSecret récupère un secret en utilisant le fetcher configuré dans BuildService.
// Ceci suppose que vous avez déjà un moyen de récupérer les secrets DANS BuildService.
// Si ce n'est pas le cas, vous devrez adapter cette partie.


// --- Implémentation de socket.BuildTriggerer ---

// logNotifierWriter est un io.Writer qui envoie les données écrites au BuildNotifier.
type logNotifierWriter struct {
	buildID  string
	stream   string // "stdout" or "stderr"
	notifier socket.BuildNotifier
	mu       sync.Mutex // Protéger les appels concurrents potentiels à Write
}

func newLogNotifierWriter(buildID string, stream string, notifier socket.BuildNotifier) *logNotifierWriter {
	return &logNotifierWriter{
		buildID:  buildID,
		stream:   stream,
		notifier: notifier,
	}
}

func (lnw *logNotifierWriter) Write(p []byte) (n int, err error) {
	if lnw.notifier == nil {
		return len(p), nil // Ne rien faire si pas de notifier
	}
	lnw.mu.Lock()
	defer lnw.mu.Unlock()
	// Envoyer le contenu comme un chunk de log
	// Convertir les bytes en string. Peut être optimisé si de très gros chunks sont attendus.
	content := string(p)
	lnw.notifier.NotifyLog(lnw.buildID, lnw.stream, content)
	return len(p), nil
}


// StartBuildAsync lance un build en arrière-plan et notifie via le notifier.
func (s *BuildService) StartBuildAsync(ctx context.Context, buildID string, buildSpecYAML string, notifier socket.BuildNotifier) error {
	log.Printf("[BuildID: %s] Received async build request.\n", buildID)

	// 1. Parser le BuildSpec depuis le YAML reçu
	// Utiliser le format .yaml par défaut car c'est ce qu'on a défini dans le payload
	spec, err := LoadBuildSpecFromBytes([]byte(buildSpecYAML), ".yaml")
	if err != nil {
		log.Printf("[BuildID: %s] Error parsing BuildSpec YAML: %v\n", buildID, err)
		// Notifier immédiatement l'échec de démarrage
		go notifier.NotifyStatus(buildID, "failure", "", fmt.Errorf("invalid build spec: %w", err), nil)
		return fmt.Errorf("invalid build spec: %w", err) // Retourner l'erreur au serveur socket
	}
	log.Printf("[BuildID: %s] Parsed BuildSpec for '%s' version '%s'.\n", buildID, spec.Name, spec.Version)

	// 2. Lancer la logique de build réelle dans une goroutine
	go s.runBuildLogic(ctx, buildID, spec, notifier)

	// 3. Retourner nil immédiatement pour indiquer que la tâche a été acceptée
	log.Printf("[BuildID: %s] Build logic started in background.\n", buildID)
	return nil
}


// runBuildLogic contient la logique de build principale, adaptée pour les notifications.
// ATTENTION: Cette fonction est maintenant longue et complexe. Envisager de la découper.
func (s *BuildService) runBuildLogic(ctx context.Context, buildID string, spec *BuildSpec, notifier socket.BuildNotifier) {
	startTime := time.Now()
	var buildErr error
	var finalStatus string = "success" // Statut par défaut
	var artifactRef string = ""        // Référence de l'artefact final

	// Créer des writers pour capturer stdout/stderr et les envoyer au notifier
	stdoutNotifier := newLogNotifierWriter(buildID, "stdout", notifier)
	// stderrNotifier := newLogNotifierWriter(buildID, "stderr", notifier) // Peut être utile plus tard

	// Créer un logger dédié pour ce build qui écrit vers le notifier
	buildLogger := log.New(stdoutNotifier, fmt.Sprintf("[%s] ", buildID), 0) // Pas de flags de date/heure par défaut

	// S'assurer que le statut final est envoyé même en cas de panic
	defer func() {
		duration := time.Since(startTime).Seconds()
		if r := recover(); r != nil {
			buildLogger.Printf("PANIC recovered during build: %v\n", r)
			buildErr = fmt.Errorf("panic during build: %v", r)
			finalStatus = "failure"
		}
		buildLogger.Printf("Build finished with status: %s (Error: %v)\n", finalStatus, buildErr)
		notifier.NotifyStatus(buildID, finalStatus, artifactRef, buildErr, &duration)
	}()


	// --- Logique de Build (adaptée de Build()) ---
	buildLogger.Println("Starting build process...")
	notifier.NotifyStatus(buildID, "starting", "", nil, nil) // Statut initial

	// Utiliser un lock spécifique au build si BuildService a des champs partagés modifiables (ici, juste pour l'exemple)
	// s.mutex.Lock()
	// defer s.mutex.Unlock() // Attention à la durée du lock

	result := &BuildResult{ // Utiliser un result local pour stocker les infos internes
		Artifacts:       make(map[string][]byte),
		ImageIDs:        make(map[string]string),
		ImageSizes:      make(map[string]int64),
		LocalImagePaths: make(map[string]string),
		ServiceOutputs:  make(map[string]ServiceOutput),
	}

	// --- 1. Setup Build Environment ---
	// Utiliser buildID pour un chemin unique
	buildDir := filepath.Join(s.workDir, buildID)
	if err := os.MkdirAll(buildDir, 0755); err != nil {
		buildErr = fmt.Errorf("cannot create build directory '%s': %w", buildDir, err)
		finalStatus = "failure"
		return // Sortir après avoir mis à jour buildErr (defer s'occupera de notifier)
	}
	// Nettoyer seulement si succès et pas sortie locale SANS chemin spécifique
	shouldCleanup := true
	defer func() {
		if shouldCleanup && buildErr == nil { // Nettoyer si succès
			if !(spec.BuildConfig.OutputTarget == "local" && spec.BuildConfig.LocalPath == "") {
				buildLogger.Printf("Cleaning up build directory: %s\n", buildDir)
				os.RemoveAll(buildDir)
			} else {
				buildLogger.Printf("Keeping build directory for local output: %s\n", buildDir)
			}
		} else if buildErr != nil {
			buildLogger.Printf("Keeping build directory due to error: %s\n", buildDir)
		}
	}()
	buildLogger.Printf("Using build directory: %s\n", buildDir)
	notifier.NotifyStatus(buildID, "preparing_env", "", nil, nil)

	// --- 2. Load Environment Variables ---
	mergedEnv := make(map[string]string)
	// Copier/Adapter la logique de chargement des EnvFiles et Env ici...
	// Utiliser buildLogger.Printf pour les warnings/infos
	buildLogger.Printf("Loading environment variables...\n")
	// ... (logique de chargement de godotenv, etc.) ...
	for k, v := range spec.Env { // Exemple simplifié
		mergedEnv[k] = v
	}
	buildLogger.Printf("Loaded %d environment variables.\n", len(mergedEnv))


	// --- 3. Fetch Secrets ---
	runtimeSecrets := make(map[string]string)
	if s.secretFetcher != nil && len(spec.Secrets) > 0 {
		buildLogger.Println("Fetching secrets...")
		notifier.NotifyStatus(buildID, "fetching_secrets", "", nil, nil)
		for _, secretSpec := range spec.Secrets {
			secretValue, err := s.GetSecret(ctx, secretSpec.Source) // Utilise la méthode locale
			if err != nil {
				buildErr = fmt.Errorf("failed to fetch secret '%s' (source: %s): %w", secretSpec.Name, secretSpec.Source, err)
				finalStatus = "failure"
				return
			}
			runtimeSecrets[secretSpec.Name] = secretValue
			// Ne pas logger la valeur du secret !
			buildLogger.Printf("Secret '%s' fetched successfully.\n", secretSpec.Name)
		}
	}
	finalRuntimeEnv := make(map[string]string)
	for k, v := range mergedEnv { finalRuntimeEnv[k] = v }
	for k, v := range runtimeSecrets { finalRuntimeEnv[k] = v }


	// --- 4. Download Resources ---
	// Adapter la logique de téléchargement ici... Utiliser buildLogger.
	notifier.NotifyStatus(buildID, "downloading_resources", "", nil, nil)
	buildLogger.Println("Downloading resources...")
	// ... (boucle sur spec.Resources, appel s.downloadFile, s.extractArchive...) ...
	// En cas d'erreur, assigner buildErr et retourner


	// --- 5. Prepare Codebases ---
	notifier.NotifyStatus(buildID, "fetching_codebases", "", nil, nil)
	buildLogger.Println("Fetching codebases...")
	codebaseMap := make(map[string]CodebaseConfig)
	for _, codebase := range spec.Codebases {
		// ... (logique pour déterminer destDir) ...
		destDir := filepath.Join(buildDir, codebase.Name) // Simplifié
		buildLogger.Printf("Fetching codebase '%s' into %s\n", codebase.Name, destDir)
		if err := s.fetchCodebase(ctx, codebase, destDir); err != nil {
			buildErr = fmt.Errorf("failed to fetch codebase '%s': %w", codebase.Name, err)
			finalStatus = "failure"
			return
		}
		codebaseMap[codebase.Name] = codebase
	}

	// --- 6. Execute Build Steps (si implémenté) ---
	// Adapter la logique des BuildSteps ici... Utiliser buildLogger.
	// ...


	// --- 7. Main Build Execution ---
	notifier.NotifyStatus(buildID, "building_image", "", nil, nil)
	buildLogger.Println("Starting main build execution...")
	// Ici, on doit passer le `stdoutNotifier` aux fonctions de build Docker

	if spec.BuildConfig.ComposeFile != "" {
		// --- 7a. Build using Docker Compose ---
		buildLogger.Printf("Building using Compose file: %s\n", spec.BuildConfig.ComposeFile)
		// ... (charger le projet compose comme avant, mais passer stdoutNotifier aux appels build) ...
		// buildErrs := s.buildComposeProject(ctx, buildDir, composeProject, spec, result, buildLogger) // Adapter buildComposeProject
		buildErr = fmt.Errorf("compose build via socket not fully adapted yet") // Placeholder
		finalStatus = "failure"
		return
	} else {
		// --- 7b. Build using Dockerfile ---
		dockerfilePath, buildContextDir, err := s.findDockerfile(buildDir, spec)
		if err != nil {
			buildErr = err
			finalStatus = "failure"
			return
		}
		buildLogger.Printf("Building with Dockerfile: %s (Context: %s)\n", dockerfilePath, buildContextDir)

		// *** Modifier buildSingleImage pour accepter un io.Writer pour les logs ***
		imageID, err := s.buildSingleImageWithLogs(ctx, buildContextDir, dockerfilePath, spec, stdoutNotifier) // Nouvelle fonction
		if err != nil {
			buildErr = fmt.Errorf("docker build failed: %w", err)
			finalStatus = "failure"
			return
		}

		// Stocker le résultat
		result.ImageID = imageID
		imageSize, _ := s.getImageSize(ctx, imageID) // Ignorer l'erreur de taille pour l'instant
		result.ImageSize = imageSize
		mainServiceName := spec.Name
		result.ImageIDs[mainServiceName] = imageID
		result.ImageSizes[mainServiceName] = imageSize
		result.ServiceOutputs[mainServiceName] = ServiceOutput{ImageID: imageID, ImageSize: imageSize}
		buildLogger.Printf("Dockerfile build successful. ImageID: %s\n", imageID)
	}


	// --- 8. Handle Build Outputs ---
	notifier.NotifyStatus(buildID, "saving_artifacts", "", nil, nil)
	buildLogger.Println("Handling build outputs...")
	// ... (logique de tagging d'image comme avant) ...
	finalImageTags := make(map[string][]string) // Recréer cette map pour le run.yml
	// ... (appliquer les tags) ...

	// Adapter la logique de OutputTarget
	outputBasePath := buildDir // Base par défaut
	if spec.BuildConfig.OutputTarget == "local" && spec.BuildConfig.LocalPath != "" {
		outputBasePath = spec.BuildConfig.LocalPath // Logique inchangée
		os.MkdirAll(outputBasePath, 0755) // Créer si besoin
	}

	buildLogger.Printf("Output target: %s\n", spec.BuildConfig.OutputTarget)
	switch spec.BuildConfig.OutputTarget {
	case "b2":
		// ... (logique exportAndUploadImage) ...
		// artifactRef = ... (chemin B2 principal)
		artifactRef = "b2://not/implemented/yet" // Placeholder
	case "local":
		for serviceName, serviceOutput := range result.ServiceOutputs {
			imageFileName := fmt.Sprintf("%s_%s.tar", spec.Name, serviceName)
			localImagePath := filepath.Join(outputBasePath, imageFileName)
			buildLogger.Printf("Saving image for service '%s' locally to %s...\n", serviceName, localImagePath)
			err := s.saveImageLocally(ctx, serviceOutput.ImageID, localImagePath)
			if err != nil {
				buildErr = fmt.Errorf("failed to save image '%s' locally: %w", serviceName, err)
				finalStatus = "failure"
				return
			}
			result.LocalImagePaths[serviceName] = localImagePath
			if serviceName == spec.Name { // Assigner la ref de l'artefact principal
				artifactRef = localImagePath // Chemin absolu ici
			}
		}
		// Si sortie locale sans chemin spécifique, ne pas nettoyer le buildDir
		if spec.BuildConfig.LocalPath == "" {
			shouldCleanup = false
		}

	case "docker":
	default:
		// Les images sont dans le daemon, utiliser le tag comme référence
		tags := finalImageTags[spec.Name]
		if len(tags) > 0 {
			artifactRef = tags[0] // Premier tag
		} else {
			artifactRef = result.ImageID // Fallback sur l'ID
		}
		buildLogger.Printf("Images available in local Docker daemon. Artifact ref: %s\n", artifactRef)

	}
	if buildErr != nil { return } // Vérifier après la gestion des sorties


	// --- 9. Generate *.run.yml (si demandé) ---
	if spec.RunConfigDef.Generate {
		buildLogger.Println("Generating *.run.yml file...")
		// ... (Logique de generateRunYAML comme avant, mais charger le projet compose ici si nécessaire) ...
		// Le chemin de sortie doit être dans outputBasePath
		runConfigPath := filepath.Join(outputBasePath, fmt.Sprintf("%s-%s.run.yml", spec.Name, spec.Version))
		// ... (générer et écrire le fichier) ...
		// Si succès, on pourrait ajouter le chemin run.yml à l'artifactRef ou un message de statut ?
	}

	buildLogger.Println("Build process completed successfully.")
	// Le defer s'occupera d'envoyer le statut final "success"
}


// findDockerfile (helper extrait de Build)
func (s *BuildService) findDockerfile(buildDir string, spec *BuildSpec) (dockerfilePath, buildContextDir string, err error) {
	buildContextDir = buildDir // Default

	if spec.BuildConfig.Dockerfile != "" {
		if strings.Contains(spec.BuildConfig.Dockerfile, "\n") {
			dockerfilePath = filepath.Join(buildDir, "Dockerfile.inline")
			if err = os.WriteFile(dockerfilePath, []byte(spec.BuildConfig.Dockerfile), 0644); err != nil {
				err = fmt.Errorf("failed to write inline Dockerfile: %w", err)
				return
			}
		} else {
			dockerfilePath = filepath.Join(buildDir, spec.BuildConfig.Dockerfile)
			buildContextDir = filepath.Dir(dockerfilePath) // Ajuster le contexte
		}
	} else {
		// Auto-detect
		dfPath := filepath.Join(buildDir, "Dockerfile")
		if _, statErr := os.Stat(dfPath); statErr == nil {
			dockerfilePath = dfPath
		} else if len(spec.Codebases) > 0 { // Fallback sur la première codebase
			firstCodebaseDir := filepath.Join(buildDir, spec.Codebases[0].Name)
			dfPath = filepath.Join(firstCodebaseDir, "Dockerfile")
			if _, statErr := os.Stat(dfPath); statErr == nil {
				dockerfilePath = dfPath
				buildContextDir = firstCodebaseDir
			}
		}
	}

	if dockerfilePath == "" {
		err = fmt.Errorf("no Dockerfile specified or found")
		return
	}
	if _, statErr := os.Stat(dockerfilePath); os.IsNotExist(statErr) {
	    err = fmt.Errorf("specified or detected Dockerfile does not exist: %s", dockerfilePath)
	    return
    }

	return filepath.Clean(dockerfilePath), filepath.Clean(buildContextDir), nil
}

// buildSingleImageWithLogs est la version de buildSingleImage qui accepte un io.Writer pour les logs.
func (s *BuildService) buildSingleImageWithLogs(ctx context.Context, buildContextDir string, dockerfilePath string, spec *BuildSpec, logWriter io.Writer) (string, error) {
	buildContextTar, err := archive.TarWithOptions(buildContextDir, &archive.TarOptions{})
	if err != nil {
		fmt.Fprintf(logWriter, "ERROR creating build context tar: %v\n", err)
		return "", fmt.Errorf("error creating context tar for '%s': %w", buildContextDir, err)
	}
	defer buildContextTar.Close()

	buildOptions := types.ImageBuildOptions{
		Dockerfile: filepath.Base(dockerfilePath),
		Tags:       spec.BuildConfig.Tags,
		Remove:     true,
		ForceRemove: true,
		NoCache:    spec.BuildConfig.NoCache,
		BuildArgs:  make(map[string]*string),
		PullParent: spec.BuildConfig.Pull,
		Version:    types.BuilderBuildKit, // Préférer BuildKit
		Target:     spec.BuildConfig.Target,
		// Platforms: spec.BuildConfig.Platforms, // Ajouter si besoin
	}
	if !spec.BuildConfig.BuildKit { buildOptions.Version = types.BuilderV1 }
	for k, v := range spec.BuildConfig.Args { value := v; buildOptions.BuildArgs[k] = &value }

	fmt.Fprintf(logWriter, "Starting Docker build (Dockerfile: %s, Context: %s)...\n", buildOptions.Dockerfile, buildContextDir)
	buildResponse, err := s.dockerClient.ImageBuild(ctx, buildContextTar, buildOptions)
	// ... (gestion fallback legacy builder si besoin) ...
	if err != nil {
		fmt.Fprintf(logWriter, "ERROR starting Docker build: %v\n", err)
		return "", fmt.Errorf("error starting Docker build: %w", err)
	}
	defer buildResponse.Body.Close()

	// Streamer la sortie JSON vers le logWriter fourni
	var imageID string
	err = jsonmessage.DisplayJSONMessagesStream(buildResponse.Body, logWriter, 0, false, func(msg jsonmessage.JSONMessage) {
		// Essayer d'extraire l'ID de l'image depuis les messages "Successfully built" ou Aux
		if strings.Contains(msg.Stream, "Successfully built ") {
			parts := strings.Fields(msg.Stream)
			if len(parts) >= 3 && parts[0] == "Successfully" && parts[1] == "built" {
				id := strings.TrimPrefix(parts[2], "sha256:")
				if id != "" { imageID = id }
			}
		}
		if msg.Aux != nil {
			var auxMsg struct { ID string `json:"ID"` }
			if json.Unmarshal(*msg.Aux, &auxMsg) == nil && auxMsg.ID != "" {
				id := strings.TrimPrefix(auxMsg.ID, "sha256:")
				if id != "" { imageID = id } // Préférer l'ID de Aux
			}
		}
	})

	if err != nil {
		fmt.Fprintf(logWriter, "ERROR streaming build logs: %v\n", err)
		// Continuer pour voir si on a quand même eu un ID
	}

	if imageID == "" {
		// Essayer de récupérer l'ID via le tag si possible
		if len(buildOptions.Tags) > 0 {
			inspected, inspectErr := s.getImageInfoByTag(ctx, buildOptions.Tags[0])
			if inspectErr == nil {
				imageID = inspected.ID
				fmt.Fprintf(logWriter, "Retrieved Image ID via tag inspection: %s\n", imageID)
			} else {
				fmt.Fprintf(logWriter, "WARNING: Could not find image ID in logs and tag inspection failed: %v\n", inspectErr)
				if err == nil { // Si le stream s'est bien terminé mais sans ID
					err = fmt.Errorf("build stream finished but image ID could not be determined")
				}
			}
		} else if err == nil {
			err = fmt.Errorf("build stream finished but image ID could not be determined (no tags specified)")
		}
	}

	if err != nil {
		return "", err // Retourner l'erreur de stream ou l'erreur "ID non trouvé"
	}

	fmt.Fprintf(logWriter, "Docker build finished. Image ID: %s\n", imageID)
	return imageID, nil
}