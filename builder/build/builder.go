package build

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"syscall"

	"github.com/evanw/esbuild/pkg/api"
)

// Créer un plugin pour modifier les imports
func createImportPlugin() api.Plugin {
	return api.Plugin{
		Name: "import-extension",
		Setup: func(build api.PluginBuild) {
			build.OnLoad(api.OnLoadOptions{
				Filter: `\.ts$`, // Cibler uniquement les fichiers TypeScript
			}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
				// Lire le contenu du fichier actuel
				content, err := os.ReadFile(args.Path)
				if err != nil {
					return api.OnLoadResult{}, err
				}

				// Modifier les imports relatifs pour ajouter ".js"
				updatedContent := regexp.MustCompile(`from\s+(['"])(\.{1,2}/[^'"]*)(['"])`).
					ReplaceAllString(string(content), `from $1$2.js$1`)

				return api.OnLoadResult{
					Contents:   &updatedContent,         // Contenu modifié
					Loader:     api.LoaderTS,            // Garder le loader TypeScript
					ResolveDir: filepath.Dir(args.Path), // Répertoire pour la résolution
				}, nil
			})
		},
	}
}

func buildTSFile(inputPath string) error {
	outputPath := filepath.Join(
		"./client/js",
		fmt.Sprintf("%s.js", inputPath[:len(inputPath)-3]),
	)

	result := api.Build(api.BuildOptions{
		EntryPoints: []string{inputPath},
		Bundle:      false,
		Write:       true,
		Outfile:     outputPath,
		Format:      api.FormatESModule,
		Target:      api.ES2015,
		Sourcemap:   api.SourceMapLinked,
		Platform:    api.PlatformBrowser,
		Loader: map[string]api.Loader{
			".ts": api.LoaderTS, // Indiquer à esbuild de gérer les fichiers TypeScript
		},
		Plugins: []api.Plugin{createImportPlugin()},
	})

	if len(result.Errors) > 0 {
		return fmt.Errorf("build error: %v", result.Errors)
	}

	return nil
}

func BuildAllTSFiles(sourceDir string) error {
	return filepath.Walk(sourceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if filepath.Ext(path) == ".ts" {
			if err := buildTSFile(path); err != nil {
				return err
			}
		}
		return nil
	})
}

// Fonction pour surveiller les fichiers TypeScript dans un dossier
func WatchTSFiles(sourceDir string) error {
	// Utiliser doublestar.Glob pour trouver tous les fichiers TypeScript dans le dossier source
	var files []string

	err := filepath.WalkDir(sourceDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if filepath.Ext(path) == ".ts" {
			files = append(files, path)
		}
		return nil
	})
	fmt.Println("Fichiers trouvés :", files)
	if err != nil {
		log.Fatalf("Erreur lors du parcours des fichiers : %v", err)
	}
	if err != nil {
		return fmt.Errorf("failed to retrieve files: %v", err)
	}
	if len(files) == 0 {
		return fmt.Errorf("no TypeScript files found in directory: %s", sourceDir)
	}

	// Créer le contexte de build avec esbuild
	ctx, err2 := api.Context(api.BuildOptions{
		EntryPoints: files,
		Bundle:      false,
		Write:       true,
		Format:      api.FormatESModule,
		Target:      api.ES2015,
		Sourcemap:   api.SourceMapLinked,
		Outdir:      "./client/js",
		Platform:    api.PlatformBrowser,
		Plugins:     []api.Plugin{createImportPlugin()},
		Loader: map[string]api.Loader{
			".ts": api.LoaderTS, // Indiquer à esbuild de gérer les fichiers TypeScript
		},
		ResolveExtensions: []string{".ts", ".js"},
	})
	if err2 != nil {
		return fmt.Errorf("failed to create build context: %v", err)
	}
	defer ctx.Dispose()

	// Activer le mode surveillance
	err = ctx.Watch(api.WatchOptions{})
	if err != nil {
		return fmt.Errorf("failed to start watch mode: %v", err)
	}

	// Gestion des signaux pour arrêter proprement la surveillance
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	return nil
}
