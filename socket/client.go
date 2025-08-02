package socket

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type Client struct {
	conn *connection // Shared wrapper for WebSocket connection

	// Incoming messages are pushed here by the readPump.
	// Users can read from this channel to process incoming messages.
	Incoming chan *Message // Public channel for incoming messages

	mu          sync.Mutex
	isConnected bool
	dialer      *websocket.Dialer
	connUrl     string
	headers     http.Header // For authentication or other headers

	// pendingRequests holds channels for requests that are waiting for a response.
	// Keyed by RequestID, so we can correlate responses.
	// This allows us to handle responses to specific requests.
	pendingRequests map[string]chan *Message
	pendingMu       sync.RWMutex
}

// Creating anew client for a websocket connection.
func NewClient() *Client {
	return &Client{
		Incoming:        make(chan *Message, 100), // Buffer for incoming messages
		dialer:          websocket.DefaultDialer,
		pendingRequests: make(map[string]chan *Message),
	}
}

// Connect to the given server url websocket with the provided headers.
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
	c.mu.Unlock()

	go c.conn.writePump()
	go c.conn.readPump(c.handleIncomingMessage, c.handleDisconnect)

	return nil
}

func (c *Client) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.isConnected
}

func (c *Client) handleIncomingMessage(msg *Message, conn *connection) error {
	log.Printf("Client: Received message type %s (ReqID: %s)\n", msg.Type, msg.RequestID) // Debug

	// Check if it's a pending request
	c.pendingMu.Lock()
	if msg.RequestID != "" {
		if respChan, ok := c.pendingRequests[msg.RequestID]; ok {
			log.Printf("Client: Correlated response for RequestID %s\n", msg.RequestID)
			select {
			case respChan <- msg:
			default:
				log.Printf("Warning: No listener for response channel of RequestID %s\n", msg.RequestID)
			}
			delete(c.pendingRequests, msg.RequestID)
			c.pendingMu.Unlock()
			return nil
		}
	}
	c.pendingMu.Unlock()

	select {
	case c.Incoming <- msg:
	default:
		log.Printf("Warning: Client Incoming channel full. Message type %s dropped.\n", msg.Type)
	}
	return nil
}

func (c *Client) handleDisconnect(conn *connection) {
	c.mu.Lock()
	if c.conn != conn {
		c.mu.Unlock()
		log.Printf("Client: Received disconnect signal for an old/stale connection (%p)\n", conn.ws)
		return
	}
	c.isConnected = false
	c.conn = nil
	log.Println("Client: Connection lost.")
	c.mu.Unlock()

	// Clean the pending request for this connection
	c.pendingMu.Lock()
	if len(c.pendingRequests) > 0 {
		log.Printf("Client: Cleaning up %d pending requests due to disconnect.\n", len(c.pendingRequests))
		for reqID, respChan := range c.pendingRequests {
			close(respChan)
			delete(c.pendingRequests, reqID)
		}
	}
	c.pendingMu.Unlock()
}

// sending message to the server asynchronously.
func (c *Client) Send(msg *Message) error {
	c.mu.Lock()
	conn := c.conn
	isConnected := c.isConnected
	c.mu.Unlock()

	if !isConnected || conn == nil {
		return fmt.Errorf("client not connected")
	}
	log.Printf("Client: Sending message type %s async\n", msg.Type) // Debug
	conn.sendMsg(msg)
	return nil
}

// sending a request and waiting for the response based on the RequestID.
func (c *Client) SendRequest(ctx context.Context, msgType EventType, payload any) (*Message, error) {
	c.mu.Lock()
	conn := c.conn
	isConnected := c.isConnected
	c.mu.Unlock()

	if !isConnected || conn == nil {
		return nil, fmt.Errorf("client not connected")
	}

	requestID := uuid.NewString()
	msg := NewMessage(msgType, requestID)
	if payload != nil {
		if err := msg.AddPayload(payload); err != nil {
			return nil, fmt.Errorf("failed to add payload for request %s: %w", requestID, err)
		}
	}

	respChan := make(chan *Message, 1)

	c.pendingMu.Lock()
	c.pendingRequests[requestID] = respChan
	c.pendingMu.Unlock()

	// Cleaning the request before the response (success, error, timeout)
	defer func() {
		c.pendingMu.Lock()
		delete(c.pendingRequests, requestID)
		c.pendingMu.Unlock()
	}()

	// Send the request
	log.Printf("Client: Sending request %s (Type: %s)\n", requestID, msg.Type)
	conn.sendMsg(msg)

	// Waiting for the response
	select {
	case resp := <-respChan:
		log.Printf("Client: Received response for request %s (Type: %s, Error: '%s')\n", requestID, resp.Type, resp.Error)
		if resp.Error != "" || resp.Type == EvtError {
			errMsg := resp.Error
			if errMsg == "" {
				errMsg = "received error event"
			}
			var errPayload ErrorPayload
			if resp.DecodePayload(&errPayload) == nil && errPayload.Details != "" {
				errMsg = fmt.Sprintf("%s: %s", errMsg, errPayload.Details)
			}
			return nil, fmt.Errorf("server error response for request %s: %s", requestID, errMsg)
		}
		return resp, nil

	case <-ctx.Done():
		log.Printf("Client: Context done while waiting for response to request %s: %v\n", requestID, ctx.Err())
		return nil, fmt.Errorf("request %s timed out or was canceled: %w", requestID, ctx.Err())
	}
}

// Close the websocket connection and stopping the client.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	log.Println("Client: Close called.")

	if c.conn != nil && c.isConnected {
		c.conn.closeSend()
	}
	c.isConnected = false
}
