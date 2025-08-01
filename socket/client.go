// socket/client.go
package socket

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"

	"github.com/google/uuid" // Pour générer les RequestID
	"github.com/gorilla/websocket"
)

// Client gère la connexion WebSocket sortante.
type Client struct {
	conn *connection // Wrapper de connexion partagé

	Incoming chan *Message // Canal public pour recevoir les messages du serveur
	Done     chan struct{} // Canal pour signaler l'arrêt du client

	mu         sync.Mutex
	isConnected bool
	dialer     *websocket.Dialer
	connUrl    string
	headers    http.Header // Pour l'authentification ou autres en-têtes

	// Pour corréler requêtes/réponses
	pendingRequests map[string]chan *Message
	pendingMu       sync.RWMutex
}

// NewClient crée une nouvelle instance client.
func NewClient() *Client {
	return &Client{
		Incoming:        make(chan *Message, 100), // Buffer pour les messages entrants
		Done:            make(chan struct{}),
		dialer:          websocket.DefaultDialer,
		pendingRequests: make(map[string]chan *Message),
	}
}

// Connect établit la connexion WebSocket avec le serveur.
func (c *Client) Connect(serverUrl string, headers http.Header) error {
	c.mu.Lock()
	if c.isConnected {
		c.mu.Unlock()
		return fmt.Errorf("client already connected")
	}
	c.connUrl = serverUrl
	c.headers = headers
	c.mu.Unlock()

	log.Printf("Client: Attempting to connect to %s...\n", serverUrl)
	ws, resp, err := c.dialer.Dial(c.connUrl, c.headers)
	if err != nil {
		errMsg := fmt.Sprintf("Client: Failed to connect to %s: %v", c.connUrl, err)
		if resp != nil {
			errMsg = fmt.Sprintf("%s (Status: %s)", errMsg, resp.Status)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if len(body) > 0 {
				errMsg = fmt.Sprintf("%s - Body: %s", errMsg, string(body))
			}
		}
		return fmt.Errorf("an error occurred %s", errMsg)
	}
	log.Printf("Client: Successfully connected to %s\n", c.connUrl)

	c.mu.Lock()
	c.conn = newConnection(ws)
	c.isConnected = true
	// Réinitialiser Done channel au cas où il aurait été fermé par une connexion précédente
	// c.Done = make(chan struct{}) // Non, Done signale la fin définitive du client
	c.mu.Unlock()

	// Démarrer les pompes
	go c.conn.writePump()
	go c.conn.readPump(c.handleIncomingMessage, c.handleDisconnect)

	return nil
}

// IsConnected retourne l'état de la connexion.
func (c *Client) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.isConnected
}

// handleIncomingMessage traite les messages reçus du serveur.
func (c *Client) handleIncomingMessage(msg *Message, conn *connection) error {
	// log.Printf("Client: Received message type %s (ReqID: %s)\n", msg.Type, msg.RequestID) // Debug

	// Vérifier si c'est une réponse à une requête en attente
	c.pendingMu.Lock() // Prendre le verrou en écriture car on va delete
	if msg.RequestID != "" {
		if respChan, ok := c.pendingRequests[msg.RequestID]; ok {
			// C'est une réponse directe à une requête envoyée via SendRequest
			log.Printf("Client: Correlated response for RequestID %s\n", msg.RequestID)
			select {
			case respChan <- msg: // Envoyer la réponse au demandeur
			default:
				// Le demandeur n'écoute plus (timeout?), logguer mais continuer
				log.Printf("Warning: No listener for response channel of RequestID %s\n", msg.RequestID)
			}
			// Supprimer la requête en attente après avoir envoyé la réponse
			delete(c.pendingRequests, msg.RequestID)
			c.pendingMu.Unlock() // Libérer le verrou ici
			return nil // Le message a été traité comme une réponse directe
		}
	}
	c.pendingMu.Unlock() // Libérer le verrou si ce n'était pas une réponse corrélée

	// Si ce n'est pas une réponse directe, le pousser sur le canal public Incoming
	select {
	case c.Incoming <- msg:
		// Message transmis à l'application utilisatrice
	default:
		// Le canal Incoming est plein, l'application ne lit pas assez vite
		log.Printf("Warning: Client Incoming channel full. Message type %s dropped.\n", msg.Type)
	}
	return nil // Succès du traitement (mis en file d'attente)
}

// handleDisconnect est appelé lorsque la connexion est perdue.
func (c *Client) handleDisconnect(conn *connection) {
	c.mu.Lock()
	// Vérifier si c'est bien la connexion actuelle qui est déconnectée
	if c.conn != conn {
		c.mu.Unlock()
		log.Printf("Client: Received disconnect signal for an old/stale connection (%p)\n", conn.ws)
		return // Ne rien faire si ce n'est pas la connexion active
	}
	c.isConnected = false
	c.conn = nil // Libérer la référence à la connexion
	log.Println("Client: Connection lost.")
	// Ne pas fermer c.Done ici, l'utilisateur peut vouloir reconnecter.
	// Fermer Done seulement quand Close() est appelé explicitement.
	c.mu.Unlock()

	// Nettoyer les requêtes en attente pour cette connexion perdue
	c.pendingMu.Lock()
	if len(c.pendingRequests) > 0 {
	    log.Printf("Client: Cleaning up %d pending requests due to disconnect.\n", len(c.pendingRequests))
        for reqID, respChan := range c.pendingRequests {
            // Envoyer une erreur ou fermer le canal pour débloquer les appelants
            // Fermer le canal est plus sûr pour signaler la fin.
            close(respChan)
            delete(c.pendingRequests, reqID)
        }
    }
	c.pendingMu.Unlock()
}

// Send envoie un message au serveur de manière asynchrone.
// Ne garantit pas la réception et ne gère pas les réponses.
func (c *Client) Send(msg *Message) error {
	c.mu.Lock()
	conn := c.conn
	isConnected := c.isConnected
	c.mu.Unlock()

	if !isConnected || conn == nil {
		return fmt.Errorf("client not connected")
	}
	// log.Printf("Client: Sending message type %s async\n", msg.Type) // Debug
	conn.sendMsg(msg)
	return nil
}

// SendRequest envoie un message et attend une réponse corrélée par RequestID.
func (c *Client) SendRequest(ctx context.Context, msgType EventType, payload interface{}) (*Message, error) {
	c.mu.Lock()
	conn := c.conn
	isConnected := c.isConnected
	c.mu.Unlock()

	if !isConnected || conn == nil {
		return nil, fmt.Errorf("client not connected")
	}

	// Générer un RequestID unique
	requestID := uuid.NewString()
	msg := NewMessage(msgType, requestID)
	if payload != nil {
		if err := msg.AddPayload(payload); err != nil {
			return nil, fmt.Errorf("failed to add payload for request %s: %w", requestID, err)
		}
	}

	// Créer un canal pour recevoir la réponse
	respChan := make(chan *Message, 1) // Buffer 1 au cas où la réponse arrive avant qu'on écoute

	c.pendingMu.Lock()
	c.pendingRequests[requestID] = respChan
	c.pendingMu.Unlock()

	// Nettoyer la requête en attente à la fin (succès, erreur, timeout)
	defer func() {
		c.pendingMu.Lock()
		delete(c.pendingRequests, requestID)
		c.pendingMu.Unlock()
		// Ne pas fermer respChan ici, car on peut le lire après la sortie
	}()

	// Envoyer la requête
	log.Printf("Client: Sending request %s (Type: %s)\n", requestID, msg.Type)
	conn.sendMsg(msg) // Envoi asynchrone

	// Attendre la réponse ou l'annulation/timeout du contexte
	select {
	case resp := <-respChan:
		// Réponse reçue !
		log.Printf("Client: Received response for request %s (Type: %s, Error: '%s')\n", requestID, resp.Type, resp.Error)
		if resp.Error != "" || resp.Type == EvtError {
			// Le serveur a renvoyé une erreur pour cette requête
			errMsg := resp.Error
			if errMsg == "" { errMsg = "received error event" } // Cas où Error est vide mais Type est EvtError
			var errPayload ErrorPayload
			if resp.DecodePayload(&errPayload) == nil && errPayload.Details != "" {
                errMsg = fmt.Sprintf("%s: %s", errMsg, errPayload.Details)
			}
			return nil, fmt.Errorf("server error response for request %s: %s", requestID, errMsg)
		}
		// Réponse réussie
		return resp, nil

	case <-ctx.Done():
		// Le contexte a été annulé (timeout ou autre)
		log.Printf("Client: Context done while waiting for response to request %s: %v\n", requestID, ctx.Err())
		return nil, fmt.Errorf("request %s timed out or was canceled: %w", requestID, ctx.Err())

    // Gérer le cas où la connexion est perdue pendant l'attente.
    // La lecture de respChan échouera si handleDisconnect ferme le canal.
    // Il faut vérifier la valeur reçue.
	// case resp, ok := <-respChan:
	// 	if !ok {
	// 		return nil, fmt.Errorf("connection closed while waiting for response to request %s", requestID)
	// 	}
    //    // Traiter resp comme ci-dessus
	}
}


// Close ferme la connexion WebSocket et arrête le client.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	log.Println("Client: Close called.")
	// Signaler l'arrêt aux utilisateurs externes
	close(c.Done)

	if c.conn != nil && c.isConnected {
		// Fermer le canal d'envoi arrêtera la writePump, qui fermera le ws.Conn
		c.conn.closeSend()
		// La readPump s'arrêtera aussi à cause de la fermeture par la writePump ou un read error.
	}
	c.isConnected = false // Marquer comme déconnecté
}