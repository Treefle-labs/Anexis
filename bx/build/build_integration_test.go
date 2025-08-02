// build/build_integration_socket_test.go
package build

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	// Importer socket et testify etc.
	"github.com/Treefle-labs/Anexis/socket" // Ajuster le chemin si besoin

	"github.com/docker/docker/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// Test d'intégration complet: Client -> Serveur Socket -> BuildService -> Docker
func TestIntegration_SocketTriggeredBuild_LocalOutput(t *testing.T) {
	// //go:build integration
	skipWithoutDocker(t) // Fonction helper définie dans build_test.go

	// 1. Setup BuildService (avec un mock fetcher simple)
	tempDir := t.TempDir()
	mockFetcher := &MockSecretFetcher{Secrets: map[string]string{"secret/for/build": "build_secret"}}
	buildService, err := NewBuildService(tempDir, false, mockFetcher)
	require.NoError(t, err)

	// 2. Setup et démarrer le Serveur Socket
	socketServer := socket.NewServer(buildService, buildService, func(r *http.Request) bool {return true}) // buildService implémente les deux interfaces
	socketServer.Run()
	httpServer := httptest.NewServer(socketServer)
	defer httpServer.Close()
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")
	t.Logf("Integration Test Server running at: %s\n", wsURL)

	// 3. Préparer le BuildSpec et la codebase locale
	codeDir := createTempDir(t, tempDir, "appcode")
	dockerfileContent := `
FROM alpine:latest
ARG SECRET_ARG=not_set
ENV BUILT_SECRET=$SECRET_ARG
COPY data.txt /
RUN echo "Built at $(date)" >> /build_time.txt
CMD cat /data.txt && echo "Secret: $BUILT_SECRET" && cat /build_time.txt
`
	createTempFile(t, codeDir, "Dockerfile", dockerfileContent)
	createTempFile(t, codeDir, "data.txt", "Real build data")

	buildVersion := fmt.Sprintf("sock-0.1-%d", time.Now().Unix())
	buildSpec := &BuildSpec{
		Name:    "integ-socket-build",
		Version: buildVersion,
		Codebases: []CodebaseConfig{
			{Name: "app", SourceType: "local", Source: codeDir},
		},
		BuildConfig: BuildConfig{
			Dockerfile:   "app/Dockerfile", // Relatif au buildDir créé par BuildService
			Args:         map[string]string{"SECRET_ARG": "${SECRET_FROM_ENV}"}, // Sera injecté par runtimeEnv
			OutputTarget: "local", // Sortie fichier .tar
			Tags: []string{ // Tags même si sortie locale
				fmt.Sprintf("integ-socket-build:%s", buildVersion),
			},
		},
		RunConfigDef: RunConfigDef{Generate: true, ArtifactStorage: "local"},
		// Secret requis par l'argument de build (sera récupéré via le serveur)
		Secrets: []SecretSpec{{Name: "SECRET_FROM_ENV", Source: "secret/for/build"}},
	}
	imageTag := buildSpec.BuildConfig.Tags[0]

	// Convertir BuildSpec en YAML pour l'envoyer
	specYAMLBytes, err := yaml.Marshal(buildSpec)
	require.NoError(t, err)
	specYAML := string(specYAMLBytes)

	// 4. Démarrer le Client Socket et se connecter
	socketClient := socket.NewClient()
	err = socketClient.Connect(wsURL, nil)
	require.NoError(t, err)
	defer socketClient.Close()

	// 5. Écouter les messages entrants sur le client
	clientMessages := make(chan *socket.Message, 20) // Buffer large pour les logs
	clientErrors := make(chan error, 1)
	go func() {
		for msg := range socketClient.Incoming {
			clientMessages <- msg
		}
		close(clientMessages) // Signaler la fin quand le client se ferme
		t.Log("Client message listener finished.")
	}()

	// 6. Envoyer la requête de build
	buildReqPayload := socket.BuildRequestPayload{BuildSpecYAML: specYAML}
	ctxReq, cancelReq := context.WithTimeout(context.Background(), 5*time.Second) // Timeout pour recevoir BuildQueued
	defer cancelReq()

	respMsg, err := socketClient.SendRequest(ctxReq, socket.EvtBuildRequest, buildReqPayload)
	require.NoError(t, err, "Failed to send build request")
	require.Equal(t, socket.EvtBuildQueued, respMsg.Type)

	var queuedPayload socket.BuildQueuedPayload
	err = respMsg.DecodePayload(&queuedPayload)
	require.NoError(t, err)
	require.NotEmpty(t, queuedPayload.BuildID)
	buildID := queuedPayload.BuildID
	t.Logf("Build queued with ID: %s", buildID)

	// 7. Attendre et vérifier les messages de logs et de statut final
	var receivedLogs strings.Builder
	var finalStatusPayload socket.BuildStatusPayload
	receivedFinalStatus := false
	buildTimeout := time.After(30 * time.Second) // Timeout pour le build complet

	keepListening := true
	for keepListening {
		select {
		case msg, ok := <-clientMessages:
			if !ok {
				t.Log("Client message channel closed unexpectedly.")
				clientErrors <- fmt.Errorf("client message channel closed before receiving final status")
				keepListening = false
				break
			}
			t.Logf("Client Received: Type=%s", msg.Type)
			switch msg.Type {
			case socket.EvtLogChunk:
				var logPayload socket.LogChunkPayload
				if err := msg.DecodePayload(&logPayload); err == nil {
					require.Equal(t, buildID, logPayload.BuildID)
					receivedLogs.WriteString(logPayload.Content) // Concaténer les logs
				} else {
					t.Logf("Failed to decode log chunk payload: %v", err)
				}
			case socket.EvtBuildStatus:
				var statusPayload socket.BuildStatusPayload
				if err := msg.DecodePayload(&statusPayload); err == nil {
					require.Equal(t, buildID, statusPayload.BuildID)
					t.Logf("Received Status Update: %s (Msg: %s)", statusPayload.Status, statusPayload.Message)
					// Vérifier si c'est un statut terminal
					if statusPayload.Status == "success" || statusPayload.Status == "failure" {
						finalStatusPayload = statusPayload
						receivedFinalStatus = true
						keepListening = false // Arrêter d'écouter
					}
				} else {
					t.Logf("Failed to decode status payload: %v", err)
				}
            case socket.EvtError: // Gérer les erreurs générales
                t.Logf("Received general error message: %s", msg.Error)
                clientErrors <- fmt.Errorf("received error event: %s", msg.Error)
                keepListening = false

			}
		case <-buildTimeout:
			t.Fatalf("Timeout waiting for final build status for BuildID %s", buildID)
		case err := <-clientErrors:
		    t.Fatalf("Error received during build: %v", err)

		}
	}

	// 8. Vérifier le résultat
	require.True(t, receivedFinalStatus, "Should have received a final build status")
	// Vérifier les logs reçus (rechercher des motifs clés)
	logs := receivedLogs.String()
	t.Logf("--- Received Build Logs ---\n%s\n-------------------------\n", logs)
	assert.Contains(t, logs, "Starting build process...")
	assert.Contains(t, logs, "Fetching secrets...") // Vient du BuildService
	assert.Contains(t, logs, "Successfully built")  // Vient de Docker
	assert.Contains(t, logs, "Build process completed successfully.")

	// Vérifier le statut final
	require.Equal(t, "success", finalStatusPayload.Status, "Final build status should be success. Error: %s", finalStatusPayload.Message)
	require.NotNil(t, finalStatusPayload.DurationSec)
	assert.True(t, *finalStatusPayload.DurationSec > 0)
	require.NotEmpty(t, finalStatusPayload.ArtifactRef, "Artifact reference should not be empty for local output")
	assert.True(t, filepath.IsAbs(finalStatusPayload.ArtifactRef), "Local artifact path should be absolute") // runBuildLogic retourne le chemin absolu

	// Vérifier l'existence de l'artefact local (.tar)
	_, err = os.Stat(finalStatusPayload.ArtifactRef)
	assert.NoError(t, err, "Local artifact file should exist at %s", finalStatusPayload.ArtifactRef)

	// Vérifier le run.yml
	runYmlPath := filepath.Join(filepath.Dir(finalStatusPayload.ArtifactRef), fmt.Sprintf("%s-%s.run.yml", buildSpec.Name, buildSpec.Version))
	_, err = os.Stat(runYmlPath)
	assert.NoError(t, err, "run.yml file should exist at %s", runYmlPath)

	// 9. Nettoyage Docker (l'image a été buildée même si sortie locale)
	cli, _ := client.NewClientWithOpts(client.FromEnv)
	defer cli.Close()
	t.Cleanup(func() {
		removeDockerImage(t, cli, imageTag)
	})
	assert.True(t, dockerImageExists(t, cli, imageTag), "Docker image should exist after build")
}