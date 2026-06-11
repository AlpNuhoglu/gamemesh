// wsclient is a demo/E2E client: it registers two players, connects both to
// the WebSocket gateway, queues both for matchmaking and waits until each
// receives the MatchFound event. Exits 0 on success.
//
// Usage:
//
//	go run ./scripts/wsclient -api http://localhost:8080 -ws ws://localhost:8084
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

type message struct {
	Type string          `json:"type"`
	Room string          `json:"room,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

func main() {
	apiURL := flag.String("api", "http://localhost:8080", "API gateway base URL")
	wsURL := flag.String("ws", "ws://localhost:8084", "WebSocket gateway base URL")
	flag.Parse()

	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		log.Fatalf("crypto/rand failed: %v", err)
	}
	suffix := n.String()
	tokenA := mustToken(*apiURL, "wsdemoa"+suffix)
	tokenB := mustToken(*apiURL, "wsdemob"+suffix)

	connA := mustDial(*wsURL, tokenA)
	defer connA.Close()
	connB := mustDial(*wsURL, tokenB)
	defer connB.Close()
	log.Println("both websocket clients connected")

	// Queue both players within the rank window.
	mustPost(*apiURL+"/api/v1/queue", tokenA, `{"rank":1000}`)
	mustPost(*apiURL+"/api/v1/queue", tokenB, `{"rank":1020}`)
	log.Println("both players queued; waiting for MatchFound...")

	roomA := waitForMatch(connA, "A")
	roomB := waitForMatch(connB, "B")
	if roomA != roomB {
		log.Fatalf("players matched into different rooms: %s vs %s", roomA, roomB)
	}
	fmt.Printf("SUCCESS: both players received MatchFound for room %s\n", roomA)
}

func mustToken(apiURL, username string) string {
	body := fmt.Sprintf(`{"username":%q,"email":"%s@example.com","password":"password123"}`, username, username)
	resp, err := http.Post(apiURL+"/api/v1/auth/register", "application/json", bytes.NewBufferString(body))
	if err != nil || resp.StatusCode != http.StatusCreated {
		log.Fatalf("register %s failed: %v (status %v)", username, err, statusOf(resp))
	}
	resp.Body.Close()

	loginBody := fmt.Sprintf(`{"identifier":%q,"password":"password123"}`, username)
	resp, err = http.Post(apiURL+"/api/v1/auth/login", "application/json", bytes.NewBufferString(loginBody))
	if err != nil || resp.StatusCode != http.StatusOK {
		log.Fatalf("login %s failed: %v (status %v)", username, err, statusOf(resp))
	}
	defer resp.Body.Close()
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		log.Fatalf("decode login response: %v", err)
	}
	return out.Token
}

func mustDial(wsURL, token string) *websocket.Conn {
	conn, _, err := websocket.DefaultDialer.Dial(wsURL+"/ws?token="+token, nil)
	if err != nil {
		log.Fatalf("websocket dial failed: %v", err)
	}
	return conn
}

func mustPost(url, token, body string) {
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		log.Fatalf("POST %s failed: %v (status %v)", url, err, statusOf(resp))
	}
	resp.Body.Close()
}

func waitForMatch(conn *websocket.Conn, name string) string {
	deadline := time.Now().Add(30 * time.Second)
	_ = conn.SetReadDeadline(deadline)
	for {
		var msg message
		if err := conn.ReadJSON(&msg); err != nil {
			log.Fatalf("client %s: no MatchFound within 30s: %v", name, err)
		}
		log.Printf("client %s received: %s (room %s)", name, msg.Type, msg.Room)
		if msg.Type == "MatchFound" {
			return msg.Room
		}
	}
}

func statusOf(resp *http.Response) any {
	if resp == nil {
		return "no response"
	}
	return resp.StatusCode
}
