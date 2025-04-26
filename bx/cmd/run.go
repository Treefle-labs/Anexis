// cmd/bx/cmd/run.go
package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time" // Pour docker load

	"anexis/bx/build"
	"anexis/socket" // Pour parser RunYAML

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var (
	runFile string
	// servicesToRun []string // Pour exécuter seulement certains services
	// detach bool            // Pour exécuter en arrière-plan

	runCmd = &cobra.Command{
		Use:   "run -f <run.yml>",
		Short: "Lance les services définis dans un fichier .run.yml généré par un build.",
		Long: `Cette commande lit un fichier .run.yml, interprète les définitions de service
et lance les conteneurs correspondants en utilisant la commande 'docker run'.
Elle gère le chargement des images locales si nécessaire.`,
		Args: cobra.NoArgs,
		RunE: runRunCommand,
	}
)

func init() {
	runCmd.Flags().StringVarP(&runFile, "file", "f", "", "Chemin vers le fichier .run.yml (obligatoire)")
	// runCmd.Flags().StringSliceVarP(&servicesToRun, "service", "", []string{}, "Spécifier les services à lancer (défaut: tous)")
	// runCmd.Flags().BoolVarP(&detach, "detach", "d", false, "Lancer les conteneurs en arrière-plan (détaché)")
	runCmd.MarkFlagRequired("file")
}

func runRunCommand(cmd *cobra.Command, args []string) error {
	if runFile == "" {
		return fmt.Errorf("le flag --file (-f) est obligatoire")
	}
	if _, err := os.Stat(runFile); os.IsNotExist(err) {
		return fmt.Errorf("le fichier .run.yml '%s' n'existe pas", runFile)
	}

	// 1. Lire et parser le fichier .run.yml
	runData, err := os.ReadFile(runFile)
	if err != nil {
		return fmt.Errorf("erreur lors de la lecture de '%s': %w", runFile, err)
	}

	var runConfig build.Bui
	err = yaml.Unmarshal(runData, &runConfig)
	if err != nil {
		return fmt.Errorf("erreur lors du parsing YAML de '%s': %w", runFile, err)
	}

	if len(runConfig.Services) == 0 {
		fmt.Println("Aucun service défini dans", runFile)
		return nil
	}

	fmt.Printf("Lancement des services depuis '%s'...\n", runFile)
	runFileDir := filepath.Dir(runFile) // Répertoire où se trouve le run.yml (pour les paths relatifs des .tar)

	// 2. Itérer et lancer chaque service
	// TODO: Gérer l'ordre basé sur depends_on si nécessaire (complexe avec docker run)
	for serviceName, service := range runConfig.Services {
		fmt.Printf("--- Lancement du service: %s ---\n", serviceName)

		// Construire la commande docker run
		dockerArgs := []string{"run"}

		// Détaché ?
		// if detach { dockerArgs = append(dockerArgs, "-d") } else {
		// Pour la simplicité, on ajoute --rm pour nettoyer après arrêt foreground
		dockerArgs = append(dockerArgs, "--rm")
		// }
		// Ajouter -it pour interactivité si pas détaché ? Peut causer problèmes.
		// dockerArgs = append(dockerArgs, "-it")

		// Nom du conteneur (basé sur service)
		containerName := fmt.Sprintf("bx_run_%s_%d", serviceName, time.Now().UnixNano())
		dockerArgs = append(dockerArgs, "--name", containerName)

		// Politique de redémarrage
		if service.Restart != "" {
			dockerArgs = append(dockerArgs, "--restart", service.Restart)
		}

		// Variables d'environnement
		for key, val := range service.Environment {
			dockerArgs = append(dockerArgs, "-e", fmt.Sprintf("%s=%s", key, val))
		}

		// Ports
		for _, portMapping := range service.Ports {
			dockerArgs = append(dockerArgs, "-p", portMapping)
		}

		// Volumes
		for _, volumeMapping := range service.Volumes {
			// Attention: Interpréter les chemins relatifs pour les bind mounts
			parts := strings.SplitN(volumeMapping, ":", 2)
			if len(parts) == 2 && !filepath.IsAbs(parts[0]) && !strings.Contains(parts[0], "/") {
				// Probablement un volume nommé, laisser tel quel
				dockerArgs = append(dockerArgs, "-v", volumeMapping)
			} else if len(parts) >= 2 && !filepath.IsAbs(parts[0]) {
				// Chemin hôte relatif -> le rendre absolu par rapport à ?? CWD? run.yml dir?
				// Soyons prudents, n'autorisons que les chemins absolus ou volumes nommés pour l'instant
				fmt.Printf("WARN: Le chemin hôte relatif '%s' dans le volume mapping n'est pas supporté. Utilisez un chemin absolu ou un volume nommé.\n", parts[0])
				// dockerArgs = append(dockerArgs, "-v", volumeMapping) // Ou skipper ?
			} else {
				dockerArgs = append(dockerArgs, "-v", volumeMapping) // Volume nommé ou chemin absolu
			}
		}

		// Image
		imageRef := service.Image
		if strings.HasSuffix(imageRef, ".tar") {
			// Assumer que c'est un fichier .tar local relatif au .run.yml
			tarPath := imageRef
			if !filepath.IsAbs(tarPath) {
				tarPath = filepath.Join(runFileDir, tarPath)
			}
			fmt.Printf("Chargement de l'image depuis l'archive locale: %s\n", tarPath)
			if _, err := os.Stat(tarPath); os.IsNotExist(err) {
				return fmt.Errorf("l'archive image '%s' pour le service '%s' n'existe pas", tarPath, serviceName)
			}

			loadCmd := exec.Command("docker", "load", "-i", tarPath)
			loadCmd.Stdout = os.Stdout
			loadCmd.Stderr = os.Stderr
			if err := loadCmd.Run(); err != nil {
				return fmt.Errorf("erreur lors du chargement de l'image depuis '%s': %w", tarPath, err)
			}
			// Comment obtenir le tag/ID chargé ? docker load l'affiche. C'est compliqué.
			// On suppose que le tar contient une image tagguée de manière prévisible.
			// => Il FAUT que le build.go (lorsqu'il sauve en local) taggue l'image avant de la sauver.
			// => Le run.yml doit référencer ce TAG, pas le .tar.
			// ---> REVISION NECESSAIRE de la génération du run.yml pour storage "local" !
			// Pour l'instant, on va supposer que le .tar contient l'image service.Image (sans le .tar)
			// Ceci est une GROSSE supposition.
			imageRef = strings.TrimSuffix(service.Image, ".tar") // Suppose que le tag est le nom du fichier sans .tar
			fmt.Printf("Supposition : l'image chargée devrait être tagguée comme '%s'\n", imageRef)

		} else if strings.HasPrefix(imageRef, "local:") {
			// Gérer l'autre cas de fallback de getImageRefForRun
			return fmt.Errorf("référence d'image locale non trouvée '%s' pour le service '%s'", imageRef, serviceName)
		}
		dockerArgs = append(dockerArgs, imageRef) // Ajouter l'image (tag ou ID)

		// Entrypoint / Command
		if len(service.Entrypoint) > 0 {
			dockerArgs = append(dockerArgs, "--entrypoint", service.Entrypoint[0]) // docker run prend seulement le premier
			// Ajouter les arguments d'entrypoint après l'image
			//dockerArgs = append(dockerArgs, service.Entrypoint[1:]...) // Non, ça c'est la commande
		}
		if len(service.Command) > 0 {
			// La commande vient après l'image (et après les args d'entrypoint s'il y en a)
			dockerArgs = append(dockerArgs, service.Command...)
		}

		// Exécuter la commande docker run
		fmt.Printf("Exécution: docker %s\n", strings.Join(dockerArgs, " "))
		runCmd := exec.CommandContext(context.Background(), "docker", dockerArgs...) // Utiliser un contexte ?
		runCmd.Stdout = os.Stdout
		runCmd.Stderr = os.Stderr
		// runCmd.Stdin = os.Stdin // Pour interactivité ?

		err = runCmd.Run() // Bloque jusqu'à la fin du conteneur (car pas -d)
		if err != nil {
			// Si le conteneur s'arrête avec un code non-nul, Run() retourne une erreur
			fmt.Printf("Erreur lors de l'exécution du service '%s': %v\n", serviceName, err)
			// Faut-il arrêter les autres services ? Pour l'instant, on continue.
			// return fmt.Errorf("le service '%s' a échoué: %w", serviceName, err) // Arrêter tout
		} else {
			fmt.Printf("--- Service '%s' terminé ---\n", serviceName)
		}
		fmt.Println() // Ligne vide entre les services
	}

	fmt.Println("Tous les services ont été lancés.")
	return nil
}