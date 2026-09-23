// Command example demonstrates integrating a game with open-lobby-relay.
//
// It implements both sides of PROTOCOL.md against a stand-in "game server"
// (a TCP echo) so it runs end to end on one machine.
//
//	go run ./docs/example host          # registers, allocates, tunnels, bridges
//	go run ./docs/example join <room-id> # authorizes and connects to the relay
//
// Start the relay first (see the README), then run "host", copy the printed
// room id, and run "join" with it.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

const defaultServer = "http://localhost:8080"

func main() {
	server := os.Getenv("RELAY_SERVER")
	if server == "" {
		server = defaultServer
	}
	if len(os.Args) < 2 {
		log.Fatal("usage: example host | example join <room-id>")
	}
	switch os.Args[1] {
	case "host":
		runHost(server)
	case "join":
		if len(os.Args) < 3 {
			log.Fatal("usage: example join <room-id>")
		}
		runJoin(server, os.Args[2])
	default:
		log.Fatalf("unknown command %q", os.Args[1]) // #nosec G706 -- %q quoted
	}
}

func runHost(server string) {
	// Stand-in for the real game server the relay will bridge to.
	gameLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = gameLn.Close() }()
	fmt.Println("game server listening on", gameLn.Addr())
	go echoAccept(gameLn)

	id, secret := mustRegister(server)
	fmt.Println("room id:", id, "(pass this to `example join`)")

	go heartbeatLoop(server, id, secret)

	relayHost, hostPort, joinerPort := mustAllocateRelay(server, id, secret)
	fmt.Printf("joiners connect to %s:%d\n", relayHost, joinerPort)

	// Persistent host tunnel.
	tunnel := mustDialAddr(net.JoinHostPort(relayHost, strconv.Itoa(hostPort)))
	defer func() { _ = tunnel.Close() }()
	writeHandshake(tunnel, 0x01, secret)

	// Each 0x01 byte means "a joiner is waiting; open a data connection".
	buf := make([]byte, 1)
	for {
		if _, err := io.ReadFull(tunnel, buf); err != nil {
			log.Fatal("tunnel closed: ", err)
		}
		if buf[0] != 0x01 {
			continue
		}
		go func() {
			data := mustDialAddr(net.JoinHostPort(relayHost, strconv.Itoa(hostPort)))
			writeHandshake(data, 0x02, secret)
			game := mustDialAddr(gameLn.Addr().String())
			bridge(data, game)
		}()
	}
}

func runJoin(server, roomID string) {
	// Authorize this IP (empty password for open rooms).
	doJSON(http.MethodPost, server+"/rooms/"+roomID+"/verify", "", map[string]string{"password": ""}, nil)

	var rooms []struct {
		ID        string `json:"id"`
		RelayHost string `json:"relay_host"`
		RelayPort int    `json:"relay_port"`
	}
	doJSON(http.MethodGet, server+"/rooms", "", nil, &rooms)

	for _, r := range rooms {
		if r.ID != roomID {
			continue
		}
		conn := mustDialAddr(net.JoinHostPort(r.RelayHost, strconv.Itoa(r.RelayPort)))
		defer func() { _ = conn.Close() }()
		fmt.Println("connected to relay; type a line and press enter (bytes are echoed)")
		go func() { _, _ = io.Copy(conn, os.Stdin) }()
		_, _ = io.Copy(os.Stdout, conn)
		return
	}
	log.Fatal("room not found")
}

func mustRegister(server string) (string, string) {
	var out struct {
		ID     string `json:"id"`
		Secret string `json:"secret"`
	}
	doJSON(http.MethodPost, server+"/rooms", "", map[string]interface{}{
		"name": "Example Game", "mode": "1v1", "version": "1.0", "max_players": 4,
	}, &out)
	return out.ID, out.Secret
}

func mustAllocateRelay(server, roomID, secret string) (host string, hostPort, joinerPort int) {
	var out struct {
		RelayHost string `json:"relay_host"`
		HostPort  int    `json:"host_port"`
		RelayPort int    `json:"relay_port"`
	}
	doJSON(http.MethodPost, server+"/relay", secret, map[string]string{"room_id": roomID}, &out)
	return out.RelayHost, out.HostPort, out.RelayPort
}

func heartbeatLoop(server, roomID, secret string) {
	for range time.Tick(10 * time.Second) {
		doJSON(http.MethodPost, server+"/heartbeat", secret, map[string]interface{}{
			"id": roomID, "player_count": 1,
		}, nil)
	}
}

func doJSON(method, url, token string, reqBody, out interface{}) {
	var body io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			log.Fatal(err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, body) // #nosec G704 -- example client; URL is the relay server the user chose
	if err != nil {
		log.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Room-Token", token)
	}
	resp, err := http.DefaultClient.Do(req) // #nosec G704 -- example client; URL is the relay server the user chose
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatal(err)
	}
	if resp.StatusCode >= 300 {
		log.Fatalf("%s %s: %s: %s", method, url, resp.Status, data) // #nosec G706 -- example client logging its own response
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			log.Fatal(err)
		}
	}
}

func writeHandshake(conn net.Conn, role byte, secret string) {
	if _, err := conn.Write(append([]byte{role}, []byte(secret)...)); err != nil {
		log.Fatal(err)
	}
}

func bridge(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
	_ = a.Close()
	_ = b.Close()
}

func mustDialAddr(addr string) net.Conn {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		log.Fatal(err)
	}
	return conn
}

func echoAccept(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func(conn net.Conn) {
			defer func() { _ = conn.Close() }()
			_, _ = io.Copy(conn, conn)
		}(c)
	}
}
