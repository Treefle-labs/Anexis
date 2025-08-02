package socket

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	receivedBuildID   string
	receivedBuildSpec string
	wg                sync.WaitGroup
	mu                sync.Mutex
)

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

	mockBuildSvc := &MockBuildTriggerer{
		StartBuildFunc: func(ctx context.Context, buildID string, buildSpecYAML string, notifier BuildNotifier) error {
			t.Logf("MockBuildTriggerer: StartBuildAsync called for BuildID: %s\n", buildID)
			mu.Lock()
			receivedBuildID = buildID // Catching this for verification
			receivedBuildSpec = buildSpecYAML
			mu.Unlock()

			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(50 * time.Millisecond)
				notifier.NotifyLog(buildID, "stdout", "Fetching code...")
				time.Sleep(50 * time.Millisecond)
				notifier.NotifyLog(buildID, "stdout", "Building image...")
				time.Sleep(50 * time.Millisecond)
				duration := 150.0 * time.Millisecond.Seconds()
				notifier.NotifyStatus(buildID, "success", "docker.io/library/test:latest", nil, &duration)
				t.Logf("MockBuildTriggerer: Sent final status for BuildID: %s\n", buildID)
			}()
			return nil
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
	server := NewServer(mockBuildSvc, mockSecretSvc, func(r *http.Request) bool { return true })
	server.Run()

	httpServer := httptest.NewServer(server)
	defer httpServer.Close()

	// Convert the HTTP url into WS url prefixed
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")
	t.Logf("Test WebSocket Server running at: %s\n", wsURL)

	// 3. Start Client and Connect
	client := NewClient()
	err := client.Connect(wsURL, nil)
	require.NoError(t, err, "Client failed to connect")
	defer client.Close() // Be sure that client is closed

	require.True(t, client.IsConnected(), "Client should be connected")

	// 4. Test Build Request / Response Flow
	buildMessagesReceived := make(chan *Message, 10) // Buffer to collect the build messages
	go func() {
		for msg := range client.Incoming {
			// Filter messages for this specific test
			if msg.Type == EvtBuildQueued || msg.Type == EvtLogChunk || msg.Type == EvtBuildStatus {
				buildMessagesReceived <- msg
			} else {
				t.Logf("Client Incoming Monitor: Received other message type: %s\n", msg.Type)
			}
		}
		t.Log("Client Incoming Monitor: Exited.")
		close(buildMessagesReceived) // Close if client.Incoming is closed
	}()

	// Send the build request
	buildSpecContent := "name: test-build\nversion: '1.0'\n..." // Mock YAML content
	buildReqPayload := BuildRequestPayload{BuildSpecYAML: buildSpecContent}
	ctxReq, cancelReq := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelReq()

	// Use SendRequest to get the acknowledgement of receipt (BuildQueued)
	respMsg, err := client.SendRequest(ctxReq, EvtBuildRequest, buildReqPayload)
	require.NoError(t, err, "SendRequest for build failed")
	require.NotNil(t, respMsg, "Response message should not be nil")
	require.Equal(t, EvtBuildQueued, respMsg.Type, "Response should be BuildQueued")

	// Decode the response payload to get the buildID
	var queuedPayload BuildQueuedPayload
	err = respMsg.DecodePayload(&queuedPayload)
	require.NoError(t, err)
	require.NotEmpty(t, queuedPayload.BuildID, "BuildID should be in queued payload")
	mu.Lock()
	assert.Equal(t, queuedPayload.BuildID, receivedBuildID, "BuildID in response should match ID received by mock service")
	assert.Equal(t, buildSpecContent, receivedBuildSpec, "BuildSpec received by mock should match sent spec")
	mu.Unlock()

	// Waiting for streaming messages (logs, status final)
	expectedLogs := []string{"Fetching code...", "Building image..."}
	receivedLogs := []string{}
	var finalStatusPayload BuildStatusPayload
	receivedFinalStatus := false

	timeout := time.After(3 * time.Second) // Timeout for waiting for the streaming messages
	logLoopDone := false
	for !logLoopDone {
		select {
		case msg, ok := <-buildMessagesReceived:
			if !ok {
				t.Log("Build message channel closed.")
				logLoopDone = true
				break
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
				logLoopDone = true
			}
		case <-timeout:
			t.Fatal("Timeout waiting for streamed build messages (logs/status)")
		}
	}

	// Check the logs and the final status
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

	// Test secret not found
	secretReqPayloadFail := SecretRequestPayload{Source: "invalid/secret"}
	ctxSecretFail, cancelSecretFail := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancelSecretFail()

	_, err = client.SendRequest(ctxSecretFail, EvtSecretRequest, secretReqPayloadFail)
	require.Error(t, err, "SendRequest for invalid secret should fail")
	assert.Contains(t, err.Error(), "secret 'invalid/secret' not found")

	wg.Wait()
	t.Log("Mock Build goroutine finished.")

	client.Close()
	// Waiting for the `buildMessagesReceived` chanel to be closed
	<-time.After(100 * time.Millisecond)

}
