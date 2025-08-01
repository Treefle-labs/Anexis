package build

import (
	"sync"

	"github.com/docker/docker/client"
)

// --- Struct Definitions ---

// BuildSpec is the specification structure parsing from the spec file
// This is the extended config for the build process
type BuildSpec struct {
	Name         string            `json:"name" yaml:"name"`                                         // The Name used for the service
	Version      string            `json:"version" yaml:"version"`                                   // The version of the software can use a semver specification
	Codebases    []CodebaseConfig  `json:"codebases" yaml:"codebases"`                               // The list of the different codebases. It can be provided by git or local or tar/zip archive
	Resources    []ResourceConfig  `json:"resources,omitempty" yaml:"resources,omitempty"`           // A list of the resources to include in build process
	BuildSteps   []BuildStep       `json:"build_steps,omitempty" yaml:"build_steps,omitempty"`       // Specify the different build step. Useful for including a binary dependency in any codebase build
	BuildConfig  BuildConfig       `json:"build_config" yaml:"build_config"`                         // The build Build configuration struct
	Env          map[string]string `json:"env,omitempty" yaml:"env,omitempty"`                       // Specify the Environment variables
	EnvFiles     []string          `json:"env_files,omitempty" yaml:"env_files,omitempty"`           // Used to load the Envs from the provided file path
	Secrets      []SecretSpec      `json:"secrets,omitempty" yaml:"secrets,omitempty"`               // Secrets specifications. Secrets is like env vars but it's provided by a specific service and encrypted/decrypted during the usage. Use this to pass very sensible information to your different services
	RunConfigDef RunConfigDef      `json:"run_config_def,omitempty" yaml:"run_config_def,omitempty"` // Configuration for the *.run.yml file. This file is used by the CLI to run your different services
}

// Representation of any codebase in the services
type CodebaseConfig struct {
	Name         string `json:"name" yaml:"name"`                                         // Specify the name of the codebase
	SourceType   string `json:"source_type" yaml:"source_type"`                           // git, local, archive, buffer
	Source       string `json:"source" yaml:"source"`                                     // URL, local path
	Branch       string `json:"branch,omitempty" yaml:"branch,omitempty"`                 // The git branch to build
	Commit       string `json:"commit,omitempty" yaml:"commit,omitempty"`                 // The specific commit to consider during the codebase pulling if the source is git
	Path         string `json:"path,omitempty" yaml:"path,omitempty"`                     // The path of the codebase in the local dir
	Content      []byte `json:"-" yaml:"-"`                                               // The memory content if the source type is buffer
	BuildOnly    bool   `json:"build_only,omitempty" yaml:"build_only,omitempty"`         // If specified the codebase is only builded
	TargetInHost string `json:"target_in_host,omitempty" yaml:"target_in_host,omitempty"` // Path to put the codebase in the host dir
}

// ResourceConfig is resource representation to download during the build
type ResourceConfig struct {
	URL        string `json:"url" yaml:"url"`                             // The resource URL
	TargetPath string `json:"target_path" yaml:"target_path"`             // relative path destination in the build dir
	Extract    bool   `json:"extract,omitempty" yaml:"extract,omitempty"` // Extract the archive (tar, tgz, zip)
}

// BuildStep is a build sequenced step, potentially with dependencies
type BuildStep struct {
	Name              string `json:"name" yaml:"name"`                                                     // The step name
	CodebaseName      string `json:"codebase_name" yaml:"codebase_name"`                                   // References a codebase name to use for this step
	OutputsBinaryPath string `json:"outputs_binary_path,omitempty" yaml:"outputs_binary_path,omitempty"`   // Path in the *container* of the binary to extract
	UseBinaryFromStep string `json:"use_binary_from_step,omitempty" yaml:"use_binary_from_step,omitempty"` // The step in which the binary will be used
	BinaryTargetPath  string `json:"binary_target_path,omitempty" yaml:"binary_target_path,omitempty"`     // The path to put the binary during the specific step
}

// BuildConfig is a Docker build config spec extended
type BuildConfig struct {
	BaseImage    string            `json:"base_image,omitempty" yaml:"base_image,omitempty"`     // The base image to use
	Dockerfile   string            `json:"dockerfile,omitempty" yaml:"dockerfile,omitempty"`     // relative path of the Dockerfile or the inline content
	ComposeFile  string            `json:"compose_file,omitempty" yaml:"compose_file,omitempty"` // the relative compose file path
	Target       string            `json:"target,omitempty" yaml:"target,omitempty"`
	Args         map[string]string `json:"args,omitempty" yaml:"args,omitempty"`             // Ens vars to inject in the build config
	Tags         []string          `json:"tags,omitempty" yaml:"tags,omitempty"`             // Tags for the finale docker image (or the principal image in case of compose)
	Platforms    []string          `json:"platforms,omitempty" yaml:"platforms,omitempty"`   // cross-platform support (experimental)
	NoCache      bool              `json:"no_cache,omitempty" yaml:"no_cache,omitempty"`     // Specify if the cache will be used between the build
	OutputTarget string            `json:"output_target" yaml:"output_target"`               // The storage target "b2", "local", "docker" (by default)
	LocalPath    string            `json:"local_path,omitempty" yaml:"local_path,omitempty"` // Output path if OutputTarget="local"
	Pull         bool              `json:"pull,omitempty" yaml:"pull,omitempty"`             // Trying to pull the based image
	BuildKit     bool              `json:"buildkit,omitempty" yaml:"buildkit,omitempty"`     // Use BuildKit (if available)
}

// SecretSpec define the way to fetch the secrets
type SecretSpec struct {
	Name         string `json:"name" yaml:"name"`                   // The name of the env var that will receive the secret
	Source       string `json:"source" yaml:"source"`               // The service ID for this secret
	InjectMethod string `json:"inject_method" yaml:"inject_method"` // "env" (default), can be file later
}

// RunConfigDef define the parameters for the *.run.yml generation
type RunConfigDef struct {
	Generate        bool     `json:"generate" yaml:"generate"`                     // Is the file will be generated ?
	ArtifactStorage string   `json:"artifact_storage" yaml:"artifact_storage"`     // "docker" (use the tags), "local" (referencing .tar)
	Commands        []string `json:"commands,omitempty" yaml:"commands,omitempty"` // The default commands (overriding if needed)
	// Some other options can be added after...
}

// RunService is any service representation in the *.run.yml
type RunService struct {
	Image       string            `yaml:"image"`                 // The name of the tar local image
	Command     []string          `yaml:"command,omitempty"`     // The command to exec
	Entrypoint  []string          `yaml:"entrypoint,omitempty"`  // The entry point
	Environment map[string]string `yaml:"environment,omitempty"` // Environment variables (include secrets)
	Ports       []string          `yaml:"ports,omitempty"`       // Format "host:container"
	Volumes     []string          `yaml:"volumes,omitempty"`     // Format "host:container" ou "named:container"
	Restart     string            `yaml:"restart,omitempty"`     // Reboot politic (e.g., "always", "on-failure")
	DependsOn   []string          `yaml:"depends_on,omitempty"`  // The depending services
	// Some other fields can be added later...
}

// RunYAML is the struct of the *.run.yml output file. This file is generated after a build and is used by the bx CLI to run your artifact
type RunYAML struct {
	Version  string                `yaml:"version"` // The file version format
	Services map[string]RunService `yaml:"services"`
	// potentially other sections for volumes, networks, etc.
}

// BuildResult is the struct representing a build result of each service
type BuildResult struct {
	Success         bool                     `json:"success"`
	ImageID         string                   `json:"image_id,omitempty"`          // The docker image ID (if applicable)
	ImageIDs        map[string]string        `json:"image_ids,omitempty"`         // Each service IDS (if compose)
	ImageSize       int64                    `json:"image_size,omitempty"`        // The main docker image size
	ImageSizes      map[string]int64         `json:"image_sizes,omitempty"`       // Image size by service
	Artifacts       map[string][]byte        `json:"-"`                           // Memory artefact
	BuildTime       float64                  `json:"build_time"`                  // Total Build time
	ErrorMessage    string                   `json:"error_message,omitempty"`     // Build error message
	Logs            string                   `json:"logs"`                        // Build logs
	B2ObjectNames   []string                 `json:"b2_object_names,omitempty"`   // For OutputTarget="b2"
	LocalImagePaths map[string]string        `json:"local_image_paths,omitempty"` // For OutputTarget="local"
	RunConfigPath   string                   `json:"run_config_path,omitempty"`   // Path to the generated *.run.yml file
	ServiceOutputs  map[string]ServiceOutput `json:"service_outputs,omitempty"`   // Specific information generated by service
}

// ServiceOutput is the specific information for each builded service (e.g., image ID)
type ServiceOutput struct {
	ImageID   string `json:"image_id"`
	ImageSize int64  `json:"image_size"`
	Logs      string `json:"logs"`
}

// B2Config is the b2 storage information struct
type B2Config struct {
	AccountID      string `json:"account_id" yaml:"account_id"`
	ApplicationKey string `json:"application_key" yaml:"application_key"`
	BucketName     string `json:"bucket_name" yaml:"bucket_name"`
	BasePath       string `json:"base_path" yaml:"base_path"`
}

// The Main service to manage each build
type BuildService struct {
	dockerClient  *client.Client
	workDir       string
	b2Config      *B2Config
	mutex         sync.Mutex
	inMemory      bool          // if true minimizing the system disk usage
	secretFetcher SecretFetcher // Interface for secrets fetching
}

type ComposeProject struct {
	Version  string                    `yaml:"version,omitempty"`
	Services map[string]ComposeService `yaml:"services"`
	Name     string
	Volumes  map[string]interface{} `yaml:"volumes,omitempty"`
	Networks map[string]interface{} `yaml:"networks,omitempty"`
}

// A representation of a compose service (simplified)
type ComposeService struct {
	Image           string             `yaml:"image,omitempty"`
	Build           *ComposeBuild      `yaml:"build,omitempty"`
	Command         []string           `yaml:"command,omitempty"`
	Entrypoint      []string           `yaml:"entrypoint,omitempty"`
	Environment     map[string]*string `yaml:"environment,omitempty"`
	Ports           []string           `yaml:"ports,omitempty"`
	Volumes         []string           `yaml:"volumes,omitempty"`
	DependsOn       []string           `yaml:"depends_on,omitempty"`
	Restart         string             `yaml:"restart,omitempty"`
	HealthCheck     *HealthCheck       `yaml:"healthcheck,omitempty"`
	Labels          map[string]string  `yaml:"labels,omitempty"`
	Expose          []string           `yaml:"expose,omitempty"`
	StopGracePeriod string             `yaml:"stop_grace_period,omitempty"`
}

type ComposeBuild struct {
	Context    string
	Dockerfile string
	Args       map[string]*string
	Target     string
	CacheFrom  []string          `yaml:"cache_from,omitempty"`
	Labels     map[string]string `yaml:"labels,omitempty"`
	Network    string            `yaml:"network,omitempty"`
}