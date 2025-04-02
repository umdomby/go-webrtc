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
	conn *websocket.Conn
	pc   *webrtc.PeerConnection
	mu   sync.Mutex
}

var (
	clients   = make(map[*Client]bool)
	clientsMu sync.Mutex
)

func (c *Client) sendJSON(v interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteJSON(v)
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("WebSocket upgrade error:", err)
		return
	}

	client := &Client{conn: conn}
	clientsMu.Lock()
	clients[client] = true
	clientsMu.Unlock()

	log.Printf("New connection from %s", r.RemoteAddr)

	// Настройка таймаутов
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	defer func() {
		clientsMu.Lock()
		delete(clients, client)
		clientsMu.Unlock()
		conn.Close()
		if client.pc != nil {
			client.pc.Close()
		}
		log.Printf("Connection closed from %s", r.RemoteAddr)
	}()

	// Пинг-понг для поддержания соединения
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	go func() {
		for range ticker.C {
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}()

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway) {
				log.Printf("WebSocket error: %v", err)
			}
			return
		}

		var data map[string]interface{}
		if err := json.Unmarshal(msg, &data); err != nil {
			log.Println("JSON decode error:", err)
			continue
		}

		switch data["type"] {
		case "offer":
			go handleOffer(client, data["sdp"].(string))
		case "ice":
			candidate := data["candidate"].(map[string]interface{})
			go handleICE(client, candidate)
		}
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
		client.sendJSON(map[string]interface{}{
			"type":      "ice",
			"candidate": c.ToJSON(),
		})
	})

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("ICE state changed: %s", state)
	})

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Printf("Track received: %s", track.Kind())
	})

	// Добавляем транспондеры
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
		log.Println("AddTransceiver video error:", err)
	}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
		log.Println("AddTransceiver audio error:", err)
	}

	// Устанавливаем удаленное описание
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdp,
	}); err != nil {
		log.Println("SetRemoteDescription error:", err)
		return
	}

	// Создаем ответ
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		log.Println("CreateAnswer error:", err)
		return
	}

	// Устанавливаем локальное описание
	if err := pc.SetLocalDescription(answer); err != nil {
		log.Println("SetLocalDescription error:", err)
		return
	}

	// Отправляем ответ
	if err := client.sendJSON(map[string]interface{}{
		"type": "answer",
		"sdp":  pc.LocalDescription().SDP,
	}); err != nil {
		log.Println("Send answer error:", err)
	}
}

func handleICE(client *Client, candidate map[string]interface{}) {
	if client.pc == nil {
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

	if err := client.pc.AddICECandidate(iceCandidate); err != nil {
		log.Println("AddICECandidate error:", err)
	}
}

func main() {
	http.HandleFunc("/ws", handleWebSocket)
	http.Handle("/", http.FileServer(http.Dir("./static")))

	server := &http.Server{
		Addr:              ":8080",
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Println("Server starting on :8080")
	if err := server.ListenAndServe(); err != nil {
		log.Fatal("Server failed:", err)
	}
}
