package socket

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Mocks pour BuildTriggerer et SecretFetcher ---

type MockBuildTriggerer struct {
	StartBuildFunc func(ctx context.Context, buildID string, buildSpecYAML string, notifier BuildNotifier) error
}

func (m *MockBuildTriggerer) StartBuildAsync(ctx context.Context, buildID string, buildSpecYAML string, notifier BuildNotifier) error {
	if m.StartBuildFunc != nil {
		return m.StartBuildFunc(ctx, buildID, buildSpecYAML, notifier)
	}
	return fmt.Errorf("StartBuildFunc not implemented in mock")
}

type MockSecretFetcher struct {
	GetSecretFunc func(ctx context.Context, source string) (string, error)
}

func (m *MockSecretFetcher) GetSecret(ctx context.Context, source string) (string, error) {
	if m.GetSecretFunc != nil {
		return m.GetSecretFunc(ctx, source)
	}
	return "", fmt.Errorf("GetSecretFunc not implemented in mock")
}

// --- Test Client-Server Interaction ---

func TestSocket_ClientServerCommunication(t *testing.T) {
	// 1. Setup Mock Services
	var wg sync.WaitGroup       // Pour attendre les goroutines de notification
	var receivedBuildID string
	var receivedBuildSpec string

	mockBuildSvc := &MockBuildTriggerer{
		StartBuildFunc: func(ctx context.Context, buildID string, buildSpecYAML string, notifier BuildNotifier) error {
			t.Logf("MockBuildTriggerer: StartBuildAsync called for BuildID: %s\n", buildID)
			receivedBuildID = buildID // Capturer pour vérification
			receivedBuildSpec = buildSpecYAML

			// Simuler un build en arrière-plan qui envoie des logs et un statut
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(50 * time.Millisecond) // Simuler le travail
				notifier.NotifyLog(buildID, "stdout", "Fetching code...")
				time.Sleep(50 * time.Millisecond)
				notifier.NotifyLog(buildID, "stdout", "Building image...")
				time.Sleep(50 * time.Millisecond)
				// Simuler un succès
				duration := 150.0 * time.Millisecond.Seconds()
				notifier.NotifyStatus(buildID, "success", "docker.io/library/test:latest", nil, &duration)
				t.Logf("MockBuildTriggerer: Sent final status for BuildID: %s\n", buildID)
			}()
			return nil // Retourner nil pour indiquer que le lancement async a réussi
		},
	}

	mockSecretSvc := &MockSecretFetcher{
		GetSecretFunc: func(ctx context.Context, source string) (string, error) {
			t.Logf("MockSecretFetcher: GetSecret called for source: %s\n", source)
			if source == "valid/secret" {
				return "secret_value_123", nil
			}
			return "", fmt.Errorf("secret '%s' not found", source)
		},
	}

	// 2. Start Test Server
	server := NewServer(mockBuildSvc, mockSecretSvc)
	server.Run() // Démarre le hub

	httpServer := httptest.NewServer(server) // Utilise le ServeHTTP du serveur socket
	defer httpServer.Close()

	// Convertir l'URL HTTP en WS
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")
	t.Logf("Test WebSocket Server running at: %s\n", wsURL)

	// 3. Start Client and Connect
	client := NewClient()
	err := client.Connect(wsURL, nil)
	require.NoError(t, err, "Client failed to connect")
	defer client.Close() // S'assurer que le client est fermé

	require.True(t, client.IsConnected(), "Client should be connected")

	// 4. Test Build Request / Response Flow
	buildMessagesReceived := make(chan *Message, 10) // Buffer pour collecter les messages liés au build
	go func() {
		for msg := range client.Incoming {
			// Filtrer les messages pour ce test
			if msg.Type == EvtBuildQueued || msg.Type == EvtLogChunk || msg.Type == EvtBuildStatus {
				buildMessagesReceived <- msg
			} else {
				t.Logf("Client Incoming Monitor: Received other message type: %s\n", msg.Type)
			}
		}
		t.Log("Client Incoming Monitor: Exited.")
		close(buildMessagesReceived) // Fermer quand client.Incoming est fermé
	}()

	// Envoyer la requête de build
	buildSpecContent := "name: test-build\nversion: '1.0'\n..." // Contenu YAML factice
	buildReqPayload := BuildRequestPayload{BuildSpecYAML: buildSpecContent}
	ctxReq, cancelReq := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelReq()

	// Utiliser SendRequest pour obtenir l'accusé de réception (BuildQueued)
	respMsg, err := client.SendRequest(ctxReq, EvtBuildRequest, buildReqPayload)
	require.NoError(t, err, "SendRequest for build failed")
	require.NotNil(t, respMsg, "Response message should not be nil")
	require.Equal(t, EvtBuildQueued, respMsg.Type, "Response should be BuildQueued")

	// Décoder le payload de la réponse pour obtenir le buildID
	var queuedPayload BuildQueuedPayload
	err = respMsg.DecodePayload(&queuedPayload)
	require.NoError(t, err)
	require.NotEmpty(t, queuedPayload.BuildID, "BuildID should be in queued payload")
	assert.Equal(t, queuedPayload.BuildID, receivedBuildID, "BuildID in response should match ID received by mock service") // Vérifier la cohérence
	assert.Equal(t, buildSpecContent, receivedBuildSpec, "BuildSpec received by mock should match sent spec")

	// Attendre les messages streamés (logs, status final)
	expectedLogs := []string{"Fetching code...", "Building image..."}
	receivedLogs := []string{}
	var finalStatusPayload BuildStatusPayload
	receivedFinalStatus := false

	timeout := time.After(3 * time.Second) // Timeout pour attendre les messages streamés
	logLoopDone := false
	for !logLoopDone {
		select {
		case msg, ok := <-buildMessagesReceived:
			if !ok {
				t.Log("Build message channel closed.")
				logLoopDone = true
				break // Sortir si le canal est fermé
			}
			t.Logf("Client Received Async Message: Type=%s", msg.Type)
			switch msg.Type {
			case EvtLogChunk:
				var logPayload LogChunkPayload
				err := msg.DecodePayload(&logPayload)
				require.NoError(t, err)
				assert.Equal(t, receivedBuildID, logPayload.BuildID)
				receivedLogs = append(receivedLogs, logPayload.Content)
			case EvtBuildStatus:
				err := msg.DecodePayload(&finalStatusPayload)
				require.NoError(t, err)
				assert.Equal(t, receivedBuildID, finalStatusPayload.BuildID)
				receivedFinalStatus = true
				logLoopDone = true // On a reçu le statut final
			}
		case <-timeout:
			t.Fatal("Timeout waiting for streamed build messages (logs/status)")
		}
	}

	// Vérifier les logs et le statut final reçus
	assert.ElementsMatch(t, expectedLogs, receivedLogs, "Received logs do not match expected logs")
	require.True(t, receivedFinalStatus, "Should have received a final build status")
	assert.Equal(t, "success", finalStatusPayload.Status)
	assert.Equal(t, "docker.io/library/test:latest", finalStatusPayload.ArtifactRef)
	require.NotNil(t, finalStatusPayload.DurationSec)
	assert.True(t, *finalStatusPayload.DurationSec > 0)

	// 5. Test Secret Request / Response Flow
	secretReqPayload := SecretRequestPayload{Source: "valid/secret"}
	ctxSecret, cancelSecret := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancelSecret()

	secretRespMsg, err := client.SendRequest(ctxSecret, EvtSecretRequest, secretReqPayload)
	require.NoError(t, err, "SendRequest for secret failed")
	require.NotNil(t, secretRespMsg)
	require.Equal(t, EvtSecretResponse, secretRespMsg.Type)

	var secretRespPayload SecretResponsePayload
	err = secretRespMsg.DecodePayload(&secretRespPayload)
	require.NoError(t, err)
	assert.Equal(t, "valid/secret", secretRespPayload.Source)
	assert.Equal(t, "secret_value_123", secretRespPayload.Value)

	// Test secret non trouvé
	secretReqPayloadFail := SecretRequestPayload{Source: "invalid/secret"}
	ctxSecretFail, cancelSecretFail := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancelSecretFail()

	_, err = client.SendRequest(ctxSecretFail, EvtSecretRequest, secretReqPayloadFail)
	require.Error(t, err, "SendRequest for invalid secret should fail")
	assert.Contains(t, err.Error(), "secret 'invalid/secret' not found") // Vérifier l'erreur du serveur renvoyée

	// Attendre que la goroutine de notification du mock se termine
	wg.Wait()
	t.Log("Mock Build goroutine finished.")

	// Fermer explicitement le client pour terminer le moniteur Incoming
	client.Close()
	// Attendre un court instant pour que le canal `buildMessagesReceived` se ferme
	<-time.After(100 * time.Millisecond)

}