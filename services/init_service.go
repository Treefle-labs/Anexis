package services

import (
	"log"
)

var (
	Opts    *ServiceOptions
	Service *SecureEncryptionService
)

func init() {
	Opts = DefaultServiceOptions()

	// Personnaliser le chemin de travail
	workDir := "../.encryption-service"
	Opts.WorkingDir = workDir

	// Augmenter les ressources disponibles
	Opts.MaxCPUs = 4
	Opts.MaxMemoryMB = 1024

	// Activer le mode debug pour les logs
	Opts.LogLevel = "debug"

	// Initialiser le service
	service, err := InitService(Opts)
	if err != nil {
		log.Fatalf("Failed to initialize encryption service: %v", err)
	}
	Service = service
}
