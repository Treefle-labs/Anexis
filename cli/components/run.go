package components

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Component struct {
	Name        string            `json:"name"`
	InstalledAt string            `json:"installed_at"`
	Options     map[string]string `json:"options,omitempty"`
}

type LockFile struct {
	Components []Component `json:"components"`
}

const lockFilePath = "../../frontend/components.lock.json"
const componentsDir = "../../frontend/components"

// Couleurs terminal
const (
	green = "\033[32m"
	red   = "\033[31m"
	blue  = "\033[34m"
	reset = "\033[0m"
)

// func main() {
// 	if len(os.Args) < 2 {
// 		fmt.Println(blue + "Usage:" + reset + " go run install_components.go [install|add|remove] [component-name(s)]")
// 		return
// 	}

// 	command := os.Args[1]

// 	switch command {
// 	case "install":
// 		installComponents()
// 	case "add":
// 		addComponents(os.Args[2:])
// 	case "remove":
// 		removeComponents(os.Args[2:])
// 	default:
// 		fmt.Println(red + "Commande inconnue:" + reset, command)
// 	}
// }

func loadLockFile() (*LockFile, error) {
	var lock LockFile
	file, err := os.ReadFile(lockFilePath)
	if err != nil {
		return &LockFile{}, nil
	}
	err = json.Unmarshal(file, &lock)
	if err != nil {
		return nil, err
	}
	return &lock, nil
}

func saveLockFile(lock *LockFile) error {
	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(lockFilePath, data, 0644)
}

func isComponentInstalled(name string) bool {
	componentPath := filepath.Join(componentsDir, name)
	_, err := os.Stat(componentPath)
	return !os.IsNotExist(err)
}

func InstallComponents() {
	lock, err := loadLockFile()
	if err != nil {
		fmt.Println(red+"Error during the lockfile reading:"+reset, err)
		return
	}

	for _, comp := range lock.Components {
		if !isComponentInstalled(comp.Name) {
			fmt.Println(blue+"Installation of"+reset, comp.Name, "...")
			err := runShadcnAdd(comp.Name)
			if err != nil {
				fmt.Println(red+"Error during the installation of"+reset, comp.Name, ":", err)
			}
		} else {
			fmt.Println(green + comp.Name + " already installed ✅" + reset)
		}
	}

	fmt.Println(green + "Installation finished." + reset)
}

func AddComponents(names []string) {
	if len(names) == 0 {
		fmt.Println(red + "No component to add." + reset)
		return
	}

	lock, err := loadLockFile()
	if err != nil {
		fmt.Println(red+"Error for the lockfile reading:"+reset, err)
		return
	}

	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}

		// Vérifier si déjà locké
		if componentExists(lock, name) {
			fmt.Println(green + name + " already in the lockfile ✅" + reset)
			continue
		}

		fmt.Println(blue+"Ajout de"+reset, name, "...")

		err := runShadcnAdd(name)
		if err != nil {
			fmt.Println(red+"Erreur ajout de"+reset, name, ":", err)
			continue
		}

		newComponent := Component{
			Name:        name,
			InstalledAt: time.Now().UTC().Format(time.RFC3339),
			Options:     map[string]string{},
		}
		lock.Components = append(lock.Components, newComponent)

		fmt.Println(green + name + " ajouté et installé 🔥" + reset)
	}

	err = saveLockFile(lock)
	if err != nil {
		fmt.Println(red+"Erreur sauvegarde lock file:"+reset, err)
	}
}

func RemoveComponents(names []string) {
	if len(names) == 0 {
		fmt.Println(red + "Aucun composant à supprimer." + reset)
		return
	}

	lock, err := loadLockFile()
	if err != nil {
		fmt.Println(red+"Erreur lecture lock file:"+reset, err)
		return
	}

	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}

		// Supprimer le dossier localement
		componentPath := filepath.Join(componentsDir, name)
		err := os.RemoveAll(componentPath)
		if err != nil {
			fmt.Println(red+"Erreur suppression composant:"+reset, name, ":", err)
			continue
		}

		// Supprimer du lock file
		lock.Components = removeComponentFromLock(lock, name)

		fmt.Println(green + name + " supprimé ✅" + reset)
	}

	err = saveLockFile(lock)
	if err != nil {
		fmt.Println(red+"Erreur sauvegarde lock file:"+reset, err)
	}
}

func runShadcnAdd(name string) error {
	cmd := exec.Command("npx", "shadcn-ui@latest", "add", name)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func componentExists(lock *LockFile, name string) bool {
	for _, c := range lock.Components {
		if c.Name == name {
			return true
		}
	}
	return false
}

func removeComponentFromLock(lock *LockFile, name string) []Component {
	newComponents := []Component{}
	for _, c := range lock.Components {
		if c.Name != name {
			newComponents = append(newComponents, c)
		}
	}
	return newComponents
}
