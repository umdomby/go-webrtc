package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type Client struct {
	conn         *websocket.Conn
	pc           *webrtc.PeerConnection
	lastActivity time.Time
	mu           sync.Mutex // Добавляем мьютекс для безопасной записи
}

var (
	clients = make(map[*Client]bool)
	mutex   = &sync.Mutex{}
)

func (c *Client) sendJSON(data interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteJSON(data)
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("WebSocket upgrade error:", err)
		return
	}

	log.Println("New WebSocket connection established")

	client := &Client{
		conn:         conn,
		lastActivity: time.Now(),
	}

	mutex.Lock()
	clients[client] = true
	mutex.Unlock()

	defer cleanupClient(client)

	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		client.lastActivity = time.Now()
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if time.Since(client.lastActivity) > 45*time.Second {
					log.Println("Connection timeout, closing...")
					cleanupClient(client)
					return
				}

				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					log.Println("Ping error:", err)
					return
				}
			}
		}
	}()

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			log.Println("Read error:", err)
			return
		}

		client.lastActivity = time.Now()

		var data map[string]interface{}
		if err := json.Unmarshal(msg, &data); err != nil {
			log.Println("JSON unmarshal error:", err)
			continue
		}

		switch data["type"] {
		case "offer":
			handleOffer(client, data["sdp"].(string))
		case "ice":
			candidate := data["candidate"].(map[string]interface{})
			handleICE(client, candidate)
		}
	}
}

func handleOffer(client *Client, sdp string) {
	log.Println("Received offer, creating PeerConnection")

	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}

	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		log.Println("PeerConnection error:", err)
		return
	}

	client.pc = pc

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}

		if err := client.sendJSON(map[string]interface{}{
			"type":      "ice",
			"candidate": c.ToJSON(),
		}); err != nil {
			log.Println("Write ICE candidate error:", err)
		}
	})

	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		log.Printf("ICE Connection State changed: %s\n", s.String())
	})

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Printf("PeerConnection State changed: %s\n", s.String())
	})

	if _, err := pc.CreateDataChannel("chat", nil); err != nil {
		log.Println("DataChannel error:", err)
	}

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdp,
	}); err != nil {
		log.Println("SetRemoteDescription error:", err)
		return
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		log.Println("CreateAnswer error:", err)
		return
	}

	if err := pc.SetLocalDescription(answer); err != nil {
		log.Println("SetLocalDescription error:", err)
		return
	}

	if err := client.sendJSON(map[string]interface{}{
		"type": "answer",
		"sdp":  answer.SDP,
	}); err != nil {
		log.Println("Send answer error:", err)
	}
}

func handleICE(client *Client, candidate map[string]interface{}) {
	if client.pc == nil {
		return
	}

	iceCandidate := webrtc.ICECandidateInit{
		Candidate:        candidate["candidate"].(string),
		SDPMid:           stringPtr(candidate["sdpMid"].(string)),
		SDPMLineIndex:    uint16Ptr(uint16(candidate["sdpMLineIndex"].(float64))),
		UsernameFragment: stringPtr(candidate["usernameFragment"].(string)),
	}

	if err := client.pc.AddICECandidate(iceCandidate); err != nil {
		log.Println("AddICECandidate error:", err)
	}
}

func stringPtr(s string) *string { return &s }
func uint16Ptr(u uint16) *uint16 { return &u }

func cleanupClient(client *Client) {
	mutex.Lock()
	defer mutex.Unlock()

	if _, ok := clients[client]; ok {
		if client.pc != nil {
			client.pc.Close()
		}
		if client.conn != nil {
			client.conn.Close()
		}
		delete(clients, client)
		log.Println("Client cleaned up")
	}
}

func main() {
	http.HandleFunc("/ws", handleWebSocket)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("WebRTC Signaling Server"))
	})

	log.Println("Server starting on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
