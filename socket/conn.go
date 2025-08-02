package socket

import (
	"encoding/json"
	"log"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// Max time for any message writing
	writeWait = 10 * time.Second
	// Max time for the next peer message reading.
	pongWait = 60 * time.Second
	// Sending ping to the server after this period. Must be low than pongWait.
	pingPeriod = (pongWait * 9) / 10
	// Max message body.
	maxMessageSize = 8192 // Adjust if the body size can be consequent (ex: build spec)
)

type connection struct {
	ws   *websocket.Conn
	send chan *Message // Channel for writing the i/o message
}

// creating a new connection struct.
func newConnection(ws *websocket.Conn) *connection {
	return &connection{
		ws:   ws,
		send: make(chan *Message, 256),
	}
}

// fetching message from the channels 'send' to the WebSocket connection.
func (c *connection) write(msgType int, payload []byte) error {
	c.ws.SetWriteDeadline(time.Now().Add(writeWait))
	return c.ws.WriteMessage(msgType, payload)
}

// Handling sorting and periodical ping messages to the server
func (c *connection) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.ws.Close()
		log.Println("writePump: Stopped and closed WebSocket connection")
	}()

	for {
		select {
		case message, ok := <-c.send:
			if !ok {
				log.Println("writePump: Send channel closed, closing connection.")
				c.write(websocket.CloseMessage, []byte{})
				return
			}

			c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			w, err := c.ws.NextWriter(websocket.TextMessage)
			if err != nil {
				log.Printf("writePump: Error getting next writer: %v\n", err)
				return
			}
			jsonBytes, err := json.Marshal(message)
			if err != nil {
				log.Printf("writePump: Error marshaling message type %s: %v\n", message.Type, err)
				// Don't return try to send the next message
				w.Close() // Close the actual writer
				continue
			}

			_, err = w.Write(jsonBytes)
			if err != nil {
				log.Printf("writePump: Error writing JSON: %v\n", err)
				w.Close()
			}

			if err := w.Close(); err != nil {
				log.Printf("writePump: Error closing writer: %v\n", err)
				return
			}
			log.Printf("writePump: Sent message type %s", message.Type) // Debug

		case <-ticker.C:
			// Sending a periodical ping message
			log.Println("writePump: Sending ping") // Debug
			if err := c.write(websocket.PingMessage, nil); err != nil {
				log.Printf("writePump: Error sending ping: %v\n", err)
				return
			}
		}
	}
}

// Handling entering message
func (c *connection) readPump(handler func(msg *Message, conn *connection) error, disconnect func(conn *connection)) {
	defer func() {
		disconnect(c)
		c.ws.Close()
		log.Println("readPump: Stopped and closed WebSocket connection")
	}()

	c.ws.SetReadLimit(maxMessageSize)
	c.ws.SetReadDeadline(time.Now().Add(pongWait))
	c.ws.SetPongHandler(func(string) error {
		log.Println("readPump: Received pong") // Debug
		c.ws.SetReadDeadline(time.Now().Add(pongWait))
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
			break
		}

		// Ignore non text message
		if msgType != websocket.TextMessage {
			log.Printf("readPump: Received non-text message type: %d\n", msgType)
			continue
		}

		log.Printf("readPump: Received raw message: %s", string(messageBytes)) // Debug

		var msg Message
		if err := json.Unmarshal(messageBytes, &msg); err != nil {
			log.Printf("readPump: Error unmarshaling message: %v --- Raw: %s\n", err, string(messageBytes))
			errMsg := NewErrorMessage("", "Invalid message format", err.Error())
			c.send <- errMsg
			continue
		}

		if err := handler(&msg, c); err != nil {
			log.Printf("readPump: Error handling message type %s: %v\n", msg.Type, err)
			errMsg := NewErrorMessage(msg.RequestID, "Failed to handle request", err.Error())
			c.send <- errMsg
		}

		c.ws.SetReadDeadline(time.Now().Add(pongWait))
	}
}

// sending message asynchronously via the websocket.
func (c *connection) sendMsg(msg *Message) {
	select {
	case c.send <- msg:
	default:
		log.Printf("Warning: Send channel full for connection %p. Message type %s dropped.\n", c.ws, msg.Type)
	}
}

// closing the send channel and stopping the writePump function.
func (c *connection) closeSend() {
	close(c.send)
}
