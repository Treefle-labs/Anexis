// socket/message.go
package socket

import (
	"encoding/json"
	"fmt"
)

// EventType définit le type de message échangé.
type EventType string

// Constantes pour les types d'événements possibles.
const (
	// Client -> Server
	EvtBuildRequest  EventType = "build_request"  // Demande de lancement d'un build
	EvtSecretRequest EventType = "secret_request" // Demande de récupération d'un secret

	// Server -> Client
	EvtBuildQueued   EventType = "build_queued"    // Accusé de réception, build mis en file d'attente
	EvtLogChunk      EventType = "log_chunk"       // Un morceau de log du build
	EvtBuildStatus   EventType = "build_status"    // Mise à jour du statut (running, success, failure)
	EvtSecretResponse EventType = "secret_response" // Réponse à une demande de secret
	EvtError         EventType = "error"           // Message d'erreur général ou spécifique à une requête

	// Bidirectionnel (peut-être moins utile ici, mais possible)
	// EvtPing EventType = "ping"
	// EvtPong EventType = "pong"
)

// Message est la structure de base échangée via WebSocket.
type Message struct {
	Type      EventType       `json:"type"`                 // Type d'événement (obligatoire)
	RequestID string          `json:"request_id,omitempty"` // ID unique pour corréler req/resp (généré par le client)
	Payload   json.RawMessage `json:"payload,omitempty"`    // Données spécifiques à l'événement (JSON brut)
	Error     string          `json:"error,omitempty"`      // Message d'erreur si Type=EvtError ou pour une réponse négative
}

// --- Payloads Spécifiques ---
// (Définir des structs pour les payloads complexes améliore la lisibilité et la robustesse)

// BuildRequestPayload contient les informations pour démarrer un build.
type BuildRequestPayload struct {
	// Le BuildSpec peut être volumineux, l'envoyer en YAML est souvent pratique.
	BuildSpecYAML string `json:"build_spec_yaml"`
	// Ou vous pourriez envoyer la struct BuildSpec directement si elle est sérialisable en JSON
	// BuildSpec build.BuildSpec `json:"build_spec"`
}

// BuildQueuedPayload confirme la réception et fournit un ID de build.
type BuildQueuedPayload struct {
	BuildID string `json:"build_id"` // ID unique assigné par le serveur pour ce build
	Message string `json:"message"`  // e.g., "Build job accepted and queued"
}

// LogChunkPayload contient un morceau de log.
type LogChunkPayload struct {
	BuildID string `json:"build_id"`          // ID du build concerné
	Stream  string `json:"stream"`            // "stdout" ou "stderr" (ou "system")
	Content string `json:"content"`           // Le contenu du log
	// Sequence int    `json:"sequence,omitempty"` // Optionnel: pour garantir l'ordre si nécessaire
}

// BuildStatusPayload donne l'état actuel du build.
type BuildStatusPayload struct {
	BuildID       string `json:"build_id"`
	Status        string `json:"status"` // e.g., "queued", "fetching", "building", "success", "failure"
	Message       string `json:"message,omitempty"` // Message additionnel (e.g., raison de l'échec)
	ArtifactRef   string `json:"artifact_ref,omitempty"` // Référence à l'artefact (URL, path B2, tag Docker, etc.) si succès
	DurationSec   *float64 `json:"duration_sec,omitempty"` // Durée du build si terminé
	// On évite d'envoyer le BuildResult complet ici, trop lourd potentiellement.
}

// SecretRequestPayload demande une valeur de secret.
type SecretRequestPayload struct {
	Source string `json:"source"` // L'identifiant du secret requis (correspond à SecretSpec.Source)
}

// SecretResponsePayload fournit la valeur d'un secret.
type SecretResponsePayload struct {
	Source string `json:"source"` // L'identifiant demandé
	Value  string `json:"value"`  // La valeur du secret (ATTENTION: Sécurité !)
}

// ErrorPayload (utilisé si Message.Type == EvtError)
type ErrorPayload struct {
	Code    int    `json:"code,omitempty"` // Code d'erreur interne (optionnel)
	Details string `json:"details"`        // Description de l'erreur
}

// --- Helpers pour créer des messages ---

// NewMessage crée un message simple sans payload.
func NewMessage(eventType EventType, requestID string) *Message {
	return &Message{Type: eventType, RequestID: requestID}
}

// NewErrorMessage crée un message d'erreur.
func NewErrorMessage(requestID, errMsg string, details string) *Message {
	payloadBytes, _ := json.Marshal(ErrorPayload{Details: details})
	return &Message{
		Type:      EvtError,
		RequestID: requestID,
		Payload:   payloadBytes,
		Error:     errMsg,
	}
}

// AddPayload attache un payload structuré à un message existant.
func (m *Message) AddPayload(payload interface{}) error {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload for type %s: %w", m.Type, err)
	}
	m.Payload = payloadBytes
	return nil
}

// DecodePayload décode le Payload json.RawMessage dans la structure fournie.
func (m *Message) DecodePayload(target interface{}) error {
	if len(m.Payload) == 0 {
		return fmt.Errorf("message payload is empty for type %s", m.Type)
	}
	if err := json.Unmarshal(m.Payload, target); err != nil {
		return fmt.Errorf("failed to unmarshal payload for type %s: %w", m.Type, err)
	}
	return nil
}