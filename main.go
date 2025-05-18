package main

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/cors"
)

type Client struct {
	ID   string
	Conn *websocket.Conn
}

type Message struct {
	Type    string          `json:"type"`
	From    string          `json:"from"`
	To      string          `json:"to,omitempty"`
	Payload json.RawMessage `json:"payload"`
}

type Room struct {
	Clients map[string]*Client
	mu      sync.RWMutex
}

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true // Allow all origins in development
		},
	}
	rooms   = make(map[string]*Room)
	roomsMu sync.RWMutex
)

func generateTLSConfig() (*tls.Config, error) {
	// Generate certificate
	certFile := "cert.pem"
	keyFile := "key.pem"

	// Check if certificate files already exist
	if _, err := os.Stat(certFile); os.IsNotExist(err) {
		log.Println("Generating self-signed certificate...")
		// Generate certificate using go run command
		cmd := `go run $(go env GOROOT)/src/crypto/tls/generate_cert.go --host localhost,127.0.0.1,192.168.0.13`
		if err := exec.Command("cmd", "/C", cmd).Run(); err != nil {
			return nil, err
		}
	}

	// Load certificate
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
	}, nil
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	allowedOrigins := []string{}
	if origins := os.Getenv("ALLOWED_ORIGINS"); origins != "" {
		allowedOrigins = append(allowedOrigins, origins)
	} else {
		// Default development origins
		allowedOrigins = []string{
			"http://localhost:5173",
			"https://localhost:5173",
			"http://127.0.0.1:5173",
			"https://127.0.0.1:5173",
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", handleWebSocket)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	// Configure CORS
	c := cors.New(cors.Options{
		AllowedOrigins:   allowedOrigins,
		AllowedMethods:   []string{"GET", "POST"},
		AllowedHeaders:   []string{"*"},
		AllowCredentials: true,
	})

	handler := c.Handler(mux)

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           handler,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("Server starting on port %s", port)
	log.Fatal(server.ListenAndServe())
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Upgrade error: %v", err)
		return
	}
	defer conn.Close()

	// Generate a unique client ID
	clientID := generateClientID()
	client := &Client{ID: clientID, Conn: conn}

	// Get room ID from query parameter
	roomID := r.URL.Query().Get("room")
	if roomID == "" {
		log.Println("Room ID not provided")
		return
	}

	log.Printf("New client %s joining room %s", clientID, roomID)

	// Join or create room
	room := joinRoom(roomID, client)
	defer leaveRoom(roomID, clientID)

	// Log current room state
	log.Printf("Room %s now has %d clients: %v",
		roomID,
		len(room.Clients),
		getClientIDs(room),
	)

	// Send client their ID
	if err := client.Conn.WriteJSON(Message{
		Type:    "client-id",
		From:    "server",
		Payload: json.RawMessage(`{"id":"` + clientID + `"}`),
	}); err != nil {
		log.Printf("Error sending client ID: %v", err)
		return
	}

	// Notify others in the room
	broadcastToRoom(room, client, Message{
		Type: "user-joined",
		From: clientID,
	})

	for {
		var msg Message
		err := conn.ReadJSON(&msg)
		if err != nil {
			log.Printf("Client %s left room %s due to error: %v", clientID, roomID, err)
			break
		}

		msg.From = clientID
		log.Printf("Message from %s in room %s: type=%s, to=%s",
			clientID,
			roomID,
			msg.Type,
			msg.To,
		)
		handleMessage(room, &msg)
	}
}

func getClientIDs(room *Room) []string {
	ids := make([]string, 0, len(room.Clients))
	for id := range room.Clients {
		ids = append(ids, id)
	}
	return ids
}

func handleMessage(room *Room, msg *Message) {
	room.mu.RLock()
	defer room.mu.RUnlock()

	switch msg.Type {
	case "offer", "answer", "ice-candidate":
		// Forward the message to the specified peer
		if target, ok := room.Clients[msg.To]; ok {
			if err := target.Conn.WriteJSON(msg); err != nil {
				log.Printf("Error sending %s message from %s to %s: %v",
					msg.Type,
					msg.From,
					msg.To,
					err,
				)
			} else {
				log.Printf("Successfully forwarded %s message from %s to %s",
					msg.Type,
					msg.From,
					msg.To,
				)
			}
		} else {
			log.Printf("Target client %s not found in room for message type %s from %s",
				msg.To,
				msg.Type,
				msg.From,
			)
			// Log all clients in the room for debugging
			log.Printf("Available clients in room: %v", getClientIDs(room))
		}
	}
}

func joinRoom(roomID string, client *Client) *Room {
	roomsMu.Lock()
	defer roomsMu.Unlock()

	if rooms[roomID] == nil {
		log.Printf("Creating new room: %s", roomID)
		rooms[roomID] = &Room{
			Clients: make(map[string]*Client),
		}
	}

	room := rooms[roomID]
	room.mu.Lock()
	defer room.mu.Unlock()

	room.Clients[client.ID] = client

	// Send list of existing peers to the new client
	peers := make([]string, 0)
	for id := range room.Clients {
		if id != client.ID {
			peers = append(peers, id)
		}
	}

	log.Printf("Room %s - Current peers: %v", roomID, getClientIDs(room))
	log.Printf("Sending peer list to %s: %v", client.ID, peers)

	client.Conn.WriteJSON(Message{
		Type:    "peers",
		From:    "server",
		Payload: json.RawMessage(`{"peers":` + marshalPeers(peers) + `}`),
	})

	return room
}

func leaveRoom(roomID, clientID string) {
	roomsMu.Lock()
	defer roomsMu.Unlock()

	room, exists := rooms[roomID]
	if !exists {
		return
	}

	room.mu.Lock()
	defer room.mu.Unlock()

	delete(room.Clients, clientID)

	// Notify others that the client has left
	broadcastToRoom(room, nil, Message{
		Type: "user-left",
		From: clientID,
	})

	// Clean up empty rooms
	if len(room.Clients) == 0 {
		delete(rooms, roomID)
	}
}

func broadcastToRoom(room *Room, exclude *Client, msg Message) {
	room.mu.RLock()
	defer room.mu.RUnlock()

	log.Printf("Broadcasting message type %s from %s to room (excluding: %v)",
		msg.Type,
		msg.From,
		exclude != nil,
	)

	for id, client := range room.Clients {
		if exclude != nil && client.ID == exclude.ID {
			continue
		}
		if err := client.Conn.WriteJSON(msg); err != nil {
			log.Printf("Error broadcasting to client %s: %v", id, err)
		} else {
			log.Printf("Successfully broadcast to client %s", id)
		}
	}
}

func generateClientID() string {
	// Generate 8 random bytes
	bytes := make([]byte, 4)
	if _, err := rand.Read(bytes); err != nil {
		log.Printf("Error generating random bytes: %v", err)
		return "user-" + randomString(8)
	}
	return "user-" + hex.EncodeToString(bytes)
}

func randomString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[i%len(letters)]
	}
	return string(b)
}

func marshalPeers(peers []string) string {
	bytes, _ := json.Marshal(peers)
	return string(bytes)
}
