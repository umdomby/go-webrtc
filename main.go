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
	mu           sync.Mutex
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
			cleanupClient(client)
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
			log.Println("Received offer")
			handleOffer(client, data["sdp"].(string))
		case "ice":
			candidate := data["candidate"].(map[string]interface{})
			log.Println("Received ICE candidate:", candidate)
			handleICE(client, candidate)
		}
	}
}

func handleOffer(client *Client, sdp string) {
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
			{
				URLs:           []string{"turn:213.184.249.66:3478"},
				Username:       "user1",
				Credential:     "pass1",
				CredentialType: webrtc.ICECredentialTypePassword,
			},
			{
				URLs:           []string{"turns:213.184.249.66:5349"},
				Username:       "user1",
				Credential:     "pass1",
				CredentialType: webrtc.ICECredentialTypePassword,
			},
		},
		ICETransportPolicy: webrtc.ICETransportPolicyAll,
	}

	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		log.Println("PeerConnection error:", err)
		return
	}

	client.pc = pc

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			log.Println("ICE gathering complete")
			return
		}

		candidate := c.ToJSON()
		log.Println("Sending ICE candidate:", candidate)

		if err := client.sendJSON(map[string]interface{}{
			"type":      "ice",
			"candidate": candidate,
		}); err != nil {
			log.Println("Failed to send ICE candidate:", err)
		}
	})

	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		log.Println("ICE connection state changed:", s)
	})

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Println("PeerConnection state changed:", s)
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

	// Enable better logging of ICE candidates
	gatherComplete := webrtc.GatheringCompletePromise(pc)

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		log.Println("CreateAnswer error:", err)
		return
	}

	if err := pc.SetLocalDescription(answer); err != nil {
		log.Println("SetLocalDescription error:", err)
		return
	}

	// Wait for ICE gathering to complete
	<-gatherComplete

	// Get the updated local description with all ICE candidates
	finalAnswer := pc.LocalDescription()

	log.Println("Sending answer with TURN support")
	if err := client.sendJSON(map[string]interface{}{
		"type": "answer",
		"sdp":  finalAnswer.SDP,
	}); err != nil {
		log.Println("Failed to send answer:", err)
	}
}

func handleICE(client *Client, candidate map[string]interface{}) {
	if client.pc == nil {
		log.Println("PeerConnection is nil, ignoring ICE candidate")
		return
	}

	iceCandidate := webrtc.ICECandidateInit{
		Candidate: candidate["candidate"].(string),
	}

	if sdpMid, ok := candidate["sdpMid"].(string); ok {
		iceCandidate.SDPMid = &sdpMid
	}
	if sdpMLineIndex, ok := candidate["sdpMLineIndex"].(float64); ok {
		idx := uint16(sdpMLineIndex)
		iceCandidate.SDPMLineIndex = &idx
	}

	log.Printf("Adding ICE candidate: %+v", iceCandidate)
	if err := client.pc.AddICECandidate(iceCandidate); err != nil {
		log.Println("Failed to add ICE candidate:", err)
	}
}

func cleanupClient(client *Client) {
	mutex.Lock()
	defer mutex.Unlock()

	if _, ok := clients[client]; ok {
		if client.pc != nil {
			client.pc.Close()
			log.Println("PeerConnection closed")
		}
		if client.conn != nil {
			client.conn.Close()
			log.Println("WebSocket connection closed")
		}
		delete(clients, client)
		log.Println("Client cleaned up")
	}
}

func main() {
	http.HandleFunc("/ws", handleWebSocket)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("WebRTC Signaling Server with TURN support"))
	})

	log.Println("Server starting on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
