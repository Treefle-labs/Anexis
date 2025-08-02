package socket

import (
	"log"
	"sync"
)

type Hub struct {
	clients    map[*connection]bool // List of connection registered
	register   chan *connection     // Channel for connection registration
	unregister chan *connection     // Channel for connection removing
	broadcast  chan *Message        // Diffusing message for all registered instance

	mu sync.RWMutex

	// Handler for incoming message
	messageHandler func(msg *Message, client *connection) error
}


func newHub(handler func(msg *Message, client *connection) error) *Hub {
	return &Hub{
		clients:    make(map[*connection]bool),
		register:   make(chan *connection),
		unregister: make(chan *connection),
		// broadcast:  make(chan *Message),
		messageHandler: handler,
	}
}

// handling registration/removing clients connection asynchronously
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
				conn.closeSend()
				log.Printf("Hub: Client unregistered (%p). Total clients: %d\n", conn.ws, len(h.clients))
			} else {
				log.Printf("Hub: Unregister request for non-existent client (%p)\n", conn.ws)
			}
			h.mu.Unlock()

		case message := <-h.broadcast:
			h.mu.RLock()
			for conn := range h.clients {
				select {
				case conn.send <- message:
				default:
					log.Printf("Hub: Broadcast failed for client %p, closing its send channel.\n", conn.ws)
					close(conn.send)       
					delete(h.clients, conn)
				}
			}
			h.mu.RUnlock()

		}
	}
}

// Calling this handler if a connection is disconnected
func (h *Hub) handleDisconnect(conn *connection) {
	h.unregister <- conn
}

// handler passed to readPump for incoming message.
func (h *Hub) handleIncomingMessage(msg *Message, conn *connection) error {
	if h.messageHandler != nil {
		return h.messageHandler(msg, conn)
	}
	log.Printf("Hub: No message handler configured, dropping message type %s from %p\n", msg.Type, conn.ws)
	return nil
}
