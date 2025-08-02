package build

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

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
