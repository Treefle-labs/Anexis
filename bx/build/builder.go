package build

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	// Go-Git imports
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/joho/godotenv" // for the .env files loading
	"github.com/moby/go-archive"
	"github.com/moby/term"
	"gopkg.in/yaml.v3"

	// mod for B2
	"github.com/Backblaze/blazer/b2"
)


// UnmarshalYAML handle the case which `build: ./context` and `build: {context: ...}`
func (cb *ComposeBuild) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode { // Case build: ./context
		cb.Context = value.Value
		return nil
	}
	// Case build: { ... } (map)
	// use a temp type to avoid the infinite recursion
	type ComposeBuildMap struct {
		Context    string             `yaml:"context,omitempty"`
		Dockerfile string             `yaml:"dockerfile,omitempty"`
		Args       map[string]*string `yaml:"args,omitempty"`
		Target     string             `yaml:"target,omitempty"`
		CacheFrom  []string           `yaml:"cache_from,omitempty"`
		Labels     map[string]string  `yaml:"labels,omitempty"`
		Network    string             `yaml:"network,omitempty"`
	}
	var temp ComposeBuildMap
	if err := value.Decode(&temp); err != nil {
		return err
	}
	cb.Context = temp.Context
	cb.Dockerfile = temp.Dockerfile
	cb.Args = temp.Args
	cb.Target = temp.Target
	cb.CacheFrom = temp.CacheFrom
	cb.Labels = temp.Labels
	cb.Network = temp.Network

	// Apply the default if context is empty but build is a non empty map
	if cb.Context == "" && !value.IsZero() && value.Kind == yaml.MappingNode {
		cb.Context = "."
	}
	return nil
}

// This is a healthcheck simplified struct
type HealthCheck struct {
	Test        []string `yaml:"test,omitempty"`
	Interval    string   `yaml:"interval,omitempty"`
	Timeout     string   `yaml:"timeout,omitempty"`
	Retries     *int     `yaml:"retries,omitempty"`
	StartPeriod string   `yaml:"start_period,omitempty"`
}

// Interface for an extern secrets service provider
type SecretFetcher interface {
	GetSecret(ctx context.Context, source string) (string, error) // Must return the secret value
}

// --- Service Initialization ---

// Create a new instance of the build service
func NewBuildService(workDir string, inMemory bool, secretFetcher SecretFetcher) (*BuildService, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("error during the Docker client initialization: %w", err)
	}

	// Creating the working directory
	effectiveWorkDir := workDir
	if inMemory && workDir == "" {
		// Memory mode creating the working temp dir
		tmpDir, err := os.MkdirTemp("", "buildservice-work-")
		if err != nil {
			return nil, fmt.Errorf("failed to create the temp working dir: %w", err)
		}
		effectiveWorkDir = tmpDir
		// TODO: Assuming that the program delete this temp dir
	} else if !inMemory && workDir != "" {
		if err := os.MkdirAll(workDir, 0755); err != nil {
			return nil, fmt.Errorf("working dir creation failed %s: %w", workDir, err)
		}
	}

	return &BuildService{
		dockerClient:  cli,
		workDir:       effectiveWorkDir,
		inMemory:      inMemory,
		secretFetcher: secretFetcher, // Inject the secret fetcher
		mutex:         sync.Mutex{},
	}, nil
}

func (s *BuildService) Cleanup() error {
	if err := os.RemoveAll(s.workDir); err != nil {
		return fmt.Errorf("failed to clean the working dir: %s %w", s.workDir, err)
	}

	return nil
}

// SetB2Config configure the B2 configuration
func (s *BuildService) SetB2Config(config *B2Config) {
	s.b2Config = config
}

// --- Configuration Loading ---

// Load the build config from a file
func LoadBuildSpecFromFile(filename string) (*BuildSpec, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("cannot read the build file specification '%s': %w", filename, err)
	}
	return LoadBuildSpecFromBytes(data, filepath.Ext(filename))
}

// Load the build config from byte array
func LoadBuildSpecFromBytes(data []byte, format string) (*BuildSpec, error) {
	var spec BuildSpec
	var err error

	// Set defaults
	spec.BuildConfig.OutputTarget = "docker"     // Default output target
	spec.RunConfigDef.Generate = true            // Default to generating run config
	spec.RunConfigDef.ArtifactStorage = "docker" // Default artifact storage for run config

	if format == ".json" {
		err = json.Unmarshal(data, &spec)
	} else if format == ".yaml" || format == ".yml" {
		err = yaml.Unmarshal(data, &spec)
	} else {
		// Try YAML decoding by default if format is unknown or missing
		err = yaml.Unmarshal(data, &spec)
		if err != nil {
			// If YAML fails, try JSON as a fallback
			errJson := json.Unmarshal(data, &spec)
			if errJson != nil {
				return nil, fmt.Errorf("invalid format. YAML error: %v, JSON error: %v", err, errJson)
			}
			err = nil // JSON succeeded
		}
	}

	if err != nil {
		return nil, fmt.Errorf("specification parsing failed (format: %s): %w", format, err)
	}

	// Basic Validation
	if spec.Name == "" || spec.Version == "" {
		return nil, fmt.Errorf("the fields 'name' and 'version' are required in the specification")
	}
	if len(spec.Codebases) == 0 && len(spec.BuildSteps) == 0 && spec.BuildConfig.Dockerfile == "" && spec.BuildConfig.ComposeFile == "" {
		return nil, fmt.Errorf("no codebase, build_step, dockerfile or compose_file specified")
	}
	if spec.BuildConfig.Dockerfile != "" && spec.BuildConfig.ComposeFile != "" {
		return nil, fmt.Errorf("don't specify 'dockerfile' et 'compose_file' in the build_config")
	}

	return &spec, nil
}

// parse a compose file
func LoadComposeFile(data []byte) (*ComposeProject, error) {
	var project ComposeProject
	err := yaml.Unmarshal(data, &project)
	if err != nil {
		return nil, fmt.Errorf("error during the compose YAML file parsing: %w", err)
	}
	if len(project.Services) == 0 {
		return nil, fmt.Errorf("no service section found in the compose file config")
	}
	// Initializing the maps/slices nil to avoid the nil pointer panics
	for _, service := range project.Services {
		if service.Environment == nil {
			service.Environment = make(map[string]*string)
		}
		if service.Build != nil && service.Build.Args == nil {
			service.Build.Args = make(map[string]*string)
		}
		// TODO: do this for other map slice...
	}
	return &project, nil
}

func (s *BuildService) GetSecret(ctx context.Context, source string) (string, error) {
	s.mutex.Lock()
	fetcher := s.secretFetcher
	defer s.mutex.Unlock()

	if fetcher == nil {
		// Using the default DummySecretFetcher if no fetcher is initialized
		fetcher = &DummySecretFetcher{}
	}
	return fetcher.GetSecret(ctx, source)
}

// --- Core Build Logic ---

// Running the build based on the provided spec
func (s *BuildService) Build(ctx context.Context, spec *BuildSpec) (*BuildResult, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	startTime := time.Now()
	result := &BuildResult{
		Artifacts:       make(map[string][]byte), // Legacy, might remove
		Logs:            "",
		ImageIDs:        make(map[string]string),
		ImageSizes:      make(map[string]int64),
		LocalImagePaths: make(map[string]string),
		ServiceOutputs:  make(map[string]ServiceOutput),
	}
	var overallLogs strings.Builder // Collect logs from all steps

	// --- 1. Setup Build Environment ---
	buildID := fmt.Sprintf("%s-%s-%d", spec.Name, spec.Version, time.Now().UnixNano())
	buildDir := filepath.Join(s.workDir, buildID) // Main directory for this build

	if err := os.MkdirAll(buildDir, 0755); err != nil {
		result.Success = false
		result.ErrorMessage = fmt.Sprintf("cannot create the build dir '%s': %v", buildDir, err)
		return result, fmt.Errorf("error during the run: \n %s", result.ErrorMessage)
	}
	// Cleanup build directory unless OutputTarget is local and no path is specified
	shouldCleanup := !(spec.BuildConfig.OutputTarget == "local" && spec.BuildConfig.LocalPath == "")
	if shouldCleanup {
		defer func() {
			// Add some robustness: Check if buildDir still exists
			if _, err := os.Stat(buildDir); err == nil || !os.IsNotExist(err) {
				os.RemoveAll(buildDir)
			}
		}()
	}
	overallLogs.WriteString(fmt.Sprintf("Using build directory: %s\n", buildDir))

	// --- 2. Load Environment Variables ---
	mergedEnv := make(map[string]string)
	// Load from EnvFiles first
	for _, envFile := range spec.EnvFiles {
		// Assume relative path to buildDir or potentially absolute path? Let's try relative first.
		envFilePath := filepath.Join(buildDir, envFile) // Or maybe relative to spec file location? Needs clarification.
		if _, err := os.Stat(envFilePath); os.IsNotExist(err) {
			// If relative to buildDir doesn't exist, try absolute
			envFilePath = envFile
		}
		envMap, err := godotenv.Read(envFilePath)
		if err != nil {
			overallLogs.WriteString(fmt.Sprintf("Warning: cannot read env file '%s': %v\n", envFile, err))
		} else {
			for k, v := range envMap {
				if _, exists := mergedEnv[k]; !exists { // Avoid overriding already set vars from earlier files
					mergedEnv[k] = v
				}
			}
		}
	}
	// Override with spec.Env
	for k, v := range spec.Env {
		mergedEnv[k] = v
	}
	overallLogs.WriteString(fmt.Sprintf("Loaded %d environment variables\n", len(mergedEnv)))

	// --- 3. Fetch Secrets (Placeholder) ---
	runtimeSecrets := make(map[string]string) // Secrets for runtime (.run.yml)
	if s.secretFetcher != nil && len(spec.Secrets) > 0 {
		overallLogs.WriteString("Fetching secrets...\n")
		for _, secretSpec := range spec.Secrets {
			if secretSpec.InjectMethod == "" || secretSpec.InjectMethod == "env" {
				secretValue, err := s.secretFetcher.GetSecret(ctx, secretSpec.Source)
				if err != nil {
					errMsg := fmt.Sprintf("error during the secret creation '%s' (source: %s): %v", secretSpec.Name, secretSpec.Source, err)
					overallLogs.WriteString(errMsg + "\n")
					result.Success = false
					result.ErrorMessage = errMsg
					result.Logs = overallLogs.String()
					return result, fmt.Errorf("error during the run: \n %s", errMsg)
				}
				runtimeSecrets[secretSpec.Name] = secretValue
				overallLogs.WriteString(fmt.Sprintf("Secret '%s' fetched successfully.\n", secretSpec.Name))
			} else {
				overallLogs.WriteString(fmt.Sprintf("Warning: Secret injection method '%s' for '%s' not yet supported.\n", secretSpec.InjectMethod, secretSpec.Name))
			}
		}
	}

	// Combine regular envs and secret envs for runtime config
	finalRuntimeEnv := make(map[string]string)
	for k, v := range mergedEnv {
		finalRuntimeEnv[k] = v
	}
	for k, v := range runtimeSecrets {
		finalRuntimeEnv[k] = v // Secrets override regular env if names clash
	}

	// --- 4. Download Resources ---
	overallLogs.WriteString("Downloading resources...\n")
	for _, res := range spec.Resources {
		overallLogs.WriteString(fmt.Sprintf("Downloading %s to %s...\n", res.URL, res.TargetPath))
		targetFullPath := filepath.Join(buildDir, res.TargetPath)
		targetDir := filepath.Dir(targetFullPath)
		if err := os.MkdirAll(targetDir, 0755); err != nil {
			errMsg := fmt.Sprintf("error during the resource target directory creation '%s': %v", targetFullPath, err)
			result.Success = false
			result.ErrorMessage = errMsg
			result.Logs = overallLogs.String()
			return result, fmt.Errorf("error during the run: \n %s", errMsg)
		}

		err := s.downloadFile(ctx, res.URL, targetFullPath)
		if err != nil {
			errMsg := fmt.Sprintf("error during the resource downloading '%s': %v", res.URL, err)
			result.Success = false
			result.ErrorMessage = errMsg
			result.Logs = overallLogs.String()
			return result, fmt.Errorf("error during the run: \n %s", errMsg)
		}

		if res.Extract {
			overallLogs.WriteString(fmt.Sprintf("Extracting %s...\n", targetFullPath))
			// Extract needs to place files inside targetDir, not create a new subdir named after the archive
			err := s.extractArchive(targetFullPath, targetDir)
			if err != nil {
				errMsg := fmt.Sprintf("error during the archive extraction '%s': %v", targetFullPath, err)
				// Log warning but continue? Or fail? Let's fail for now.
				result.Success = false
				result.ErrorMessage = errMsg
				result.Logs = overallLogs.String()
				return result, fmt.Errorf("error during the run: \n %s", errMsg)
			}
			// Optionally remove the archive after extraction
			os.Remove(targetFullPath)
			overallLogs.WriteString(fmt.Sprintf("Extracted %s successfully.\n", res.TargetPath))
		}
	}

	// --- 5. Prepare Codebases ---
	overallLogs.WriteString("Fetching codebases...\n")
	codebaseMap := make(map[string]CodebaseConfig) // For easy lookup by name
	for _, codebase := range spec.Codebases {
		codebaseMap[codebase.Name] = codebase
		var destDir string
		// If TargetInHost is specified, place it there relative to buildDir
		if codebase.TargetInHost != "" {
			destDir = filepath.Join(buildDir, codebase.TargetInHost)
		} else {
			// Default: place it in a subdirectory named after the codebase
			destDir = filepath.Join(buildDir, codebase.Name)
		}

		overallLogs.WriteString(fmt.Sprintf("Fetching codebase '%s' (%s: %s) into %s\n", codebase.Name, codebase.SourceType, codebase.Source, destDir))
		if err := s.fetchCodebase(ctx, codebase, destDir); err != nil {
			errMsg := fmt.Sprintf("error during the codebase fetching '%s': %v", codebase.Name, err)
			result.Success = false
			result.ErrorMessage = errMsg
			result.Logs = overallLogs.String()
			return result, fmt.Errorf("error during the run: \n %s", errMsg)
		}
	}

	// --- 6. Execute Build Steps (Sequential Build & Binary Handling) ---
	extractedBinaries := make(map[string][]byte) // Map step name -> binary data
	overallLogs.WriteString("Executing build steps...\n")
	for _, step := range spec.BuildSteps {
		overallLogs.WriteString(fmt.Sprintf("--- Build Step: %s ---\n", step.Name))
		cb, ok := codebaseMap[step.CodebaseName]
		if !ok {
			errMsg := fmt.Sprintf("build step '%s' referencing a non existent codebase: '%s'", step.Name, step.CodebaseName)
			result.Success = false
			result.ErrorMessage = errMsg
			result.Logs = overallLogs.String()
			return result, fmt.Errorf("error during the run: \n %s", errMsg)
		}

		stepBuildDir := filepath.Join(buildDir, cb.Name) // Assume codebase is in its named dir

		// Inject binary from previous step if needed
		if step.UseBinaryFromStep != "" {
			binaryData, exists := extractedBinaries[step.UseBinaryFromStep]
			if !exists {
				errMsg := fmt.Sprintf("build step '%s' require a binary for the step '%s', but it's not found", step.Name, step.UseBinaryFromStep)
				result.Success = false
				result.ErrorMessage = errMsg
				result.Logs = overallLogs.String()
				return result, fmt.Errorf("error during the run: \n %s", errMsg)
			}
			if step.BinaryTargetPath == "" {
				errMsg := fmt.Sprintf("build step '%s' uses a 'binary_target_path' not defined", step.Name)
				result.Success = false
				result.ErrorMessage = errMsg
				result.Logs = overallLogs.String()
				return result, fmt.Errorf("error during the run: \n %s", errMsg)
			}

			targetBinaryPath := filepath.Join(stepBuildDir, step.BinaryTargetPath)
			targetBinaryDir := filepath.Dir(targetBinaryPath)
			overallLogs.WriteString(fmt.Sprintf("Injecting binary from step '%s' to '%s'\n", step.UseBinaryFromStep, targetBinaryPath))
			if err := os.MkdirAll(targetBinaryDir, 0755); err != nil {
				errMsg := fmt.Sprintf("error during the repertory '%s' creation for the injected binary: %v", targetBinaryDir, err)
				result.Success = false
				result.ErrorMessage = errMsg
				result.Logs = overallLogs.String()
				return result, fmt.Errorf("error during the run: \n %s", errMsg)
			}
			if err := os.WriteFile(targetBinaryPath, binaryData, 0755); err != nil { // Make executable
				errMsg := fmt.Sprintf("error during the binary writing '%s': %v", targetBinaryPath, err)
				result.Success = false
				result.ErrorMessage = errMsg
				result.Logs = overallLogs.String()
				return result, fmt.Errorf("error during the run: \n %s", errMsg)
			}
		}

		// Build this step's codebase (assuming it has a Dockerfile)
		// We need a way to find the Dockerfile for this specific step/codebase
		stepDockerfilePath := filepath.Join(stepBuildDir, "Dockerfile") // Default assumption
		// Allow overriding Dockerfile path via CodebaseConfig or BuildStep? For now, default.
		if _, err := os.Stat(stepDockerfilePath); os.IsNotExist(err) {
			errMsg := fmt.Sprintf("No Dockerfile founded '%s' in the build step '%s' (waiting path: %s)", cb.Name, step.Name, stepDockerfilePath)
			result.Success = false
			result.ErrorMessage = errMsg
			result.Logs = overallLogs.String()
			return result, fmt.Errorf("error during the run: \n %s", errMsg)
		}

		// Create a temporary BuildSpec for this step
		stepSpec := &BuildSpec{
			Name:    fmt.Sprintf("%s-%s-step-%s", spec.Name, spec.Version, step.Name),
			Version: "latest",
			BuildConfig: BuildConfig{
				// Use build args from the main spec? Or step-specific? Let's use main spec for now.
				Args:    spec.BuildConfig.Args,
				NoCache: spec.BuildConfig.NoCache,
				Tags:    []string{fmt.Sprintf("%s-%s-step-%s:latest", spec.Name, spec.Version, step.Name)}, // Temporary tag
				Pull:    spec.BuildConfig.Pull,
			},
		}

		// Build the image for the step
		stepImageID, stepLogs, err := s.buildSingleImage(ctx, stepBuildDir, stepDockerfilePath, stepSpec)
		overallLogs.WriteString(fmt.Sprintf("Logs for step %s:\n%s\n", step.Name, stepLogs))
		if err != nil {
			errMsg := fmt.Sprintf("error during the step build '%s': %v", step.Name, err)
			result.Success = false
			result.ErrorMessage = errMsg
			result.Logs = overallLogs.String()
			return result, fmt.Errorf("error during the run: \n %s", errMsg)
		}
		overallLogs.WriteString(fmt.Sprintf("Step '%s' built successfully, ImageID: %s\n", step.Name, stepImageID))

		// Extract binary if needed
		if step.OutputsBinaryPath != "" {
			overallLogs.WriteString(fmt.Sprintf("Extracting binary '%s' from step '%s' image %s\n", step.OutputsBinaryPath, step.Name, stepImageID))
			binaryData, err := s.extractFromContainer(ctx, stepImageID, step.OutputsBinaryPath)
			if err != nil {
				errMsg := fmt.Sprintf("erro during the extraction of the binary '%s' in the step '%s': %v", step.OutputsBinaryPath, step.Name, err)
				result.Success = false
				result.ErrorMessage = errMsg
				result.Logs = overallLogs.String()
				return result, fmt.Errorf("error during the run: \n %s", errMsg)
			}
			extractedBinaries[step.Name] = binaryData
			overallLogs.WriteString(fmt.Sprintf("Binary extracted successfully (%d bytes).\n", len(binaryData)))
		}
		overallLogs.WriteString(fmt.Sprintf("--- End Build Step: %s ---\n", step.Name))
	} // End of build steps loop

	// --- 7. Main Build Execution ---
	overallLogs.WriteString("--- Starting Main Build ---\n")

	if spec.BuildConfig.ComposeFile != "" {
		// --- 7a. Build using Docker Compose ---
		overallLogs.WriteString(fmt.Sprintf("Building using Compose file: %s\n", spec.BuildConfig.ComposeFile))
		composeFilePath := filepath.Join(buildDir, spec.BuildConfig.ComposeFile)
		composeData, err := os.ReadFile(composeFilePath)
		if err != nil {
			errMsg := fmt.Sprintf("error during the compose file reading '%s': %v", composeFilePath, err)
			result.Success = false
			result.ErrorMessage = errMsg
			result.Logs = overallLogs.String()
			return result, fmt.Errorf("error during the run: \n %s", errMsg)
		}

		// Use the provided LoadComposeFile function (assuming it's adapted for compose-go v2)
		composeProject, err := LoadComposeFile(composeData)
		if err != nil {
			errMsg := fmt.Sprintf("error during the compose file parsing '%s': %v", spec.BuildConfig.ComposeFile, err)
			result.Success = false
			result.ErrorMessage = errMsg
			result.Logs = overallLogs.String()
			return result, fmt.Errorf("error during the run: \n %s", errMsg)
		}

		buildErrs := s.buildComposeProject(ctx, buildDir, composeProject, spec, result, &overallLogs)
		if len(buildErrs) > 0 {
			errMsg := fmt.Sprintf("errors during the compose project building: %v", buildErrs)
			result.Success = false
			result.ErrorMessage = strings.Join(buildErrs, "; ")
			result.Logs = overallLogs.String()
			return result, fmt.Errorf("error during the run: \n %s", errMsg)
		}
		// Note: ImageID in result might remain empty if compose file only defines services with existing images
		overallLogs.WriteString("Compose project built successfully.\n")

	} else {
		// --- 7b. Build using Dockerfile ---
		dockerfilePath := ""
		buildContextDir := buildDir // Default context is the root build directory

		if spec.BuildConfig.Dockerfile != "" {
			// Check if Dockerfile content is inline or a path
			if strings.Contains(spec.BuildConfig.Dockerfile, "\n") {
				// Inline Dockerfile content
				dockerfilePath = filepath.Join(buildDir, "Dockerfile.inline")
				if err := os.WriteFile(dockerfilePath, []byte(spec.BuildConfig.Dockerfile), 0644); err != nil {
					errMsg := fmt.Sprintf("error during the inline Dockerfile creation: %v", err)
					result.Success = false
					result.ErrorMessage = errMsg
					result.Logs = overallLogs.String()
					return result, fmt.Errorf("error during the run: \n %s", errMsg)
				}
				overallLogs.WriteString("Using inline Dockerfile.\n")
			} else {
				// Path to Dockerfile relative to buildDir
				dockerfilePath = filepath.Join(buildDir, spec.BuildConfig.Dockerfile)
				// The build context might need adjustment if the Dockerfile is not at the root
				buildContextDir = filepath.Dir(dockerfilePath)
				overallLogs.WriteString(fmt.Sprintf("Using Dockerfile at path: %s\n", spec.BuildConfig.Dockerfile))
			}
		} else {
			// Auto-detect Dockerfile (simple case: look for Dockerfile at the root)
			dfPath := filepath.Join(buildDir, "Dockerfile")
			if _, err := os.Stat(dfPath); err == nil {
				dockerfilePath = dfPath
				buildContextDir = buildDir
				overallLogs.WriteString("Auto-detected Dockerfile at build root.\n")
			} else {
				// Try finding in the first codebase dir (legacy behavior, might need refinement)
				if len(spec.Codebases) > 0 {
					firstCodebaseDir := filepath.Join(buildDir, spec.Codebases[0].Name)
					dfPath = filepath.Join(firstCodebaseDir, "Dockerfile")
					if _, err := os.Stat(dfPath); err == nil {
						dockerfilePath = dfPath
						buildContextDir = firstCodebaseDir // Context is the codebase dir
						overallLogs.WriteString(fmt.Sprintf("Auto-detected Dockerfile in first codebase: %s\n", spec.Codebases[0].Name))
					}
				}
			}
		}

		if dockerfilePath == "" {
			errMsg := "not found/provided Dockerfile for the build"
			result.Success = false
			result.ErrorMessage = errMsg
			result.Logs = overallLogs.String()
			return result, fmt.Errorf("error during the run: \n %s", errMsg)
		}

		// Perform the build for the single Dockerfile
		imageID, logs, err := s.buildSingleImage(ctx, buildContextDir, dockerfilePath, spec)
		overallLogs.WriteString(fmt.Sprintf("Dockerfile Build Logs:\n%s\n", logs))
		if err != nil {
			errMsg := fmt.Sprintf("erreur lors du build Docker: %v", err)
			result.Success = false
			result.ErrorMessage = errMsg
			result.Logs = overallLogs.String()
			return result, fmt.Errorf("error during the run: \n %s", errMsg)
		}

		// Store result for the single image build
		result.ImageID = imageID
		imageSize, err := s.getImageSize(ctx, imageID)
		if err == nil {
			result.ImageSize = imageSize
		} else {
			overallLogs.WriteString(fmt.Sprintf("Warning: could not get size for image %s: %v\n", imageID, err))
		}
		// Add to ServiceOutputs as a pseudo-service if needed for consistency
		mainServiceName := spec.Name // Use build name as service name
		result.ServiceOutputs[mainServiceName] = ServiceOutput{
			ImageID:   imageID,
			ImageSize: imageSize,
			Logs:      logs,
		}
		result.ImageIDs[mainServiceName] = imageID
		result.ImageSizes[mainServiceName] = imageSize

		overallLogs.WriteString(fmt.Sprintf("Dockerfile build successful. ImageID: %s, Size: %d\n", imageID, imageSize))
	}

	// --- 8. Handle Build Outputs (Save/Upload Images) ---
	outputBasePath := buildDir // Default base for local output
	if spec.BuildConfig.OutputTarget == "local" && spec.BuildConfig.LocalPath != "" {
		outputBasePath = spec.BuildConfig.LocalPath
		if err := os.MkdirAll(outputBasePath, 0755); err != nil {
			errMsg := fmt.Sprintf("cannot create the output base directory '%s': %v", outputBasePath, err)
			result.Success = false
			result.ErrorMessage = errMsg
			result.Logs = overallLogs.String()
			return result, fmt.Errorf("error during the run: \n %s", errMsg)
		}
		overallLogs.WriteString(fmt.Sprintf("Using custom local output path: %s\n", outputBasePath))
	}

	finalImageTags := make(map[string][]string) // serviceName -> tags

	// Collect image IDs and desired tags
	if spec.BuildConfig.ComposeFile != "" {
		// Get tags from the built compose services
		for serviceName, serviceOutput := range result.ServiceOutputs {
			// Generate default tag if none specific
			defaultTag := fmt.Sprintf("%s_%s:latest", spec.Name, serviceName)
			finalImageTags[serviceName] = []string{defaultTag} // Simple tagging for now
			// We could potentially read custom tags from the compose file's build section
			// Apply tags to the image
			for _, tag := range finalImageTags[serviceName] {
				if err := s.dockerClient.ImageTag(ctx, serviceOutput.ImageID, tag); err != nil {
					overallLogs.WriteString(fmt.Sprintf("Warning: Failed to tag image %s for service %s with tag %s: %v\n", serviceOutput.ImageID, serviceName, tag, err))
				} else {
					overallLogs.WriteString(fmt.Sprintf("Tagged image %s for service %s with %s\n", serviceOutput.ImageID, serviceName, tag))
				}
			}
		}
	} else if result.ImageID != "" {
		// Get tags from the main build config for the single image
		mainServiceName := spec.Name
		if len(spec.BuildConfig.Tags) > 0 {
			finalImageTags[mainServiceName] = spec.BuildConfig.Tags
		} else {
			// Generate default tag
			finalImageTags[mainServiceName] = []string{fmt.Sprintf("%s:%s", spec.Name, spec.Version)}
		}
		// Apply tags
		for _, tag := range finalImageTags[mainServiceName] {
			if err := s.dockerClient.ImageTag(ctx, result.ImageID, tag); err != nil {
				overallLogs.WriteString(fmt.Sprintf("Warning: Failed to tag image %s with tag %s: %v\n", result.ImageID, tag, err))
			} else {
				overallLogs.WriteString(fmt.Sprintf("Tagged image %s with %s\n", result.ImageID, tag))
			}
		}
	}

	// Save or upload based on OutputTarget
	overallLogs.WriteString(fmt.Sprintf("Handling build output target: %s\n", spec.BuildConfig.OutputTarget))
	switch spec.BuildConfig.OutputTarget {
	case "b2":
		if s.b2Config == nil {
			errMsg := "OutputTarget is 'b2' but no config is defined"
			result.Success = false
			result.ErrorMessage = errMsg
			result.Logs = overallLogs.String()
			return result, fmt.Errorf("error during the run: \n %s", errMsg)
		}
		for serviceName, serviceOutput := range result.ServiceOutputs {
			tags := finalImageTags[serviceName] // Get the tags we just applied
			overallLogs.WriteString(fmt.Sprintf("Exporting and uploading image for service '%s' (ID: %s) to B2...\n", serviceName, serviceOutput.ImageID))
			// Adapt exportAndUploadImage to handle multiple tags per image
			objectNames, err := s.exportAndUploadImage(ctx, serviceOutput.ImageID, serviceName, spec.Version, tags)
			if err != nil {
				overallLogs.WriteString(fmt.Sprintf("Warning: Failed to export/upload image for service '%s' to B2: %v\n", serviceName, err))
				// Continue with other images? Or fail? Let's continue but log.
			} else {
				result.B2ObjectNames = append(result.B2ObjectNames, objectNames...)
				overallLogs.WriteString(fmt.Sprintf("Service '%s' image uploaded to B2: %v\n", serviceName, objectNames))
			}
		}

	case "local":
		for serviceName, serviceOutput := range result.ServiceOutputs {
			imageFileName := fmt.Sprintf("%s_%s.tar", spec.Name, serviceName) // Consistent naming
			localImagePath := filepath.Join(outputBasePath, imageFileName)
			overallLogs.WriteString(fmt.Sprintf("Saving image for service '%s' (ID: %s) locally to %s...\n", serviceName, serviceOutput.ImageID, localImagePath))

			err := s.saveImageLocally(ctx, serviceOutput.ImageID, localImagePath)
			if err != nil {
				errMsg := fmt.Sprintf("error during the service image saving locally '%s': %v", serviceName, err)
				result.Success = false
				result.ErrorMessage = errMsg
				result.Logs = overallLogs.String()
				return result, fmt.Errorf("error during the run: \n %s", errMsg)
			}
			result.LocalImagePaths[serviceName] = localImagePath
			overallLogs.WriteString(fmt.Sprintf("Service '%s' image saved successfully.\n", serviceName))
		}
	case "docker":
		// Images are already in the local Docker daemon, tagged. Nothing more to do here.
		overallLogs.WriteString("Output target is 'docker', images are available in local daemon.\n")
	default:
		errMsg := fmt.Sprintf("OutputTarget not supported: %s", spec.BuildConfig.OutputTarget)
		result.Success = false
		result.ErrorMessage = errMsg
		result.Logs = overallLogs.String()
		return result, fmt.Errorf("error during the run: \n %s", errMsg)
	}

	// --- 9. Generate *.run.yml ---
	if spec.RunConfigDef.Generate {
		overallLogs.WriteString("Generating *.run.yml file...\n")
		runConfigPath := filepath.Join(outputBasePath, fmt.Sprintf("%s-%s.run.yml", spec.Name, spec.Version))

		// Loading the project if it's compose
		var parsedComposeProject *ComposeProject // Using a simplified type
		if spec.BuildConfig.ComposeFile != "" {
			composeFilePath := filepath.Join(buildDir, spec.BuildConfig.ComposeFile) // Chemin dans le contexte de build temporaire
			composeData, err := os.ReadFile(composeFilePath)
			if err != nil {
				overallLogs.WriteString(fmt.Sprintf("Warning: Failed to read compose file '%s' for run.yml generation: %v\n", composeFilePath, err))
			} else {
				parsedComposeProject, err = LoadComposeFile(composeData)
				if err != nil {
					overallLogs.WriteString(fmt.Sprintf("Warning: Failed to parse compose file for run.yml generation: %v\n", err))
					parsedComposeProject = nil
				}
			}
		}

		runYAML, err := s.generateRunYAML(ctx, spec, result, finalRuntimeEnv, finalImageTags, parsedComposeProject)
		if err != nil {
			errMsg := fmt.Sprintf("error during the run.yml generating: %v", err)
			overallLogs.WriteString(fmt.Sprintf("Warning: %s\n", errMsg))
		} else if runYAML != nil && len(runYAML.Services) > 0 {
			yamlData, err := yaml.Marshal(runYAML)
			if err != nil {
				overallLogs.WriteString(fmt.Sprintf("Warning: Failed to parse run file for run.yml generation: %v\n", err))
			}
			os.WriteFile(runConfigPath, yamlData, 0755)
		} else {
			overallLogs.WriteString("Skipping writing run.yml as no services were generated.\n")
		}
	}

	// --- 10. Finalize ---
	result.Success = true
	result.BuildTime = time.Since(startTime).Seconds()
	result.Logs = overallLogs.String() // Assign collected logs

	// Clean up temporary build step images (optional)
	// Could add logic here to remove images tagged like *-step-*

	overallLogs.WriteString(fmt.Sprintf("Build finished successfully in %.2f seconds.\n", result.BuildTime))

	return result, nil
}

// --- Helper Functions ---

// fetching codebase from the provided source type and config
func (s *BuildService) fetchCodebase(ctx context.Context, config CodebaseConfig, destDir string) error {
	// Ensure the parent directory exists, but destDir itself should not exist for git clone
	parentDir := filepath.Dir(destDir)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		return fmt.Errorf("cannot create the parent directory '%s': %w", parentDir, err)
	}

	switch config.SourceType {
	case "git":
		// Remove destination dir if it exists before cloning
		if _, err := os.Stat(destDir); err == nil {
			if err := os.RemoveAll(destDir); err != nil {
				return fmt.Errorf("cannot clean the destination dir for repository fetching '%s': %w", destDir, err)
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("error during the verification of the destination directory '%s': %w", destDir, err)
		}
		return s.fetchGitRepoWithGoGit(ctx, config, destDir)
	case "local":
		// copyLocalDir expects destDir to exist
		if err := os.MkdirAll(destDir, 0755); err != nil {
			return fmt.Errorf("cannot create the destination dir '%s' for the local copy: %w", destDir, err)
		}
		return s.copyLocalDir(config.Source, destDir)
	case "archive":
		// extractArchive expects destDir to exist
		if err := os.MkdirAll(destDir, 0755); err != nil {
			return fmt.Errorf("cannot create the destination dir '%s' for the archive: %w", destDir, err)
		}
		return s.extractArchive(config.Source, destDir)
	case "buffer":
		if len(config.Content) == 0 {
			return fmt.Errorf("empty content for the buffer codebase type '%s'", config.Name)
		}
		// extractBufferToDir expects destDir to exist
		if err := os.MkdirAll(destDir, 0755); err != nil {
			return fmt.Errorf("cannot create the destination dir '%s' for the buffer: %w", destDir, err)
		}
		return s.extractBufferToDir(config.Content, destDir)
	default:
		return fmt.Errorf("this source type is not implemented yet '%s' for the codebase '%s'", config.SourceType, config.Name)
	}
}

// cloning repository using the go-git API
func (s *BuildService) fetchGitRepoWithGoGit(ctx context.Context, config CodebaseConfig, destDir string) error {
	options := &git.CloneOptions{
		URL:               config.Source,
		Progress:          os.Stdout,
		RecurseSubmodules: git.DefaultSubmoduleRecursionDepth,
		Auth:              nil, // TODO: Implement authentication
		RemoteName:        "origin",
		Depth:             0, // Clone full history by default
	}

	if config.Branch != "" {
		options.ReferenceName = plumbing.NewBranchReferenceName(config.Branch)
		options.SingleBranch = true
		options.Depth = 1
	}

	parentDir := filepath.Dir(destDir)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		return fmt.Errorf("cannot create the parent dir '%s': %w", parentDir, err)
	}
	if _, err := os.Stat(destDir); err == nil {
		fmt.Printf("Removing existing directory before clone: %s\n", destDir)
		if err := os.RemoveAll(destDir); err != nil {
			return fmt.Errorf("failed to remove the dest dir before cloning the repository '%s': %w", destDir, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("error during the dest repertory verification '%s': %w", destDir, err)
	}

	fmt.Printf("Cloning repository %s to %s...\n", config.Source, destDir)
	repo, err := git.PlainCloneContext(ctx, destDir, false, options)
	if err != nil {
		// Handle specific errors
		if err == transport.ErrAuthenticationRequired {
			return fmt.Errorf("authentication require for the codebase fetching '%s'. configure an auth provider (SSH key, token HTTPS)", config.Source)
		}
		if strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("the repertory '%s' already existing (post verification error): %w", destDir, err)
		}
		return fmt.Errorf("error during the repository cloning '%s' (branch: %s): %w", config.Source, config.Branch, err)
	}
	fmt.Printf("Repository cloned successfully.\n")

	// If a specific commit is requested, check it out
	if config.Commit != "" {
		fmt.Printf("Attempting to checkout commit %s...\n", config.Commit)
		w, err := repo.Worktree()
		if err != nil {
			return fmt.Errorf("cannot get the repository work tree '%s' after cloning: %w", config.Source, err)
		}

		commitHash := plumbing.NewHash(config.Commit)
		checkoutOptions := &git.CheckoutOptions{
			Hash:  commitHash,
			Force: true, // Force checkout
		}

		err = w.Checkout(checkoutOptions)
		if err != nil {
			fmt.Printf("Initial checkout failed for commit %s (%v), attempting fetch...\n", config.Commit, err)
			// Try fetching explicitly, making sure to fetch all heads and tags
			// which should bring in the necessary commit object if it exists remotely.
			fetchOpts := &git.FetchOptions{
				RefSpecs: []gitconfig.RefSpec{
					// Fetch all branches from the remote into remote-tracking branches
					gitconfig.RefSpec("+refs/heads/*:refs/remotes/origin/*"),
					// Fetch all tags
					gitconfig.RefSpec("+refs/tags/*:refs/tags/*"),
				},
				Auth:     options.Auth, // Reuse auth method from clone options
				Progress: os.Stdout,    // Show progress
				// Depth: 0, // Ensure full fetch if depth was used in clone? Or rely on default fetch behavior.
			}

			errFetch := repo.FetchContext(ctx, fetchOpts)
			// git.NoErrAlreadyUpToDate is expected if the commit was already there but checkout failed for other reasons
			if errFetch != nil && errFetch != git.NoErrAlreadyUpToDate {
				// Log the fetch error, but the primary error is still the checkout failure if retry also fails.
				fmt.Printf("Fetch failed: %v\n", errFetch)
				// Return combined error information
				return fmt.Errorf("error during the checkout of the commit '%s' (%w) and fetch also failed (%v)", config.Commit, err, errFetch)
			} else if errFetch == git.NoErrAlreadyUpToDate {
				fmt.Println("Fetch reported remote is already up-to-date.")
			} else {
				fmt.Println("Fetch completed successfully.")
			}

			// Retry checkout after fetch
			fmt.Printf("Retrying checkout of commit %s after fetch...\n", config.Commit)
			err = w.Checkout(checkoutOptions)
			if err != nil {
				// If it still fails after fetch, the commit might be invalid or unreachable
				return fmt.Errorf("error during the checkout of the commit '%s' (after fetch): %w", config.Commit, err)
			}
		}
		fmt.Printf("Successfully checked out commit %s\n", config.Commit)
	}

	return nil
}

// Used to copy a local dir/files with appropriate permissions
func (s *BuildService) copyLocalDir(source, dest string) error {
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !sourceInfo.IsDir() {
		return fmt.Errorf("the source '%s' doesn't exist", source)
	}

	// Ensure dest directory exists with source permissions
	if err := os.MkdirAll(dest, sourceInfo.Mode()); err != nil {
		return err
	}

	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		sourcePath := filepath.Join(source, entry.Name())
		destPath := filepath.Join(dest, entry.Name())

		fileInfo, err := entry.Info()
		if err != nil {
			return err
		}

		if entry.IsDir() {
			// Recursively copy subdirectory
			if err := s.copyLocalDir(sourcePath, destPath); err != nil {
				return err
			}
		} else if fileInfo.Mode()&os.ModeSymlink != 0 {
			// Handle symlinks (read link and recreate) - Optional, can be complex
			link, err := os.Readlink(sourcePath)
			if err != nil {
				return err
			}
			if err := os.Symlink(link, destPath); err != nil {
				return err
			}
		} else {
			// Copy regular file content and permissions
			data, err := os.ReadFile(sourcePath)
			if err != nil {
				return err
			}
			if err := os.WriteFile(destPath, data, fileInfo.Mode()); err != nil {
				return err
			}
		}
	}
	return nil
}

// Extract an archive (tar, tar.gz, zip) to a repertory
func (s *BuildService) extractArchive(sourcePath string, destDir string) error {
	file, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("cannot open the archive '%s': %w", sourcePath, err)
	}
	defer file.Close()

	// Peek at the first few bytes to guess the format
	header := make([]byte, 4)
	_, err = file.ReadAt(header, 0)
	if err != nil && err != io.EOF {
		return fmt.Errorf("cannot read the archive header '%s': %w", sourcePath, err)
	}
	// Reset reader position
	_, err = file.Seek(0, io.SeekStart)
	if err != nil {
		return fmt.Errorf("cannot reset the reading position in the archive '%s': %w", sourcePath, err)
	}

	if bytes.HasPrefix(header, []byte{0x1F, 0x8B}) {
		// Gzip compressed (likely tar.gz)
		gzr, err := gzip.NewReader(file)
		if err != nil {
			return fmt.Errorf("error during the gzip reader creation for the archive '%s': %w", sourcePath, err)
		}
		defer gzr.Close()
		return extractTar(tar.NewReader(gzr), destDir)
	} else if bytes.HasPrefix(header, []byte{0x50, 0x4B, 0x03, 0x04}) {
		// ZIP archive
		// Need file size for zip reader
		fileInfo, err := file.Stat()
		if err != nil {
			return fmt.Errorf("cannot get the zip file size '%s': %w", sourcePath, err)
		}
		return extractZip(file, fileInfo.Size(), destDir) // Implement extractZip
	} else {
		// Assume plain tar
		return extractTar(tar.NewReader(file), destDir)
	}
}

// Extract a buffer slice to a dir
func (s *BuildService) extractBufferToDir(data []byte, destDir string) error {
	dataReader := bytes.NewReader(data)

	if bytes.HasPrefix(data, []byte{0x1F, 0x8B}) {
		// Archive gzip (tar.gz)
		gzr, err := gzip.NewReader(dataReader)
		if err != nil {
			return fmt.Errorf("error during the archive reading from the buffer: %w", err)
		}
		defer gzr.Close()
		return extractTar(tar.NewReader(gzr), destDir)
	} else if bytes.HasPrefix(data, []byte{0x50, 0x4B, 0x03, 0x04}) {
		// Archive ZIP
		return extractZip(dataReader, int64(len(data)), destDir) // Implement extractZip for ReaderAt
	} else {
		// Supposer tar simple
		return extractTar(tar.NewReader(dataReader), destDir)
	}
}

// Extract a tar archive
func extractTar(tr *tar.Reader, destDir string) error {
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break // End of archive
		}
		if err != nil {
			return fmt.Errorf("error during the tar entry reading: %w", err)
		}

		// Sanitize the target path to prevent path traversal vulnerabilities
		target := filepath.Join(destDir, header.Name)
		if !strings.HasPrefix(target, filepath.Clean(destDir)+string(os.PathSeparator)) {
			return fmt.Errorf("invalid tar content: '%s' trying to get out from the source repertory", header.Name)
		}

		// Get file info from header
		info := header.FileInfo()

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, info.Mode()); err != nil {
				return fmt.Errorf("cannot create the repertory for the tar '%s': %w", target, err)
			}
		case tar.TypeReg:
			// Ensure parent directory exists
			parentDir := filepath.Dir(target)
			if err := os.MkdirAll(parentDir, 0755); err != nil { // Use default mode for parent dirs
				return fmt.Errorf("cannot the parent directory '%s' for the tar file: %w", parentDir, err)
			}

			// Create the file
			file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode())
			if err != nil {
				return fmt.Errorf("cannot create the tar file '%s': %w", target, err)
			}
			// Copy contents
			_, err = io.Copy(file, tr)
			file.Close() // Close immediately after copy
			if err != nil {
				return fmt.Errorf("error during the tar content copying '%s': %w", target, err)
			}
		case tar.TypeSymlink:
			// Recreate symlink
			if err := os.Symlink(header.Linkname, target); err != nil {
				return fmt.Errorf("cannot create the symblink for the tar '%s' -> '%s': %w", target, header.Linkname, err)
			}
		case tar.TypeLink:
			// Handle hard links (less common, might require mapping) - Skip for now
			fmt.Printf("Warning: Hard link extraction not fully supported (from %s to %s)\n", header.Name, header.Linkname)
		default:
			// Skip other types (char device, block device, fifo)
			fmt.Printf("Warning: Skipping unsupported tar entry type %c for %s\n", header.Typeflag, header.Name)
		}
	}
	return nil
}

// Extract a zip archive
func extractZip(r io.ReaderAt, size int64, destDir string) error {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return fmt.Errorf("error during the zip opening: %w", err)
	}

	for _, f := range zr.File {
		// Sanitize the target path
		targetPath := filepath.Join(destDir, f.Name)
		if !strings.HasPrefix(targetPath, filepath.Clean(destDir)+string(os.PathSeparator)) {
			return fmt.Errorf("invalid content: '%s' trying to get out from the target repertory", f.Name)
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(targetPath, f.Mode()); err != nil {
				return fmt.Errorf("cannot create the zip repertory '%s': %w", targetPath, err)
			}
			continue
		}

		// Ensure parent directory exists
		parentDir := filepath.Dir(targetPath)
		if err := os.MkdirAll(parentDir, 0755); err != nil {
			return fmt.Errorf("cannot create the parent repertory '%s' for the zip file: %w", parentDir, err)
		}

		// Open the file inside the zip archive
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("cannot open the file '%s' in the zip: %w", f.Name, err)
		}

		// Create the destination file
		outFile, err := os.OpenFile(targetPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			rc.Close()
			return fmt.Errorf("cannot create the targeting zip file '%s': %w", targetPath, err)
		}

		// Copy the content
		_, err = io.Copy(outFile, rc)

		// Close files
		outFile.Close()
		rc.Close()

		if err != nil {
			return fmt.Errorf("error during the zip content copying '%s': %w", f.Name, err)
		}
	}
	return nil
}

// Resource downloader
func (s *BuildService) downloadFile(ctx context.Context, url, targetPath string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("error during the request creation %s: %w", url, err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("error during the GET request for the resource URL %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed downloading of %s: status %s", url, resp.Status)
	}

	file, err := os.Create(targetPath)
	if err != nil {
		return fmt.Errorf("cannot create the target file %s: %w", targetPath, err)
	}
	defer file.Close()

	_, err = io.Copy(file, resp.Body)
	if err != nil {
		return fmt.Errorf("error during the target path writing %s: %w", targetPath, err)
	}

	return nil
}

// Build a single image from a context and a specific Config
func (s *BuildService) buildSingleImage(ctx context.Context, buildContextDir string, dockerfilePath string, spec *BuildSpec) (string, string, error) {
	var logBuffer bytes.Buffer

	// Créer le contexte de build en mémoire (tar)
	// Exclude .git by default? Or rely on .dockerignore? Let's rely on .dockerignore for now.
	buildContextTar, err := archive.TarWithOptions(buildContextDir, &archive.TarOptions{})
	if err != nil {
		return "", logBuffer.String(), fmt.Errorf("erreur lors de la création du contexte tar pour '%s': %w", buildContextDir, err)
	}
	defer buildContextTar.Close()

	// Préparer les options de build
	buildOptions := types.ImageBuildOptions{
		Dockerfile:  filepath.Base(dockerfilePath), // Dockerfile name relative to context root
		Tags:        spec.BuildConfig.Tags,         // Tags defined in the main spec or step spec
		Remove:      true,                          // Remove intermediate containers
		ForceRemove: true,
		NoCache:     spec.BuildConfig.NoCache,
		BuildArgs:   make(map[string]*string),
		PullParent:  spec.BuildConfig.Pull, // Tenter de pull l'image de base
		Version:     types.BuilderBuildKit, // Préférer BuildKit si disponible
		// TODO: Add Platform handling spec.BuildConfig.Platforms
	}
	if !spec.BuildConfig.BuildKit {
		buildOptions.Version = types.BuilderV1 // Force legacy builder if requested
	}

	// Ajouter les arguments de build (variables d'env du spec peuvent être utilisées ici si préfixées ou explicitement mappées)
	for k, v := range spec.BuildConfig.Args {
		value := v // Copie locale
		buildOptions.BuildArgs[k] = &value
	}
	// Injecter les variables d'env comme build args (optionnel, dépend de l'usage)
	// for k, v := range mergedEnv { // Use the merged env from Build() scope if needed
	// 	if _, exists := buildOptions.BuildArgs[k]; !exists {
	// 		value := v
	// 		buildOptions.BuildArgs[k] = &value
	// 	}
	// }

	if spec.BuildConfig.Target != "" {
		buildOptions.Target = spec.BuildConfig.Target
	}

	// Exécuter le build
	fmt.Fprintf(&logBuffer, "Starting Docker build with context: %s, Dockerfile: %s\n", buildContextDir, dockerfilePath)
	buildResponse, err := s.dockerClient.ImageBuild(ctx, buildContextTar, buildOptions)
	if err != nil {
		// Try falling back to legacy builder if BuildKit failed?
		if spec.BuildConfig.BuildKit && strings.Contains(err.Error(), "BuildKit") {
			fmt.Fprintf(&logBuffer, "BuildKit build failed, trying legacy builder...\n")
			buildOptions.Version = types.BuilderV1
			buildResponse, err = s.dockerClient.ImageBuild(ctx, buildContextTar, buildOptions)
		}
		if err != nil {
			logBuffer.WriteString(fmt.Sprintf("\nDocker build command failed: %v\n", err))
			return "", logBuffer.String(), fmt.Errorf("erreur lors du lancement du build Docker: %w", err)
		}
	}
	defer buildResponse.Body.Close()

	// Lire et traiter la sortie JSON
	var imageID string
	decoder := json.NewDecoder(buildResponse.Body)
	for {
		var msg jsonmessage.JSONMessage
		if err := decoder.Decode(&msg); err != nil {
			if err == io.EOF {
				break // Fin normale du stream
			}
			// Log incomplete stream error?
			logBuffer.WriteString(fmt.Sprintf("\nError decoding build response stream: %v\n", err))
			// Return success if we already got an image ID? Or fail? Let's fail.
			if imageID == "" {
				return "", logBuffer.String(), fmt.Errorf("erreur de décodage du flux de build et aucun ID d'image obtenu: %w", err)
			}
			break // Break but potentially return success if imageID was found
		}

		if msg.Stream != "" {
			fmt.Fprint(&logBuffer, msg.Stream)
			// Try to parse image ID from common "Successfully built <id>" messages
			if strings.Contains(msg.Stream, "Successfully built ") {
				parts := strings.Fields(msg.Stream)
				if len(parts) >= 3 && parts[0] == "Successfully" && parts[1] == "built" {
					// Handle potential sha256: prefix
					id := strings.TrimPrefix(parts[2], "sha256:")
					imageID = id
				}
			}
			// Docker Engine API v1.31+ may send ID in "Successfully tagged <tag>"
			if strings.Contains(msg.Stream, "Successfully tagged ") && imageID == "" {
				// Less reliable way to get ID if build is tagged, might need inspection later
			}
		} else if msg.Status != "" {
			logLine := msg.Status
			if msg.Progress != nil {
				logLine += " " + msg.Progress.String()
			}
			if msg.ID != "" {
				logLine = fmt.Sprintf("[%s] %s", msg.ID, logLine)
			}
			fmt.Fprintln(&logBuffer, logLine)
		}

		// Check for build errors reported in the stream
		if msg.Error != nil {
			logBuffer.WriteString(fmt.Sprintf("\nBuild Error: %s\n", msg.Error.Message))
			return "", logBuffer.String(), fmt.Errorf("erreur dans le flux de build: %s", msg.Error.Message)
		}

		// Extract Image ID from Aux message (often contains the final sha256 ID)
		if msg.Aux != nil {
			var auxMsg struct {
				ID string `json:"ID"`
			}
			if err := json.Unmarshal(*msg.Aux, &auxMsg); err == nil && auxMsg.ID != "" {
				// Prefer the ID from Aux if available
				id := strings.TrimPrefix(auxMsg.ID, "sha256:")
				imageID = id
			}
		}
	} // End stream reading loop

	if imageID == "" {
		// If no ID found after successful stream processing, maybe inspect the tag?
		if len(buildOptions.Tags) > 0 {
			inspected, err := s.getImageInfoByTag(ctx, buildOptions.Tags[0])
			if err == nil {
				imageID = inspected.ID
				fmt.Fprintf(&logBuffer, "\nImage ID retrieved via tag inspection: %s\n", imageID)
			} else {
				logBuffer.WriteString("\nBuild stream finished, but no image ID found and tag inspection failed.\n")
				return "", logBuffer.String(), fmt.Errorf("build terminé mais impossible de déterminer l'ID de l'image finale")
			}
		} else {
			logBuffer.WriteString("\nBuild stream finished, but no image ID found (and no tags specified).\n")
			return "", logBuffer.String(), fmt.Errorf("build terminé mais impossible de déterminer l'ID de l'image finale (aucun tag)")
		}
	}

	// Clean the image ID (remove potential sha256: prefix if still there)
	imageID = strings.TrimPrefix(imageID, "sha256:")

	fmt.Fprintf(&logBuffer, "\nBuild successful. Final Image ID: %s\n", imageID)
	return imageID, logBuffer.String(), nil
}

// buildComposeProject itère sur les services d'un projet Compose et les construit
func (s *BuildService) buildComposeProject(ctx context.Context, buildDir string, project *ComposeProject, spec *BuildSpec, result *BuildResult, overallLogs *strings.Builder) []string {
	var buildErrors []string
	composeFileDir := filepath.Dir(filepath.Join(buildDir, spec.BuildConfig.ComposeFile)) // Directory containing the compose file

	for Name, service := range project.Services {
		if service.Build == nil {
			// Service uses an existing image, maybe pull it?
			if service.Image != "" {
				overallLogs.WriteString(fmt.Sprintf("Service '%s' uses image '%s'. Pulling...\n", Name, service.Image))
				if err := s.pullImage(ctx, service.Image, overallLogs); err != nil {
					overallLogs.WriteString(fmt.Sprintf("Warning: Failed to pull image '%s' for service '%s': %v\n", service.Image, Name, err))
					// Continue or fail? Let's continue.
				}
			} else {
				overallLogs.WriteString(fmt.Sprintf("Service '%s' has no 'build' section and no 'image' specified. Skipping build.\n", Name))
			}
			continue
		}

		overallLogs.WriteString(fmt.Sprintf("--- Building Service: %s ---\n", Name))

		// Determine build context and Dockerfile path relative to the compose file directory
		contextPath := service.Build.Context
		if contextPath == "" || contextPath == "." {
			contextPath = composeFileDir
		} else if !filepath.IsAbs(contextPath) {
			contextPath = filepath.Join(composeFileDir, contextPath)
		}
		// Clean the path
		contextPath = filepath.Clean(contextPath)

		dockerfilePath := service.Build.Dockerfile
		if dockerfilePath == "" {
			dockerfilePath = "Dockerfile" // Default Dockerfile name
		}
		// Dockerfile path is relative to the context path
		fullDockerfilePath := filepath.Join(contextPath, dockerfilePath)

		overallLogs.WriteString(fmt.Sprintf("Service '%s': Context='%s', Dockerfile='%s'\n", Name, contextPath, fullDockerfilePath))

		// Create a temporary BuildSpec for this service build
		serviceSpec := &BuildSpec{
			Name:    fmt.Sprintf("%s-%s-service-%s", spec.Name, spec.Version, Name),
			Version: "latest", // Or derive from main spec?
			BuildConfig: BuildConfig{
				Args:    make(map[string]string),                  // Start with empty args
				NoCache: spec.BuildConfig.NoCache,                 // Inherit NoCache setting
				Target:  service.Build.Target,                     // Inherit Target setting
				Pull:    spec.BuildConfig.Pull,                    // Inherit Pull setting
				Tags:    []string{fmt.Sprintf("%s:latest", Name)}, // Default tag for the service image
				// Use buildkit setting from main spec?
				BuildKit: spec.BuildConfig.BuildKit,
			},
		}

		// Add build args from main spec first
		for k, v := range spec.BuildConfig.Args {
			serviceSpec.BuildConfig.Args[k] = v
		}
		// Override/add with build args from compose file service.build.args
		if service.Build.Args != nil {
			for k, v := range service.Build.Args {
				// Compose args can be string pointers, handle nil
				if v != nil {
					serviceSpec.BuildConfig.Args[k] = *v
				} else {
					// Handle case where arg is defined but has no value (e.g., ARG name)
					// We might need to resolve these from the environment?
					// For now, let's just skip them or assign an empty string?
					// buildOptions.BuildArgs expects map[string]*string, so nil is possible.
					serviceSpec.BuildConfig.Args[k] = "" // Or handle differently?
				}
			}
		}

		// Build the image for the service
		imageID, logs, err := s.buildSingleImage(ctx, contextPath, fullDockerfilePath, serviceSpec)
		overallLogs.WriteString(fmt.Sprintf("Logs for service %s:\n%s\n", Name, logs))

		if err != nil {
			errMsg := fmt.Sprintf("erreur lors du build du service '%s': %v", Name, err)
			buildErrors = append(buildErrors, errMsg)
			overallLogs.WriteString(errMsg + "\n")
			// Store partial results?
			result.ServiceOutputs[Name] = ServiceOutput{Logs: logs}
			continue // Continue to build other services even if one fails
		}

		imageSize, sizeErr := s.getImageSize(ctx, imageID)
		if sizeErr != nil {
			overallLogs.WriteString(fmt.Sprintf("Warning: could not get size for image %s (service %s): %v\n", imageID, Name, sizeErr))
		}

		// Store results for this service
		result.ImageIDs[Name] = imageID
		result.ImageSizes[Name] = imageSize
		result.ServiceOutputs[Name] = ServiceOutput{
			ImageID:   imageID,
			ImageSize: imageSize,
			Logs:      logs,
		}
		overallLogs.WriteString(fmt.Sprintf("Service '%s' built successfully. ImageID: %s, Size: %d\n", Name, imageID, imageSize))
		overallLogs.WriteString(fmt.Sprintf("--- Finished Service: %s ---\n", Name))

	} // End loop over services

	return buildErrors
}

// pullImage pulls a Docker image if it doesn't exist locally
func (s *BuildService) pullImage(ctx context.Context, imageName string, logs io.Writer) error {
	// Check if image exists locally first to avoid unnecessary pulls
	_, _, err := s.dockerClient.ImageInspectWithRaw(ctx, imageName)
	if err == nil {
		fmt.Fprintf(logs, "Image '%s' already exists locally.\n", imageName)
		return nil // Image found
	}
	if !client.IsErrNotFound(err) {
		// Different error during inspection
		return fmt.Errorf("erreur lors de l'inspection de l'image '%s' avant pull: %w", imageName, err)
	}

	// Image not found, proceed to pull
	fmt.Fprintf(logs, "Pulling image '%s'...\n", imageName)
	reader, err := s.dockerClient.ImagePull(ctx, imageName, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("erreur lors du lancement du pull de l'image '%s': %w", imageName, err)
	}
	defer reader.Close()

	// Write pull progress to logs
	termFd, isTerm := term.GetFdInfo(logs) // Check if logs is a terminal for progress bars
	err = jsonmessage.DisplayJSONMessagesStream(reader, logs, termFd, isTerm, nil)
	if err != nil {
		return fmt.Errorf("erreur lors de la lecture du flux de pull pour l'image '%s': %w", imageName, err)
	}

	fmt.Fprintf(logs, "Image '%s' pulled successfully.\n", imageName)
	return nil
}

// getImageSize récupère la taille d'une image Docker
func (s *BuildService) getImageSize(ctx context.Context, imageID string) (int64, error) {
	// Use the image ID (which should be sha256 or short ID) for inspection
	summary, _, err := s.dockerClient.ImageInspectWithRaw(ctx, imageID)
	if err != nil {
		return 0, fmt.Errorf("erreur d'inspection de l'image '%s': %w", imageID, err)
	}
	return summary.Size, nil
}

// getImageInfoByTag récupère les infos d'une image par son tag
func (s *BuildService) getImageInfoByTag(ctx context.Context, imageTag string) (*types.ImageInspect, error) {
	summary, _, err := s.dockerClient.ImageInspectWithRaw(ctx, imageTag)
	if err != nil {
		return nil, fmt.Errorf("erreur d'inspection de l'image taggée '%s': %w", imageTag, err)
	}
	return &summary, nil
}

// saveImageLocally sauvegarde une image Docker dans un fichier .tar local
func (s *BuildService) saveImageLocally(ctx context.Context, imageID string, targetPath string) error {
	reader, err := s.dockerClient.ImageSave(ctx, []string{imageID})
	if err != nil {
		return fmt.Errorf("erreur lors de l'export de l'image '%s': %w", imageID, err)
	}
	defer reader.Close()

	file, err := os.Create(targetPath)
	if err != nil {
		return fmt.Errorf("impossible de créer le fichier image local '%s': %w", targetPath, err)
	}
	defer file.Close()

	_, err = io.Copy(file, reader)
	if err != nil {
		return fmt.Errorf("erreur lors de l'écriture dans le fichier image local '%s': %w", targetPath, err)
	}

	return nil
}

// exportAndUploadImage exporte une image Docker et l'upload vers B2 (modifié pour nom/version/tags)
func (s *BuildService) exportAndUploadImage(ctx context.Context, imageID, serviceName, version string, tags []string) ([]string, error) {
	if s.b2Config == nil {
		return nil, fmt.Errorf("configuration B2 non définie pour upload")
	}

	// Créer un reader pour l'image exportée
	reader, err := s.dockerClient.ImageSave(ctx, []string{imageID}) // Use the actual image ID
	if err != nil {
		return nil, fmt.Errorf("erreur lors de l'export de l'image ID '%s': %w", imageID, err)
	}
	defer reader.Close()

	// Utiliser io.Pipe pour streamer directement vers B2 sans charger en mémoire (plus efficace pour grosses images)
	pr, pw := io.Pipe()

	var uploadErr error
	var wg sync.WaitGroup
	wg.Add(1)

	// Goroutine pour uploader depuis le pipe reader
	go func() {
		defer wg.Done()
		defer pr.Close() // Fermer le reader quand l'upload est fini ou échoue

		b2Client, err := b2.NewClient(context.WithoutCancel(ctx), s.b2Config.AccountID, s.b2Config.ApplicationKey, b2.UserAgent("build-service")) // Use context without timeout for upload potentially
		if err != nil {
			uploadErr = fmt.Errorf("erreur lors de l'initialisation du client B2: %w", err)
			return
		}

		bucket, err := b2Client.Bucket(ctx, s.b2Config.BucketName)
		if err != nil {
			uploadErr = fmt.Errorf("erreur d'accès au bucket B2 '%s': %w", s.b2Config.BucketName, err)
			return
		}

		// Nom d'objet principal basé sur service et version
		imageName := fmt.Sprintf("%s-%s.tar", serviceName, version)
		objectPath := filepath.Join(s.b2Config.BasePath, imageName)

		obj := bucket.Object(objectPath)
		writer := obj.NewWriter(ctx)

		fmt.Printf("Starting B2 upload to %s...\n", objectPath) // Log start
		_, err = io.Copy(writer, pr)                            // Lire depuis le pipe et écrire vers B2
		if err != nil {
			writer.Close() // Important to close writer even on error
			uploadErr = fmt.Errorf("erreur lors de l'écriture stream vers B2 (%s): %w", objectPath, err)
			return
		}

		err = writer.Close() // Finaliser l'upload
		if err != nil {
			uploadErr = fmt.Errorf("erreur lors de la finalisation de l'upload B2 (%s): %w", objectPath, err)
			return
		}
		fmt.Printf("Finished B2 upload to %s.\n", objectPath) // Log success
		// Upload successful for the main object path
	}()

	// Goroutine pour copier depuis Docker save vers le pipe writer
	var copyErr error
	go func() {
		defer pw.Close() // Fermer le writer quand la copie est finie ou échoue
		_, copyErr = io.Copy(pw, reader)
	}()

	// Attendre la fin de l'upload
	wg.Wait()

	// Vérifier les erreurs
	if copyErr != nil {
		return nil, fmt.Errorf("erreur lors de la lecture des données de l'image Docker: %w", copyErr)
	}
	if uploadErr != nil {
		return nil, fmt.Errorf("erreur lors de l'upload vers B2: %w", uploadErr)
	}

	// L'upload principal a réussi. Maintenant, gérer les tags comme des références (petits fichiers texte).
	// Note: B2 ne supporte pas les liens symboliques directs. On crée des fichiers de ref.
	objectNames := []string{filepath.Join(s.b2Config.BasePath, fmt.Sprintf("%s-%s.tar", serviceName, version))} // Start with the main path

	// Re-init client/bucket for tag uploads (ou réutiliser si possible)
	b2Client, err := b2.NewClient(ctx, s.b2Config.AccountID, s.b2Config.ApplicationKey, b2.UserAgent("build-service"))
	if err != nil {
		// Log error mais on a déjà réussi l'upload principal
		fmt.Printf("Warning: Failed to re-init B2 client for tag refs: %v\n", err)
		return objectNames, nil // Return only the main object name
	}
	bucket, err := b2Client.Bucket(ctx, s.b2Config.BucketName)
	if err != nil {
		fmt.Printf("Warning: Failed to get B2 bucket for tag refs: %v\n", err)
		return objectNames, nil
	}

	for _, tag := range tags {
		cleanTag := strings.ReplaceAll(tag, ":", "-")
		cleanTag = strings.ReplaceAll(cleanTag, "/", "_") // Replace slashes too
		tagFileName := fmt.Sprintf("%s.ref.txt", cleanTag)
		tagPath := filepath.Join(s.b2Config.BasePath, tagFileName)

		refContent := fmt.Sprintf("ImageID: %s\nTag: %s\nVersion: %s\nServiceName: %s\nMainObject: %s\n",
			imageID, tag, version, serviceName, objectNames[0])

		refObj := bucket.Object(tagPath)
		refWriter := refObj.NewWriter(ctx)

		_, err = refWriter.Write([]byte(refContent))
		if err != nil {
			refWriter.Close()
			fmt.Printf("Warning: Failed to write B2 ref file for tag '%s' (%s): %v\n", tag, tagPath, err)
			continue // Continue with other tags
		}
		err = refWriter.Close()
		if err != nil {
			fmt.Printf("Warning: Failed to close B2 ref file for tag '%s' (%s): %v\n", tag, tagPath, err)
			continue
		}
		objectNames = append(objectNames, tagPath)
	}

	return objectNames, nil
}

// extractFromContainer copie un fichier/dossier depuis un conteneur temporaire
func (s *BuildService) extractFromContainer(ctx context.Context, imageID, containerPath string) ([]byte, error) {
	// Créer un conteneur temporaire basé sur l'image
	resp, err := s.dockerClient.ContainerCreate(ctx, &container.Config{Image: imageID}, nil, nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("erreur lors de la création du conteneur temporaire pour l'extraction: %w", err)
	}
	containerID := resp.ID
	defer s.dockerClient.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true}) // Cleanup

	// Copier le fichier/dossier depuis le conteneur
	readCloser, _, err := s.dockerClient.CopyFromContainer(ctx, containerID, containerPath)
	if err != nil {
		return nil, fmt.Errorf("erreur lors de la copie depuis le conteneur '%s' (path: %s): %w", containerID, containerPath, err)
	}
	defer readCloser.Close()

	// Lire le contenu de l'archive tar retournée par CopyFromContainer
	tarReader := tar.NewReader(readCloser)

	// On s'attend à un seul fichier (ou le premier fichier si c'est un dossier)
	// Pour extraire un dossier complet, il faudrait itérer et recréer la structure
	header, err := tarReader.Next()
	if err == io.EOF {
		return nil, fmt.Errorf("aucune donnée trouvée dans l'archive copiée depuis le conteneur (path: %s)", containerPath)
	}
	if err != nil {
		return nil, fmt.Errorf("erreur lors de la lecture de l'en-tête tar depuis la copie du conteneur: %w", err)
	}

	// Vérifier si c'est un fichier régulier
	if header.Typeflag != tar.TypeReg {
		// Si c'est un dossier, on pourrait vouloir lire le premier fichier ou échouer?
		// Pour un binaire, on s'attend à un fichier.
		// Tentative : si c'est un dossier, lire le contenu du premier fichier trouvé dedans?
		// Ou simplement retourner une erreur si ce n'est pas un fichier régulier.
		return nil, fmt.Errorf("le chemin extrait '%s' n'est pas un fichier régulier (type: %c)", containerPath, header.Typeflag)
	}

	// Lire le contenu du fichier
	fileData, err := io.ReadAll(tarReader)
	if err != nil {
		return nil, fmt.Errorf("erreur lors de la lecture du contenu du fichier depuis l'archive tar: %w", err)
	}

	return fileData, nil
}

// generateRunYAML crée la structure pour *.run.yml (CORRIGÉ pour accepter projet parsé)
func (s *BuildService) generateRunYAML(ctx context.Context, spec *BuildSpec, result *BuildResult, runtimeEnv map[string]string, finalImageTags map[string][]string, composeProject *ComposeProject) (*RunYAML, error) { // Modifié: Prend *ComposeProject
	runYAML := &RunYAML{
		Version:  "1.0",
		Services: make(map[string]RunService),
	}

	if composeProject != nil { // Utiliser le projet parsé si fourni
		// Base run.yml on the parsed compose file structure
		for serviceName, service := range composeProject.Services {
			// Skip build-only services? (Nécessite une logique/annotation pour identifier)
			// Pour l'instant, on inclut tous les services définis.
			// isBuildOnly := false // Logique à ajouter si nécessaire
			// if isBuildOnly { continue }

			runService := RunService{
				Image:       s.getImageRefForRun(serviceName, spec.RunConfigDef.ArtifactStorage, result, finalImageTags),
				Command:     service.Command,
				Entrypoint:  service.Entrypoint,
				Environment: make(map[string]string),
				Ports:       service.Ports,   // Directement []string maintenant
				Volumes:     service.Volumes, // Directement []string maintenant
				Restart:     service.Restart,
				DependsOn:   service.DependsOn, // Directement []string maintenant
			}

			// Combine env vars: Global runtime env puis Service-specific
			for k, v := range runtimeEnv {
				runService.Environment[k] = v
			}
			if service.Environment != nil {
				for k, vPtr := range service.Environment {
					if vPtr != nil {
						// NOTE: Pas d'interpolation ici ! Les valeurs sont littérales.
						runService.Environment[k] = *vPtr
					} else {
						// Variable définie sans valeur (ex: FOO:) -> essayer l'env host? Mettre vide?
						// Mettons vide pour l'instant pour la simplicité.
						runService.Environment[k] = ""
					}
				}
			}
			// Copier d'autres champs si définis dans RunService (ex: HealthCheck, Labels)
			// runService.HealthCheck = service.HealthCheck // Si RunService a un HealthCheck

			runYAML.Services[serviceName] = runService
		}

	} else {
		// Single service based on the main build spec name (non-compose build)
		mainServiceName := spec.Name
		// Vérifier si cette image existe (au cas où le build a échoué mais on génère quand même)
		if _, ok := result.ImageIDs[mainServiceName]; !ok && spec.RunConfigDef.ArtifactStorage != "local" {
			fmt.Printf("Warning: Image for main service '%s' not found in results, skipping run.yml generation for it.\n", mainServiceName)
			// Retourner un run.yml vide ou une erreur? Retournons le runYAML potentiellement vide.
		} else {
			runService := RunService{
				Image:       s.getImageRefForRun(mainServiceName, spec.RunConfigDef.ArtifactStorage, result, finalImageTags),
				Environment: runtimeEnv,
				Command:     spec.RunConfigDef.Commands, // Utiliser les commandes globales définies
				// Ajouter d'autres champs par défaut si nécessaire
			}
			runYAML.Services[mainServiceName] = runService
		}
	}

	// Vérifier si aucun service n'a été ajouté (peut arriver si build compose échoue complètement)
	if len(runYAML.Services) == 0 {
		fmt.Println("Warning: No services could be added to run.yml.")
		// Retourner une erreur ou un succès avec un fichier vide ? Succès vide pour l'instant.
	}

	return runYAML, nil
}

// getImageRefForRun détermine la référence d'image à utiliser dans run.yml
func (s *BuildService) getImageRefForRun(serviceName, storageType string, result *BuildResult, finalImageTags map[string][]string) string {
	switch storageType {
	case "local":
		if path, ok := result.LocalImagePaths[serviceName]; ok && path != "" {
			// Retourner seulement le nom du fichier .tar
			return filepath.Base(path)
		}
		// Fallback si chemin non trouvé
		fmt.Printf("Warning: Local image path not found for service '%s' in build result.\n", serviceName)
		return fmt.Sprintf("local:%s_image_not_found.tar", serviceName)

	case "docker":
		// Utiliser le premier tag trouvé pour ce service
		if tags, ok := finalImageTags[serviceName]; ok && len(tags) > 0 && tags[0] != "" {
			return tags[0] // Utilise le premier tag appliqué
		}
		// Fallback si aucun tag trouvé (ne devrait pas arriver si build a réussi et taggé)
		fmt.Printf("Warning: No Docker tags found for service '%s' in finalImageTags map.\n", serviceName)
		// En dernier recours, utiliser l'ID si disponible ? Ou un tag par défaut ? Utilisons un tag par défaut.
		if result.ImageIDs != nil {
			if imgID, ok := result.ImageIDs[serviceName]; ok && imgID != "" {
				fmt.Printf("Warning: Falling back to default tag for service '%s' as no specific tags were found.\n", serviceName)
				// Construire un tag par défaut plausible (peut nécessiter le nom du projet)
				// Ceci est un fallback, la logique de tagging dans Build() devrait être la source principale.
				return fmt.Sprintf("%s:latest", serviceName) // Simple fallback
			}
		}
		return fmt.Sprintf("docker:%s_image_or_tag_not_found", serviceName)

	default: // Cas inconnu ou ""
		fmt.Printf("Warning: Unknown artifact storage type '%s'. Falling back to default behavior.\n", storageType)
		// Comportement par défaut : essayer tag docker, puis id, puis fallback
		if tags, ok := finalImageTags[serviceName]; ok && len(tags) > 0 && tags[0] != "" {
			return tags[0]
		}
		if result.ImageIDs != nil {
			if imgID, ok := result.ImageIDs[serviceName]; ok && imgID != "" {
				return imgID // Retourne l'ID si pas de tag
			}
		}
		return fmt.Sprintf("unknown_storage:%s_not_found", serviceName)
	}
}

// --- Other Existing Functions (Potentially useful, keep for now) ---
// CreateContainer, RunContainer, ExportContainer, Cleanup, UploadArtifactToB2,
// DownloadImageFromB2, BuildWithMultipleCodebases, SaveDockerImageToBuffer,
// CreateDockerfileFromMultipleCodebases, BuildWithBufferInput, GetImageInfo,
// TagImage, PushImage, CompressAndUploadArtifacts, ExecuteInContainer,
// GenerateMultistageDockerfile, SetupDockerIgnore, GenerateImageReport

// Note: Some functions like BuildWithMultipleCodebases might be redundant now
// that the main Build function handles codebases properly. Review and refactor/remove later.
// ExecuteInContainer might be useful for BuildSteps binary extraction, but needs refinement.
// Added import for "github.com/docker/docker/pkg/term" for pull progress

// Placeholder pour l'implémentation du SecretFetcher si non fourni
type DummySecretFetcher struct{}

func (d *DummySecretFetcher) GetSecret(ctx context.Context, source string) (string, error) {
	fmt.Printf("Warning: Using DummySecretFetcher. Secret '%s' not actually fetched.\n", source)
	// Retourner une valeur placeholder ou une erreur selon le comportement souhaité
	return fmt.Sprintf("dummy-secret-for-%s", source), nil
	// Ou: return "", fmt.Errorf("secret fetcher not implemented")
}
