package socket

import (
	"encoding/json"
	"fmt"
)

type EventType string

const (
	// Client -> Server
	EvtBuildRequest  EventType = "build_request"  // Build request
	EvtSecretRequest EventType = "secret_request" // Secret fetching request

	// Server -> Client
	EvtBuildQueued    EventType = "build_queued"    // Queued build response message
	EvtLogChunk       EventType = "log_chunk"       // A build part log result
	EvtBuildStatus    EventType = "build_status"    // Updating the build status (running, success, failure)
	EvtSecretResponse EventType = "secret_response" // Secret request response
	EvtError          EventType = "error"           // A standard error message for any event

	EvtPing EventType = "ping"
	EvtPong EventType = "pong"
)

type Message struct {
	Type      EventType       `json:"type"` // The event type (needed)
	RequestID string          `json:"request_id,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"` // Event specific data (raw JSON)
	Error     string          `json:"error,omitempty"`   // Event message if Type=EvtError or for negative error message
}

type BuildRequestPayload struct {
	BuildSpecYAML string `json:"build_spec_yaml"`
	// BuildSpec build.BuildSpec `json:"build_spec"`
}

type BuildQueuedPayload struct {
	BuildID string `json:"build_id"` // UID for this build assigned by the server
	Message string `json:"message"`  // e.g., "Build job accepted and queued"
}

// The log message chunk.
type LogChunkPayload struct {
	BuildID string `json:"build_id"`
	Stream  string `json:"stream"` // "stdout" or "stderr" (or "system")
	Content string `json:"content"`
	// Sequence int    `json:"sequence,omitempty"` // The log sequence
}

// The actual build status.
type BuildStatusPayload struct {
	BuildID     string   `json:"build_id"`
	Status      string   `json:"status"`                 // e.g., "queued", "fetching", "building", "success", "failure"
	Message     string   `json:"message,omitempty"`      // additional Message (e.g., failure reason)
	ArtifactRef string   `json:"artifact_ref,omitempty"` // The ref of the actual completed build (URL, path B2, tag Docker, etc.)
	DurationSec *float64 `json:"duration_sec,omitempty"`
}

type SecretRequestPayload struct {
	Source string `json:"source"`
}

type SecretResponsePayload struct {
	Source string `json:"source"`
	Value  string `json:"value"`
}

type ErrorPayload struct {
	Code    int    `json:"code,omitempty"`
	Details string `json:"details"`
}

func NewMessage(eventType EventType, requestID string) *Message {
	return &Message{Type: eventType, RequestID: requestID}
}

func NewErrorMessage(requestID, errMsg, details string) *Message {
	payloadBytes, _ := json.Marshal(ErrorPayload{Details: details})
	return &Message{
		Type:      EvtError,
		RequestID: requestID,
		Payload:   payloadBytes,
		Error:     errMsg,
	}
}

func (m *Message) AddPayload(payload interface{}) error {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload for type %s: %w", m.Type, err)
	}
	m.Payload = payloadBytes
	return nil
}

func (m *Message) DecodePayload(target interface{}) error {
	if len(m.Payload) == 0 {
		return fmt.Errorf("message payload is empty for type %s", m.Type)
	}
	if err := json.Unmarshal(m.Payload, target); err != nil {
		return fmt.Errorf("failed to unmarshal payload for type %s: %w", m.Type, err)
	}
	return nil
}
