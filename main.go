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
}

var (
	clients = make(map[*Client]bool)
	mutex   = &sync.Mutex{}
)

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("WebSocket upgrade error:", err)
		return
	}

	client := &Client{
		conn:         conn,
		lastActivity: time.Now(),
	}

	// Добавляем клиента
	mutex.Lock()
	clients[client] = true
	mutex.Unlock()

	// Запускаем пинг-понг для проверки соединения
	go pingPongHandler(client)

	defer func() {
		cleanupClient(client)
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
			handleICE(client, data["candidate"].(map[string]interface{}))
		case "pong":
			// Обработка pong-сообщения
			continue
		}
	}
}

func pingPongHandler(client *Client) {
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

			if err := client.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				log.Println("Ping error:", err)
				cleanupClient(client)
				return
			}
		}
	}
}

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

func handleOffer(client *Client, sdp string) {
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
		if err := client.conn.WriteJSON(map[string]interface{}{
			"type":      "ice",
			"candidate": c.ToJSON(),
		}); err != nil {
			log.Println("Write ICE candidate error:", err)
		}
	})

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Printf("Connection state changed: %s\n", s.String())
		if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateClosed {
			cleanupClient(client)
		}
	})

	dataChannel, err := pc.CreateDataChannel("chat", nil)
	if err != nil {
		log.Println("DataChannel error:", err)
		return
	}

	dataChannel.OnOpen(func() {
		log.Println("DataChannel opened!")
		if err := dataChannel.Send([]byte("Hello from Go server!")); err != nil {
			log.Println("Send welcome message error:", err)
		}
	})

	dataChannel.OnMessage(func(msg webrtc.DataChannelMessage) {
		log.Printf("Received message: %s\n", string(msg.Data))
		if err := dataChannel.Send([]byte("Echo: " + string(msg.Data))); err != nil {
			log.Println("Send echo error:", err)
		}
	})

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

	if err := client.conn.WriteJSON(map[string]interface{}{
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

	candidateMap, ok := candidate["candidate"].(map[string]interface{})
	if !ok {
		log.Println("Invalid candidate format")
		return
	}

	iceCandidate := webrtc.ICECandidateInit{
		Candidate: candidateMap["candidate"].(string),
	}

	if sdpMid, ok := candidateMap["sdpMid"].(string); ok {
		iceCandidate.SDPMid = &sdpMid
	}

	if sdpMLineIndex, ok := candidateMap["sdpMLineIndex"].(float64); ok {
		idx := uint16(sdpMLineIndex)
		iceCandidate.SDPMLineIndex = &idx
	}

	if err := client.pc.AddICECandidate(iceCandidate); err != nil {
		log.Println("AddICECandidate error:", err)
	}
}

func main() {
	http.HandleFunc("/ws", handleWebSocket)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Server is healthy"))
	})

	log.Println("Server starting on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatal("Server failed:", err)
	}
}
