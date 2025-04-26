// socket/conn.go
package socket

import (
	"encoding/json"
	"log"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// Temps max pour écrire un message.
	writeWait = 10 * time.Second
	// Temps max pour lire le prochain message pong du peer.
	pongWait = 60 * time.Second
	// Envoyer des pings au peer avec cette période. Doit être inférieur à pongWait.
	pingPeriod = (pongWait * 9) / 10
	// Taille maximale des messages lus.
	maxMessageSize = 8192 // Ajuster si de gros messages sont attendus (ex: build spec)
)

// connection est un wrapper autour de websocket.Conn gérant les pompes lecture/écriture.
type connection struct {
	ws   *websocket.Conn
	send chan *Message // Channel pour envoyer des messages sortants
}

// newConnection crée une nouvelle structure de connexion.
func newConnection(ws *websocket.Conn) *connection {
	return &connection{
		ws:   ws,
		send: make(chan *Message, 256), // Buffer pour éviter blocage sur écritures rapides
	}
}

// write pompe les messages du channel 'send' vers la connexion WebSocket.
func (c *connection) write(msgType int, payload []byte) error {
	c.ws.SetWriteDeadline(time.Now().Add(writeWait))
	return c.ws.WriteMessage(msgType, payload)
}

// writePump gère l'écriture des messages et les pings périodiques.
// Doit être lancée dans une goroutine séparée par connexion.
func (c *connection) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.ws.Close() // Ferme la connexion WebSocket si la pompe d'écriture s'arrête
		log.Println("writePump: Stopped and closed WebSocket connection")
	}()

	for {
		select {
		case message, ok := <-c.send:
			if !ok {
				// Le channel 'send' a été fermé. Fermer la connexion.
				log.Println("writePump: Send channel closed, closing connection.")
				c.write(websocket.CloseMessage, []byte{})
				return
			}

			c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			w, err := c.ws.NextWriter(websocket.TextMessage) // Utiliser TextMessage pour JSON
			if err != nil {
				log.Printf("writePump: Error getting next writer: %v\n", err)
				return // Erreur critique, arrêter la pompe
			}
			jsonBytes, err := json.Marshal(message)
			if err != nil {
				log.Printf("writePump: Error marshaling message type %s: %v\n", message.Type, err)
				// Ne pas retourner, essayer d'envoyer le prochain message
				w.Close() // Fermer le writer actuel
				continue
			}

			_, err = w.Write(jsonBytes)
			if err != nil {
				log.Printf("writePump: Error writing JSON: %v\n", err)
				// Ne pas retourner ici, mais fermer le writer est important
			}

			// Fermer le writer pour flusher le message sur le réseau.
			if err := w.Close(); err != nil {
				log.Printf("writePump: Error closing writer: %v\n", err)
				return // Erreur critique, arrêter la pompe
			}
			// log.Printf("writePump: Sent message type %s", message.Type) // Debug

		case <-ticker.C:
			// Envoyer un ping périodique
			// log.Println("writePump: Sending ping") // Debug
			if err := c.write(websocket.PingMessage, nil); err != nil {
				log.Printf("writePump: Error sending ping: %v\n", err)
				return // Erreur critique, arrêter la pompe
			}
		}
	}
}

// readPump lit les messages entrants et les passe au handler fourni.
// Le handler est responsable du traitement métier.
// Doit être lancée dans une goroutine séparée par connexion.
func (c *connection) readPump(handler func(msg *Message, conn *connection) error, disconnect func(conn *connection)) {
	defer func() {
		disconnect(c) // S'assurer que la déconnexion est gérée
		c.ws.Close()  // Ferme la connexion WebSocket si la pompe de lecture s'arrête
		log.Println("readPump: Stopped and closed WebSocket connection")
	}()

	c.ws.SetReadLimit(maxMessageSize)
	c.ws.SetReadDeadline(time.Now().Add(pongWait)) // Attendre le premier message ou pong
	c.ws.SetPongHandler(func(string) error {
		// log.Println("readPump: Received pong") // Debug
		c.ws.SetReadDeadline(time.Now().Add(pongWait)) // Repousser l'échéance à la réception d'un pong
		return nil
	})

	for {
		msgType, messageBytes, err := c.ws.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure, websocket.CloseNormalClosure) {
				log.Printf("readPump: WebSocket read error: %v\n", err)
			} else if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("readPump: WebSocket closed normally: %v\n", err)
			} else {
				log.Printf("readPump: Unhandled WebSocket read error: %v\n", err)
			}
			break // Sortir de la boucle en cas d'erreur de lecture ou de fermeture
		}

		// Ignorer les messages non-texte pour le moment
		if msgType != websocket.TextMessage {
			log.Printf("readPump: Received non-text message type: %d\n", msgType)
			continue
		}

		// log.Printf("readPump: Received raw message: %s", string(messageBytes)) // Debug

		var msg Message
		if err := json.Unmarshal(messageBytes, &msg); err != nil {
			log.Printf("readPump: Error unmarshaling message: %v --- Raw: %s\n", err, string(messageBytes))
			// Envoyer un message d'erreur au client ? Ou juste ignorer ? Ignorons pour l'instant.
			// errMsg := NewErrorMessage("", "Invalid message format", err.Error())
			// c.send <- errMsg
			continue
		}

		// Appeler le handler métier avec le message décodé
		if err := handler(&msg, c); err != nil {
			log.Printf("readPump: Error handling message type %s: %v\n", msg.Type, err)
			// Envoyer une réponse d'erreur liée à la requête si possible
			errMsg := NewErrorMessage(msg.RequestID, "Failed to handle request", err.Error())
			c.send <- errMsg // Mettre dans le canal d'envoi
			// Faut-il fermer la connexion ici ? Probablement pas, laisser le client décider.
		}

		// Réinitialiser le délai de lecture après avoir traité un message (pas strictement nécessaire si on reçoit des pongs)
		// c.ws.SetReadDeadline(time.Now().Add(pongWait))
	}
}

// sendMsg envoie un message de manière asynchrone via le channel.
func (c *connection) sendMsg(msg *Message) {
	select {
	case c.send <- msg:
		// Message envoyé avec succès (mis en file d'attente)
	default:
		// Le buffer d'envoi est plein, le client est peut-être trop lent ou déconnecté
		log.Printf("Warning: Send channel full for connection %p. Message type %s dropped.\n", c.ws, msg.Type)
		// Potentiellement fermer la connexion ici si c'est un problème récurrent.
	}
}

// close ferme le canal d'envoi, ce qui arrêtera la writePump.
func (c *connection) closeSend() {
	close(c.send)
}