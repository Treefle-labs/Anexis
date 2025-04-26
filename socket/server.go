// socket/server.go
package socket

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Server gère les connexions WebSocket entrantes.
type Server struct {
	hub      *Hub
	upgrader websocket.Upgrader
	// Ajouter ici les dépendances nécessaires au handler métier
	buildService BuildTriggerer // Interface pour découpler du package build
	secretFetcher SecretFetcher  // Interface pour découpler
}

// BuildTriggerer définit l'interface pour démarrer un build et obtenir des notifications.
// Ceci permet de ne pas dépendre directement du package build concret.
type BuildTriggerer interface {
	// StartBuildAsync démarre un build et retourne immédiatement un buildID.
	// Il prend un notificateur pour renvoyer les logs et le statut final.
	StartBuildAsync(ctx context.Context, buildID string, buildSpecYAML string, notifier BuildNotifier) error
}

// SecretFetcher définit l'interface pour récupérer des secrets.
type SecretFetcher interface {
	GetSecret(ctx context.Context, source string) (string, error)
}

// BuildNotifier est utilisé par le service de build pour envoyer des mises à jour au serveur socket.
type BuildNotifier interface {
	NotifyLog(buildID string, stream string, content string)
	NotifyStatus(buildID, status, artifactRef string, buildErr error, duration *float64)
}

// serverBuildNotifier implémente BuildNotifier et envoie les notifications via le Hub.
type serverBuildNotifier struct {
	hub     *Hub
	buildToClient map[string]*connection // Map pour savoir à quel client envoyer les notifs
	mu      sync.RWMutex                 // Protéger la map
}

func newServerBuildNotifier(hub *Hub) *serverBuildNotifier {
	return &serverBuildNotifier{
		hub:           hub,
		buildToClient: make(map[string]*connection),
	}
}

// Associer un build ID à une connexion client spécifique
func (sbn *serverBuildNotifier) registerBuildClient(buildID string, clientConn *connection) {
	sbn.mu.Lock()
	defer sbn.mu.Unlock()
	sbn.buildToClient[buildID] = clientConn
	log.Printf("Notifier: Registered client %p for build %s\n", clientConn.ws, buildID)
}

// Désassocier un build ID (quand le build est fini ou le client déconnecté)
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

	msg := NewMessage(EvtLogChunk, "") // Pas de requestID pour les logs streamés
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
		// On doit quand même désenregistrer le build pour éviter les fuites
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
		payload.Message = buildErr.Error() // Ajouter le message d'erreur au statut
	}

	if err := msg.AddPayload(payload); err == nil {
		clientConn.sendMsg(msg)
	} else {
		log.Printf("Notifier: Error creating build status payload for build %s: %v\n", buildID, err)
	}

	// Si le build est terminé (succès ou échec), désenregistrer
	if status == "success" || status == "failure" {
		sbn.unregisterBuild(buildID)
	}
}


// NewServer crée une nouvelle instance de serveur WebSocket.
func NewServer(buildSvc BuildTriggerer, secretF SecretFetcher) *Server {
	server := &Server{
		// Configurer l'upgrader (vérifier l'origine, etc.)
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			// CheckOrigin est important pour la sécurité en production !
			CheckOrigin: func(r *http.Request) bool {
				// Mettre ici la logique pour autoriser les origines (ex: localhost, votre domaine CLI/UI)
				// Pour le développement, on peut autoriser tout :
				log.Printf("CheckOrigin: Allowing origin %s\n", r.Header.Get("Origin"))
				return true // ATTENTION: Trop permissif pour la prod
			},
		},
		buildService: buildSvc,
		secretFetcher: secretF,
	}
	// Le hub a besoin du handler de message défini dans le serveur
	server.hub = newHub(server.handleMessage)
	return server
}

// Run démarre le hub dans une goroutine.
func (s *Server) Run() {
	go s.hub.run()
}

// ServeHTTP gère les requêtes HTTP et tente de les upgrader en WebSocket.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Upgrade la connexion HTTP en WebSocket
	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ServeHTTP: Failed to upgrade connection: %v\n", err)
		// L'upgrader envoie déjà une réponse HTTP d'erreur
		return
	}
	log.Printf("ServeHTTP: Client connected from %s\n", ws.RemoteAddr())

	// Crée une nouvelle connexion gérée
	conn := newConnection(ws)

	// Enregistre le client auprès du hub
	s.hub.register <- conn

	// Démarre les pompes de lecture et d'écriture pour cette connexion
	// La readPump appellera hub.handleDisconnect quand elle se terminera.
	go conn.writePump()
	go conn.readPump(s.hub.handleIncomingMessage, s.hub.handleDisconnect)
}

// handleMessage est le point central de traitement des messages entrants du serveur.
func (s *Server) handleMessage(msg *Message, client *connection) error {
	ctx := context.Background() // Utiliser un contexte approprié
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

		// Générer un ID de build unique
		buildID := fmt.Sprintf("build-%d", time.Now().UnixNano()) // Ou utiliser UUID

		// Accusé de réception immédiat au client
		ackPayload := BuildQueuedPayload{BuildID: buildID, Message: "Build job accepted"}
		ackMsg := NewMessage(EvtBuildQueued, msg.RequestID) // Utilise le RequestID original
		if err := ackMsg.AddPayload(ackPayload); err != nil {
			log.Printf("Server: Failed to create build queued payload: %v\n", err)
			// Continuer quand même à lancer le build
		}
		client.sendMsg(ackMsg) // Envoyer l'ack

		// Créer et enregistrer le notificateur pour ce build
		notifier := newServerBuildNotifier(s.hub) // TODO: Avoir un seul notificateur partagé ? Ou un par build ? Un partagé semble mieux.
		notifier.registerBuildClient(buildID, client) // Associer le client à ce build

		// Démarrer le build de manière asynchrone via l'interface
		go func() {
			log.Printf("Server: Starting build %s asynchronously\n", buildID)
			// Le contexte peut être utilisé pour l'annulation éventuelle
			err := s.buildService.StartBuildAsync(context.Background(), buildID, payload.BuildSpecYAML, notifier)
			if err != nil {
				// Si StartBuildAsync échoue immédiatement (rare), notifier l'échec
				log.Printf("Server: Failed to start build %s: %v\n", buildID, err)
				notifier.NotifyStatus(buildID, "failure", "", err, nil)
				// Le notifier va désenregistrer le build
			}
            // Si StartBuildAsync réussit, le build s'exécute et le notifier gèrera les logs/status
		}()

		return nil // Succès du traitement de la requête (le build est lancé en async)

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

		// Récupérer le secret (synchrone ici, pourrait être async)
		secretValue, err := s.secretFetcher.GetSecret(ctx, payload.Source)
		if err != nil {
			// Envoyer une réponse d'erreur spécifique au secret
			errMsg := NewErrorMessage(msg.RequestID, "Failed to fetch secret", err.Error())
			client.sendMsg(errMsg)
			return nil // L'erreur a été gérée en envoyant un message d'erreur
		}

		// Envoyer la réponse avec le secret
		respPayload := SecretResponsePayload{Source: payload.Source, Value: secretValue}
		respMsg := NewMessage(EvtSecretResponse, msg.RequestID)
		if err := respMsg.AddPayload(respPayload); err != nil {
			return fmt.Errorf("failed to create secret response payload: %w", err) // Erreur interne serveur
		}
		client.sendMsg(respMsg)
		return nil

	default:
		log.Printf("Server: Received unhandled message type '%s'\n", msg.Type)
		// Envoyer une erreur au client pour indiquer que le type n'est pas géré ?
		errMsg := NewErrorMessage(msg.RequestID, "Unhandled message type", fmt.Sprintf("Type '%s' not supported by server", msg.Type))
		client.sendMsg(errMsg)
		return nil // Pas une erreur fatale pour la connexion
	}
}