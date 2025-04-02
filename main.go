package main

import (
	"encoding/json"
	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	"log"
	"net/http"
	"sync"
	"time"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type Client struct {
	conn *websocket.Conn
	pc   *webrtc.PeerConnection
	mu   sync.Mutex
	dc   *webrtc.DataChannel
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

	log.Printf("New WebSocket connection from %s", r.RemoteAddr)

	defer func() {
		clientsMu.Lock()
		delete(clients, client)
		clientsMu.Unlock()
		conn.Close()
		if client.pc != nil {
			client.pc.Close()
		}
		log.Printf("Closed connection from %s", r.RemoteAddr)
	}()

	conn.SetReadDeadline(time.Now().Add(5 * time.Minute))

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			log.Println("Read error:", err)
			return
		}

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
		case "chat":
			if client.dc != nil && client.dc.ReadyState() == webrtc.DataChannelStateOpen {
				client.dc.SendText(data["message"].(string))
			}
		}
	}
}

func handleOffer(client *Client, sdp string) {
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs:       []string{"turn:213.184.249.66:3478"},
				Username:   "user1",
				Credential: "pass1",
			},
			{
				URLs:       []string{"turns:213.184.249.66:5349"},
				Username:   "user1",
				Credential: "pass1",
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
			return
		}

		candidate := c.ToJSON()
		if err := client.sendJSON(map[string]interface{}{
			"type":      "ice",
			"candidate": candidate,
		}); err != nil {
			log.Println("Failed to send ICE candidate:", err)
		}
	})

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("ICE Connection State changed: %s", state.String())
	})

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Println("Got remote track")
	})

	pc.OnDataChannel(func(d *webrtc.DataChannel) {
		client.dc = d
		log.Printf("New DataChannel %s %d", d.Label(), d.ID())

		d.OnOpen(func() {
			log.Printf("Data channel '%s'-'%d' open", d.Label(), d.ID())
		})

		d.OnMessage(func(msg webrtc.DataChannelMessage) {
			log.Printf("Message from DataChannel '%s': %s", d.Label(), string(msg.Data))
		})
	})

	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
		log.Println("AddTransceiverFromKind error:", err)
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

	<-webrtc.GatheringCompletePromise(pc)

	if err := client.sendJSON(map[string]interface{}{
		"type": "answer",
		"sdp":  pc.LocalDescription().SDP,
	}); err != nil {
		log.Println("Failed to send answer:", err)
	}
}

func handleICE(client *Client, candidate map[string]interface{}) {
	if client.pc == nil {
		log.Println("PeerConnection is nil")
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
		log.Println("Failed to add ICE candidate:", err)
	}
}

func main() {
	http.HandleFunc("/ws", handleWebSocket)
	http.Handle("/", http.FileServer(http.Dir("./static")))

	log.Println("Server starting on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
