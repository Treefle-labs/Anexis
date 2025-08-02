package socket

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type Server struct {
	hub           *Hub
	upgrader      websocket.Upgrader
	buildService  BuildTriggerer // Interface implementing a build process
	secretFetcher SecretFetcher  // Interface implementing the secret service fetcher
}

type BuildTriggerer interface {
	StartBuildAsync(ctx context.Context, buildID string, buildSpecYAML string, notifier BuildNotifier) error
}

type SecretFetcher interface {
	GetSecret(ctx context.Context, source string) (string, error)
}

type BuildNotifier interface {
	NotifyLog(buildID string, stream string, content string)
	NotifyStatus(buildID, status, artifactRef string, buildErr error, duration *float64)
}

type serverBuildNotifier struct {
	hub           *Hub
	buildToClient map[string]*connection
	mu            sync.RWMutex
}

func newServerBuildNotifier(hub *Hub) *serverBuildNotifier {
	return &serverBuildNotifier{
		hub:           hub,
		buildToClient: make(map[string]*connection),
	}
}

func (sbn *serverBuildNotifier) registerBuildClient(buildID string, clientConn *connection) {
	sbn.mu.Lock()
	defer sbn.mu.Unlock()
	sbn.buildToClient[buildID] = clientConn
	log.Printf("Notifier: Registered client %p for build %s\n", clientConn.ws, buildID)
}

func (sbn *serverBuildNotifier) unregisterBuild(buildID string) {
	sbn.mu.Lock()
	defer sbn.mu.Unlock()
	delete(sbn.buildToClient, buildID)
	log.Printf("Notifier: Unregistered build %s\n", buildID)
}

func (sbn *serverBuildNotifier) getClientForBuild(buildID string) *connection {
	sbn.mu.RLock()
	defer sbn.mu.RUnlock()
	return sbn.buildToClient[buildID]
}

func (sbn *serverBuildNotifier) NotifyLog(buildID string, stream string, content string) {
	clientConn := sbn.getClientForBuild(buildID)
	if clientConn == nil {
		log.Printf("Notifier: No client found for build %s to send log chunk.\n", buildID)
		return
	}

	msg := NewMessage(EvtLogChunk, "")
	payload := LogChunkPayload{
		BuildID: buildID,
		Stream:  stream,
		Content: content,
	}
	if err := msg.AddPayload(payload); err == nil {
		clientConn.sendMsg(msg)
	} else {
		log.Printf("Notifier: Error creating log chunk payload for build %s: %v\n", buildID, err)
	}
}

func (sbn *serverBuildNotifier) NotifyStatus(buildID string, status string, artifactRef string, buildErr error, duration *float64) {
	clientConn := sbn.getClientForBuild(buildID)
	if clientConn == nil {
		log.Printf("Notifier: No client found for build %s to send status update.\n", buildID)
		sbn.unregisterBuild(buildID)
		return
	}

	msg := NewMessage(EvtBuildStatus, "")
	payload := BuildStatusPayload{
		BuildID:     buildID,
		Status:      status,
		ArtifactRef: artifactRef,
		DurationSec: duration,
	}
	if buildErr != nil {
		payload.Message = buildErr.Error()
	}

	if err := msg.AddPayload(payload); err == nil {
		clientConn.sendMsg(msg)
	} else {
		log.Printf("Notifier: Error creating build status payload for build %s: %v\n", buildID, err)
	}

	if status == "success" || status == "failure" {
		sbn.unregisterBuild(buildID)
	}
}

// Creating a new Websocket server and upgrading connection
func NewServer(buildSvc BuildTriggerer, secretF SecretFetcher, originChecker func (r *http.Request) bool) *Server {
	server := &Server{
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin: func(r *http.Request) bool {
				log.Printf("CheckOrigin: Checking origin %s\n", r.Header.Get("Origin"))
				return originChecker(r)
			},
		},
		buildService:  buildSvc,
		secretFetcher: secretF,
	}
	server.hub = newHub(server.handleMessage)
	return server
}

// Launching the Hub in a goroutine.
func (s *Server) Run() {
	go s.hub.run()
}

// Handling http request and trying to upgrade it to a websocket connection.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ServeHTTP: Failed to upgrade connection: %v\n", err)
		return
	}
	log.Printf("ServeHTTP: Client connected from %s\n", ws.RemoteAddr())

	conn := newConnection(ws)

	s.hub.register <- conn

	go conn.writePump()
	go conn.readPump(s.hub.handleIncomingMessage, s.hub.handleDisconnect)
}

// The main entry point for all incoming Message.
func (s *Server) handleMessage(msg *Message, client *connection) error {
	ctx := context.Background()
	log.Printf("Server: Handling message type '%s' from %p (ReqID: %s)\n", msg.Type, client.ws, msg.RequestID)

	switch msg.Type {
	case EvtBuildRequest:
		var payload BuildRequestPayload
		if err := msg.DecodePayload(&payload); err != nil {
			return fmt.Errorf("invalid build request payload: %w", err)
		}
		if payload.BuildSpecYAML == "" {
			return fmt.Errorf("build spec YAML cannot be empty")
		}

		uuid := uuid.NewString()
		buildID := fmt.Sprintf("build-%s", uuid)

		// immediately acknowledge the build request
		ackPayload := BuildQueuedPayload{BuildID: buildID, Message: "Build job accepted"}
		ackMsg := NewMessage(EvtBuildQueued, msg.RequestID) // Utilise le RequestID original
		if err := ackMsg.AddPayload(ackPayload); err != nil {
			log.Printf("Server: Failed to create build queued payload: %v\n", err)
		}
		client.sendMsg(ackMsg)

		// Create and register the notifier for this build
		notifier := newServerBuildNotifier(s.hub) 
		notifier.registerBuildClient(buildID, client)

		// Start the build asynchronously via the interface
		go func() {
			log.Printf("Server: Starting build %s asynchronously\n", buildID)
			// The context can be used for eventual cancellation
			err := s.buildService.StartBuildAsync(context.Background(), buildID, payload.BuildSpecYAML, notifier)
			if err != nil {
				// If StartBuildAsync fails immediately (rare), notify the failure
				log.Printf("Server: Failed to start build %s: %v\n", buildID, err)
				notifier.NotifyStatus(buildID, "failure", "", err, nil)
				// The notifier will unregister the build
			}
			// If StartBuildAsync succeeds, the build runs and the notifier will handle logs/status
		}()

		return nil // Success in processing the request (the build is started asynchronously)

	case EvtSecretRequest:
		var payload SecretRequestPayload
		if err := msg.DecodePayload(&payload); err != nil {
			return fmt.Errorf("invalid secret request payload: %w", err)
		}
		if payload.Source == "" {
			return fmt.Errorf("secret source cannot be empty")
		}
		if s.secretFetcher == nil {
			return fmt.Errorf("secret fetcher service is not configured on the server")
		}

		// Fetch the secret using the secret fetcher service
		secretValue, err := s.secretFetcher.GetSecret(ctx, payload.Source)
		if err != nil {
			errMsg := NewErrorMessage(msg.RequestID, "Failed to fetch secret", err.Error())
			client.sendMsg(errMsg)
			return nil
		}

		respPayload := SecretResponsePayload{Source: payload.Source, Value: secretValue}
		respMsg := NewMessage(EvtSecretResponse, msg.RequestID)
		if err := respMsg.AddPayload(respPayload); err != nil {
			return fmt.Errorf("failed to create secret response payload: %w", err)
		}
		client.sendMsg(respMsg)
		return nil

	default:
		log.Printf("Server: Received unhandled message type '%s'\n", msg.Type)
		errMsg := NewErrorMessage(msg.RequestID, "Unhandled message type", fmt.Sprintf("Type '%s' not supported by server", msg.Type))
		client.sendMsg(errMsg)
		return nil
	}
}
