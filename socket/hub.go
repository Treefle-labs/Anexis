// socket/hub.go
package socket

import (
	"log"
	"sync"
)

// Hub maintient l'ensemble des clients actifs et diffuse les messages.
type Hub struct {
	clients    map[*connection]bool // Utiliser la connexion comme clé, booléen juste pour la présence
	register   chan *connection     // Canal pour enregistrer de nouveaux clients
	unregister chan *connection     // Canal pour désenregistrer les clients
	// broadcast chan *Message // Si vous avez besoin de diffuser à tous (moins utile pour ce cas d'usage)

	// Mutex pour protéger l'accès à la map clients
	mu sync.RWMutex

	// Référence au handler métier pour traiter les messages entrants
	messageHandler func(msg *Message, client *connection) error
}

// newHub crée un nouveau Hub.
func newHub(handler func(msg *Message, client *connection) error) *Hub {
	return &Hub{
		clients:    make(map[*connection]bool),
		register:   make(chan *connection),
		unregister: make(chan *connection),
		// broadcast:  make(chan *Message),
		messageHandler: handler,
	}
}

// run démarre la boucle principale du Hub pour gérer les enregistrements/désenregistrements.
// Doit être lancée dans une goroutine.
func (h *Hub) run() {
	log.Println("Hub: Starting run loop")
	for {
		select {
		case conn := <-h.register:
			h.mu.Lock()
			h.clients[conn] = true
			h.mu.Unlock()
			log.Printf("Hub: Client registered (%p). Total clients: %d\n", conn.ws, len(h.clients))

		case conn := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[conn]; ok {
				delete(h.clients, conn)
				// Important: Fermer le canal d'envoi de la connexion pour arrêter sa writePump
				conn.closeSend()
				log.Printf("Hub: Client unregistered (%p). Total clients: %d\n", conn.ws, len(h.clients))
			} else {
				log.Printf("Hub: Unregister request for non-existent client (%p)\n", conn.ws)
			}
			h.mu.Unlock()

			/* // Logique de broadcast si nécessaire
			case message := <-h.broadcast:
				h.mu.RLock()
				for conn := range h.clients {
					select {
					case conn.send <- message:
					default:
						log.Printf("Hub: Broadcast failed for client %p, closing its send channel.\n", conn.ws)
						close(conn.send) // Ferme le canal pour ce client lent/mort
						delete(h.clients, conn) // Supprimer immédiatement ? Attention à l'itération RLock
					}
				}
				h.mu.RUnlock()
			*/
		}
	}
}

// handleDisconnect est appelé par la readPump d'une connexion lorsqu'elle se termine.
func (h *Hub) handleDisconnect(conn *connection) {
	h.unregister <- conn
}

// handleIncomingMessage est passé à la readPump pour traiter les messages reçus.
func (h *Hub) handleIncomingMessage(msg *Message, conn *connection) error {
	// Logique métier déléguée au handler fourni lors de la création du hub
	if h.messageHandler != nil {
		return h.messageHandler(msg, conn)
	}
	log.Printf("Hub: No message handler configured, dropping message type %s from %p\n", msg.Type, conn.ws)
	return nil // Ne pas retourner d'erreur pour ne pas fermer la connexion
}