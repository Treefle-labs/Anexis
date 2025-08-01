package build

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrAmbiguousEcosystem = errors.New("multiple incompatible major ecosystems detected (e.g., Go and Rust). Cannot auto-resolve")
	ErrNoEcosystemFound   = errors.New("no supported ecosystem found (e.g., go.mod, package.json, Cargo.toml) at project root")
	ErrNoTemplateFound    = errors.New("no Dockerfile template found for the detected ecosystem")
)

// DetectedEcosystem holds language/ecosystem detection details
// Compatible with extensible language addition.
type DetectedEcosystem struct {
	Language       string
	Ecosystem      string
	PackageManager string
	RootPath       string
	MainMarkerFile string
}

type detectionCandidate struct {
	ecosystem DetectedEcosystem
	priority  int
}

// DetectEcosystem returns the main detected ecosystem in a project directory
func DetectEcosystem(codebasePath string) (*DetectedEcosystem, error) {
	absPath, err := filepath.Abs(codebasePath)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve absolute path for %s: %w", codebasePath, err)
	}

	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("codebase path %s does not exist: %w", absPath, err)
		}
		return nil, fmt.Errorf("error checking path %s: %w", absPath, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("codebase path %s is not a directory", absPath)
	}

	primaryMarkers := loadPrimaryMarkers()
	secondaryMarkers := loadSecondaryMarkers()

	entries, err := os.ReadDir(absPath)
	if err != nil {
		return nil, fmt.Errorf("cannot read directory %s: %w", absPath, err)
	}

	detected, err := scanMarkers(absPath, entries, primaryMarkers)
	if err != nil {
		return nil, err
	}

	postDetectionTweaks(absPath, entries, detected, secondaryMarkers)
	fmt.Printf("Detected ecosystem: %s (%s) using %s in %s\n", detected.Language, detected.Ecosystem, detected.PackageManager, detected.RootPath)
	return detected, nil
}

func loadPrimaryMarkers() map[string]detectionCandidate {
	return map[string]detectionCandidate{
		"go.work":              {DetectedEcosystem{"Go", "Workspaces", "go", "", ""}, 10},
		"go.mod":               {DetectedEcosystem{"Go", "Modules", "go", "", ""}, 9},
		"Cargo.toml":           {DetectedEcosystem{"Rust", "Cargo", "cargo", "", ""}, 9},
		"package.json":         {DetectedEcosystem{"JavaScript", "Node", "npm", "", ""}, 8},
		"pom.xml":              {DetectedEcosystem{"Java", "Maven", "mvn", "", ""}, 9},
		"build.gradle":         {DetectedEcosystem{"Java", "Gradle", "gradle", "", ""}, 9},
		"build.gradle.kts":     {DetectedEcosystem{"Java", "Gradle", "gradle", "", ""}, 9},
		"requirements.txt":     {DetectedEcosystem{"Python", "Pip", "pip", "", ""}, 8},
		"pyproject.toml":       {DetectedEcosystem{"Python", "Poetry/Pip", "pip", "", ""}, 9},
		"composer.json":        {DetectedEcosystem{"PHP", "Composer", "composer", "", ""}, 9},
		"Gemfile":              {DetectedEcosystem{"Ruby", "Bundler", "bundle", "", ""}, 9},
		"*.csproj":             {DetectedEcosystem{"C#", "MSBuild", "dotnet", "", ""}, 9},
		"Package.swift":        {DetectedEcosystem{"Swift", "SwiftPM", "swift", "", ""}, 9},
		"build.gradle.kts.kts": {DetectedEcosystem{"Kotlin", "Gradle", "gradle", "", ""}, 9},
	}
}

func loadSecondaryMarkers() map[string]struct{ PackageManager, Ecosystem string } {
	return map[string]struct{ PackageManager, Ecosystem string }{
		"pnpm-lock.yaml": {"pnpm", "PNPM"},
		"yarn.lock":      {"yarn", "Yarn"},
	}
}

func scanMarkers(path string, entries []os.DirEntry, primary map[string]detectionCandidate) (*DetectedEcosystem, error) {
	highestPriority := -1
	var detected *DetectedEcosystem

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.Contains(name, ".csproj") {
			if candidate, ok := primary["*.csproj"]; ok {
				if detected != nil && detected.Language != candidate.ecosystem.Language {
					return nil, fmt.Errorf("%w: detected %s (%s) and %s (%s)", ErrAmbiguousEcosystem, detected.MainMarkerFile, detected.Language, name, candidate.ecosystem.Language)
				}
				if candidate.priority > highestPriority {
					highestPriority = candidate.priority
					d := candidate.ecosystem
					d.RootPath = path
					d.MainMarkerFile = name
					detected = &d
				}
			}
			continue
		}
		if candidate, ok := primary[name]; ok {
			if detected != nil && detected.Language != candidate.ecosystem.Language {
				return nil, fmt.Errorf("%w: detected %s (%s) and %s (%s)", ErrAmbiguousEcosystem, detected.MainMarkerFile, detected.Language, name, candidate.ecosystem.Language)
			}
			if candidate.priority > highestPriority {
				highestPriority = candidate.priority
				d := candidate.ecosystem
				d.RootPath = path
				d.MainMarkerFile = name
				detected = &d
			}
		}
	}

	if detected == nil {
		return nil, ErrNoEcosystemFound
	}
	return detected, nil
}

func postDetectionTweaks(path string, entries []os.DirEntry, detected *DetectedEcosystem, secondary map[string]struct{ PackageManager, Ecosystem string }) {
	if detected.MainMarkerFile == "package.json" {
		bestLock := -1
		lockPriority := map[string]int{"pnpm-lock.yaml": 2, "yarn.lock": 1}
		for _, entry := range entries {
			name := entry.Name()
			if val, ok := secondary[name]; ok && lockPriority[name] > bestLock {
				bestLock = lockPriority[name]
				detected.PackageManager = val.PackageManager
				detected.Ecosystem = val.Ecosystem
			}
		}
	}

	if detected.MainMarkerFile == "pyproject.toml" {
		data, err := os.ReadFile(filepath.Join(path, "pyproject.toml"))
		if err == nil && strings.Contains(string(data), "[tool.poetry]") {
			detected.Ecosystem = "Poetry"
		}
	}
}
