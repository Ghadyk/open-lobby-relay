package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	s := NewServer(Config{
		Port:               "0",
		PublicHost:         "relay.example.com",
		RelayPortMin:       20000,
		RelayPortMax:       20050,
		MaxRooms:           100,
		MaxRelays:          10,
		MaxConnsPerRelay:   8,
		RelayIdleTimeout:   300 * time.Second,
		RelayMaxBytes:      1 << 30,
		RelayConnRPM:       0,
		AllowPrivateHostIP: true,
		RateLimitRPM:       100000,
		RateLimitBurst:     1000,
		BcryptCost:         10,
		RequireJoinAuth:    false,
		JoinAuthTTL:        120 * time.Second,
	})
	t.Cleanup(s.closeAllRelays)
	return s
}

// relayAddrs allocates a relay and returns the host and joiner addresses.
func relayAddrs(t *testing.T, mux http.Handler, roomID, secret string) (string, string) {
	t.Helper()
	w := doRequest(t, mux, "POST", "/relay", map[string]string{"room_id": roomID}, secret)
	if w.Code != http.StatusCreated {
		t.Fatalf("relay: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var rr map[string]interface{}
	_ = json.NewDecoder(w.Body).Decode(&rr)
	return "127.0.0.1:" + strconv.Itoa(int(rr["host_port"].(float64))),
		"127.0.0.1:" + strconv.Itoa(int(rr["relay_port"].(float64)))
}

func doRequest(t *testing.T, mux http.Handler, method, path string, body interface{}, token string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	req.RemoteAddr = "1.2.3.4:56789"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("X-Room-Token", token)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func writeHandshake(t *testing.T, conn net.Conn, role byte, secret string) {
	t.Helper()
	if _, err := conn.Write(append([]byte{role}, []byte(secret)...)); err != nil {
		t.Fatalf("host handshake write failed: %v", err)
	}
}

func createRoom(t *testing.T, mux http.Handler, name string) (id, secret string) {
	t.Helper()
	w := doRequest(t, mux, "POST", "/rooms", map[string]interface{}{
		"name": name, "mode": "1v1", "version": "1.0", "max_players": 4,
	}, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("create room: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.NewDecoder(w.Body).Decode(&resp)
	return resp["id"].(string), resp["secret"].(string)
}

func startEchoServer(t *testing.T) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
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
	}()
	return ln, ln.Addr().String()
}

func TestHealth(t *testing.T) {
	s := newTestServer(t)
	w := doRequest(t, s.routes(), "GET", "/health", nil, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ok") {
		t.Fatalf("health: %d %s", w.Code, w.Body.String())
	}
}

func TestCreateRoom(t *testing.T) {
	s := newTestServer(t)
	mux := s.routes()
	w := doRequest(t, mux, "POST", "/rooms", map[string]interface{}{
		"name": "Test Game", "mode": "Commander", "version": "1.0", "max_players": 4,
	}, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp["id"] == "" || resp["secret"] == "" {
		t.Fatalf("expected id and secret, got %v", resp)
	}
	if resp["mode"] != "Commander" {
		t.Fatalf("expected mode echoed, got %v", resp["mode"])
	}
}

func TestCreateRoomValidation(t *testing.T) {
	s := newTestServer(t)
	mux := s.routes()
	cases := []map[string]interface{}{
		{"name": "", "max_players": 4},
		{"name": strings.Repeat("a", 101), "max_players": 4},
		{"name": "x", "max_players": 0},
		{"name": "x", "max_players": 999},
		{"name": "x", "max_players": 4, "mode": strings.Repeat("a", 51)},
		{"name": "x", "max_players": 4, "password": strings.Repeat("a", 73)},
	}
	for i, body := range cases {
		w := doRequest(t, mux, "POST", "/rooms", body, "")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("case %d: expected 400, got %d: %s", i, w.Code, w.Body.String())
		}
	}
}

func TestPrivateIPRejected(t *testing.T) {
	s := newTestServer(t)
	s.cfg.AllowPrivateHostIP = false
	req := httptest.NewRequest("POST", "/rooms", strings.NewReader(`{"name":"x","max_players":4}`))
	req.RemoteAddr = "127.0.0.1:56789"
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "host IP not allowed") {
		t.Fatalf("expected private IP rejection, got %d %s", w.Code, w.Body.String())
	}
}

func TestTrustProxyUsesLastForwardedFor(t *testing.T) {
	s := newTestServer(t)
	s.cfg.TrustProxy = true
	req := httptest.NewRequest("GET", "/rooms", nil)
	req.RemoteAddr = "127.0.0.1:1"
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 203.0.113.9")
	if got := s.extractIP(req); got != "203.0.113.9" {
		t.Fatalf("expected rightmost XFF entry, got %q", got)
	}
}

func TestExtractIPWithoutProxy(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest("GET", "/rooms", nil)
	req.RemoteAddr = "203.0.113.9:1"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := s.extractIP(req); got != "203.0.113.9" {
		t.Fatalf("expected RemoteAddr, got %q", got)
	}
}

func TestListRoomsHidesSecrets(t *testing.T) {
	s := newTestServer(t)
	mux := s.routes()
	createRoom(t, mux, "Game 1")
	w := doRequest(t, mux, "GET", "/rooms", nil, "")
	var list []map[string]interface{}
	_ = json.NewDecoder(w.Body).Decode(&list)
	if len(list) != 1 {
		t.Fatalf("expected 1 room, got %d", len(list))
	}
	if v, ok := list[0]["secret"]; ok && v != nil && v != "" {
		t.Fatal("secret must not be listed")
	}
}

func TestPasswordVerification(t *testing.T) {
	s := newTestServer(t)
	mux := s.routes()
	w := doRequest(t, mux, "POST", "/rooms", map[string]interface{}{
		"name": "Private", "max_players": 4, "has_password": true, "password": "hunter2",
	}, "")
	var resp map[string]interface{}
	_ = json.NewDecoder(w.Body).Decode(&resp)
	roomID := resp["id"].(string)

	w = doRequest(t, mux, "POST", "/rooms/"+roomID+"/verify", map[string]string{"password": "wrong"}, "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	w = doRequest(t, mux, "POST", "/rooms/"+roomID+"/verify", map[string]string{"password": "hunter2"}, "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestDeleteRoomAuthorization(t *testing.T) {
	s := newTestServer(t)
	mux := s.routes()
	roomID, secret := createRoom(t, mux, "Test")

	if w := doRequest(t, mux, "DELETE", "/rooms/"+roomID, nil, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	if w := doRequest(t, mux, "DELETE", "/rooms/"+roomID, nil, "wrong"); w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	if w := doRequest(t, mux, "DELETE", "/rooms/"+roomID, nil, secret); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestHeartbeatAuthorization(t *testing.T) {
	s := newTestServer(t)
	mux := s.routes()
	roomID, secret := createRoom(t, mux, "Test")

	if w := doRequest(t, mux, "POST", "/heartbeat", map[string]interface{}{"id": roomID, "player_count": 2}, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	if w := doRequest(t, mux, "POST", "/heartbeat", map[string]interface{}{"id": roomID, "player_count": 2}, "wrong"); w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	if w := doRequest(t, mux, "POST", "/heartbeat", map[string]interface{}{"id": roomID, "player_count": 2}, secret); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w := doRequest(t, mux, "POST", "/heartbeat", map[string]interface{}{"id": "missing", "player_count": 1}, "x"); w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestRelayAllocationAuthorization(t *testing.T) {
	s := newTestServer(t)
	mux := s.routes()
	roomID, secret := createRoom(t, mux, "Test")

	if w := doRequest(t, mux, "POST", "/relay", map[string]string{"room_id": roomID}, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	if w := doRequest(t, mux, "POST", "/relay", map[string]string{"room_id": roomID}, "wrong"); w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	w := doRequest(t, mux, "POST", "/relay", map[string]string{"room_id": roomID}, secret)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var rr map[string]interface{}
	_ = json.NewDecoder(w.Body).Decode(&rr)
	if rr["host_port"] == nil || rr["relay_port"] == nil || rr["relay_host"] != "relay.example.com" {
		t.Fatalf("unexpected relay response: %v", rr)
	}
}

func TestMaxRoomsEnforced(t *testing.T) {
	s := newTestServer(t)
	s.cfg.MaxRooms = 2
	mux := s.routes()
	createRoom(t, mux, "A")
	createRoom(t, mux, "B")
	w := doRequest(t, mux, "POST", "/rooms", map[string]interface{}{"name": "C", "max_players": 4}, "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

func TestMultiJoinerRelay(t *testing.T) {
	s := newTestServer(t)
	mux := s.routes()
	_, gameAddr := startEchoServer(t)
	roomID, secret := createRoom(t, mux, "Multi")

	hostAddr, joinerAddr := relayAddrs(t, mux, roomID, secret)

	tunnel, err := net.DialTimeout("tcp", hostAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("tunnel dial: %v", err)
	}
	defer func() { _ = tunnel.Close() }()
	writeHandshake(t, tunnel, 0x01, secret)
	time.Sleep(200 * time.Millisecond)

	for i := 0; i < 3; i++ {
		joiner, err := net.DialTimeout("tcp", joinerAddr, 5*time.Second)
		if err != nil {
			t.Fatalf("joiner %d dial: %v", i, err)
		}
		msg := []byte("hello " + strconv.Itoa(i))
		if _, err := joiner.Write(msg); err != nil {
			t.Fatalf("joiner %d write: %v", i, err)
		}
		time.Sleep(100 * time.Millisecond)

		dataConn, err := net.DialTimeout("tcp", hostAddr, 5*time.Second)
		if err != nil {
			t.Fatalf("data dial: %v", err)
		}
		writeHandshake(t, dataConn, 0x02, secret)
		gameConn, err := net.DialTimeout("tcp", gameAddr, 5*time.Second)
		if err != nil {
			t.Fatalf("game dial: %v", err)
		}
		go func() {
			_, _ = io.Copy(dataConn, gameConn)
			_ = dataConn.Close()
			_ = gameConn.Close()
		}()
		go func() {
			_, _ = io.Copy(gameConn, dataConn)
			_ = dataConn.Close()
			_ = gameConn.Close()
		}()

		buf := make([]byte, len(msg))
		_ = joiner.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(joiner, buf); err != nil {
			t.Fatalf("joiner %d read: %v", i, err)
		}
		if !bytes.Equal(buf, msg) {
			t.Fatalf("joiner %d: expected %q, got %q", i, msg, buf)
		}
		_ = joiner.Close()
	}
}

func TestHostTunnelReconnect(t *testing.T) {
	s := newTestServer(t)
	mux := s.routes()
	_, gameAddr := startEchoServer(t)
	roomID, secret := createRoom(t, mux, "Reconnect")

	hostAddr, joinerAddr := relayAddrs(t, mux, roomID, secret)

	joinOnce := func() {
		joiner, err := net.DialTimeout("tcp", joinerAddr, 5*time.Second)
		if err != nil {
			t.Fatalf("joiner dial: %v", err)
		}
		defer func() { _ = joiner.Close() }()
		msg := []byte("ping")
		_, _ = joiner.Write(msg)
		time.Sleep(100 * time.Millisecond)

		dataConn, err := net.DialTimeout("tcp", hostAddr, 5*time.Second)
		if err != nil {
			t.Fatalf("data dial: %v", err)
		}
		writeHandshake(t, dataConn, 0x02, secret)
		gameConn, err := net.DialTimeout("tcp", gameAddr, 5*time.Second)
		if err != nil {
			t.Fatalf("game dial: %v", err)
		}
		go func() { _, _ = io.Copy(dataConn, gameConn); _ = dataConn.Close(); _ = gameConn.Close() }()
		go func() { _, _ = io.Copy(gameConn, dataConn); _ = dataConn.Close(); _ = gameConn.Close() }()

		buf := make([]byte, len(msg))
		_ = joiner.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(joiner, buf); err != nil {
			t.Fatalf("expected echo: %v", err)
		}
	}

	t1, _ := net.DialTimeout("tcp", hostAddr, 5*time.Second)
	writeHandshake(t, t1, 0x01, secret)
	time.Sleep(200 * time.Millisecond)
	joinOnce()

	_ = t1.Close()
	time.Sleep(100 * time.Millisecond)

	t2, _ := net.DialTimeout("tcp", hostAddr, 5*time.Second)
	defer func() { _ = t2.Close() }()
	writeHandshake(t, t2, 0x01, secret)
	time.Sleep(200 * time.Millisecond)
	joinOnce()
}

// roomUseRelay returns the relay advertisement fields for a room.
func roomUseRelay(t *testing.T, mux http.Handler, roomID string) (useRelay bool, host string, port int) {
	t.Helper()
	w := doRequest(t, mux, "GET", "/rooms", nil, "")
	var list []map[string]interface{}
	_ = json.NewDecoder(w.Body).Decode(&list)
	for _, r := range list {
		if r["id"] != roomID {
			continue
		}
		useRelay, _ = r["use_relay"].(bool)
		host, _ = r["relay_host"].(string)
		if p, ok := r["relay_port"].(float64); ok {
			port = int(p)
		}
		return useRelay, host, port
	}
	t.Fatalf("room %s not found in listing", roomID)
	return false, "", 0
}

func waitUseRelay(t *testing.T, mux http.Handler, roomID string, want bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if use, _, _ := roomUseRelay(t, mux, roomID); use == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	use, host, port := roomUseRelay(t, mux, roomID)
	t.Fatalf("use_relay = %v (host=%q port=%d), want %v", use, host, port, want)
}

// TestHostTunnelLossUnadvertises checks that dropping the host tunnel removes
// the relay from the room listing and that a reconnect re-advertises it.
func TestHostTunnelLossUnadvertises(t *testing.T) {
	s := newTestServer(t)
	mux := s.routes()
	roomID, secret := createRoom(t, mux, "Liveness")
	hostAddr, _ := relayAddrs(t, mux, roomID, secret)

	tunnel, err := net.DialTimeout("tcp", hostAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("tunnel dial: %v", err)
	}
	writeHandshake(t, tunnel, 0x01, secret)
	waitUseRelay(t, mux, roomID, true)

	_ = tunnel.Close()
	waitUseRelay(t, mux, roomID, false)

	t2, err := net.DialTimeout("tcp", hostAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("reconnect dial: %v", err)
	}
	defer func() { _ = t2.Close() }()
	writeHandshake(t, t2, 0x01, secret)
	waitUseRelay(t, mux, roomID, true)
}

// TestCloseAllRelaysUnadvertises checks that the kill switch clears the relay
// advertisement from every room.
func TestCloseAllRelaysUnadvertises(t *testing.T) {
	s := newTestServer(t)
	mux := s.routes()
	roomID, secret := createRoom(t, mux, "Kill")
	hostAddr, _ := relayAddrs(t, mux, roomID, secret)

	tunnel, err := net.DialTimeout("tcp", hostAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("tunnel dial: %v", err)
	}
	defer func() { _ = tunnel.Close() }()
	writeHandshake(t, tunnel, 0x01, secret)
	waitUseRelay(t, mux, roomID, true)

	s.closeAllRelays()

	use, host, port := roomUseRelay(t, mux, roomID)
	if use || host != "" || port != 0 {
		t.Fatalf("after close-all: use_relay=%v host=%q port=%d, want cleared", use, host, port)
	}
}

// TestHostHandshakeCap checks that more unauthenticated host connections than
// the per-relay cap allows are dropped instead of piling up.
func TestHostHandshakeCap(t *testing.T) {
	s := newTestServer(t)
	s.cfg.MaxConnsPerRelay = 2 // cap = 3 in-flight host handshakes
	mux := s.routes()
	roomID, secret := createRoom(t, mux, "Cap")
	hostAddr, _ := relayAddrs(t, mux, roomID, secret)

	slots := s.cfg.MaxConnsPerRelay + 1
	held := make([]net.Conn, 0, slots)
	defer func() {
		for _, c := range held {
			_ = c.Close()
		}
	}()
	// Open connections that never send the handshake, occupying every slot.
	for i := 0; i < slots; i++ {
		c, err := net.DialTimeout("tcp", hostAddr, 5*time.Second)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		held = append(held, c)
	}
	time.Sleep(300 * time.Millisecond)

	// The next one exceeds the cap and must be closed by the relay.
	extra, err := net.DialTimeout("tcp", hostAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("extra dial: %v", err)
	}
	defer func() { _ = extra.Close() }()
	_ = extra.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
	if _, err := extra.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected extra host connection to be dropped")
	}
}

func TestRelayRejectsBadHostSecret(t *testing.T) {
	s := newTestServer(t)
	mux := s.routes()
	roomID, secret := createRoom(t, mux, "Auth")
	hostAddr, _ := relayAddrs(t, mux, roomID, secret)

	bad, err := net.DialTimeout("tcp", hostAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	wrong := bytes.Repeat([]byte{'f'}, 64)
	_, _ = bad.Write(append([]byte{0x01}, wrong...))
	_ = bad.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := bad.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected bad-secret host connection to be closed")
	}
	_ = bad.Close()

	good, err := net.DialTimeout("tcp", hostAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = good.Close() }()
	writeHandshake(t, good, 0x01, secret)
	time.Sleep(300 * time.Millisecond)
}

func TestJoinAuthorization(t *testing.T) {
	s := newTestServer(t)
	s.cfg.RequireJoinAuth = true
	mux := s.routes()
	roomID, secret := createRoom(t, mux, "Gated")

	hostAddr, joinerAddr := relayAddrs(t, mux, roomID, secret)

	tunnel, err := net.DialTimeout("tcp", hostAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("tunnel dial: %v", err)
	}
	defer func() { _ = tunnel.Close() }()
	writeHandshake(t, tunnel, 0x01, secret)
	time.Sleep(200 * time.Millisecond)

	hostSignalled := func() bool {
		_ = tunnel.SetReadDeadline(time.Now().Add(700 * time.Millisecond))
		_, err := tunnel.Read(make([]byte, 1))
		_ = tunnel.SetReadDeadline(time.Time{})
		return err == nil
	}

	// Unauthorized joiner: rejected, host not signalled.
	j1, _ := net.DialTimeout("tcp", joinerAddr, 5*time.Second)
	_, _ = j1.Write([]byte{0x00})
	if hostSignalled() {
		t.Fatal("relay signalled host for unauthorized joiner")
	}
	_ = j1.Close()

	// Authorize 127.0.0.1 via /verify.
	req := httptest.NewRequest("POST", "/rooms/"+roomID+"/verify", strings.NewReader(`{"password":""}`))
	req.RemoteAddr = "127.0.0.1:4242"
	req.Header.Set("Content-Type", "application/json")
	vw := httptest.NewRecorder()
	mux.ServeHTTP(vw, req)
	if vw.Code != http.StatusOK {
		t.Fatalf("verify failed: %d %s", vw.Code, vw.Body.String())
	}

	j2, _ := net.DialTimeout("tcp", joinerAddr, 5*time.Second)
	defer func() { _ = j2.Close() }()
	_, _ = j2.Write([]byte{0x00})
	if !hostSignalled() {
		t.Fatal("relay did not accept authorized joiner")
	}
}

func TestRateLimitDisabledWhenZero(t *testing.T) {
	s := newTestServer(t)
	s.cfg.RateLimitRPM = 0
	mux := s.routes()
	for i := 0; i < 500; i++ {
		if w := doRequest(t, mux, "GET", "/rooms", nil, ""); w.Code == http.StatusTooManyRequests {
			t.Fatalf("expected rate limiting disabled, got 429 at %d", i)
		}
	}
}

func TestAdminAuthAndBans(t *testing.T) {
	s := newTestServer(t)
	s.cfg.AdminToken = "s3cret"
	mux := s.routes()

	// No token -> unauthorized.
	req := httptest.NewRequest("GET", "/admin/bans", nil)
	req.RemoteAddr = "1.2.3.4:1"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}

	admin := func(method, path string, body interface{}) *httptest.ResponseRecorder {
		var r io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			r = bytes.NewReader(b)
		}
		rq := httptest.NewRequest(method, path, r)
		rq.RemoteAddr = "1.2.3.4:1"
		rq.Header.Set("Authorization", "Bearer s3cret")
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, rq)
		return rr
	}

	if w := admin("POST", "/admin/bans", map[string]interface{}{"ip": "203.0.113.7"}); w.Code != http.StatusOK {
		t.Fatalf("ban: expected 200, got %d", w.Code)
	}
	if !s.bans.isBanned("203.0.113.7") {
		t.Fatal("expected IP to be banned")
	}
	if w := admin("GET", "/admin/bans", nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "203.0.113.7") {
		t.Fatalf("list bans: %d %s", w.Code, w.Body.String())
	}
	if w := admin("DELETE", "/admin/bans", map[string]interface{}{"ip": "203.0.113.7"}); w.Code != http.StatusOK {
		t.Fatalf("unban: expected 200, got %d", w.Code)
	}
	if s.bans.isBanned("203.0.113.7") {
		t.Fatal("expected IP to be unbanned")
	}
}

func TestAdminDisabledWithoutToken(t *testing.T) {
	s := newTestServer(t)
	mux := s.routes()
	req := httptest.NewRequest("GET", "/admin/rooms", nil)
	req.RemoteAddr = "1.2.3.4:1"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when admin disabled, got %d", w.Code)
	}
}

func TestPortPairAllocation(t *testing.T) {
	s := newTestServer(t)
	s.cfg.RelayPortMin = 30000
	s.cfg.RelayPortMax = 30004
	s.mu.Lock()
	a, b := s.allocatePortPair()
	c, d := s.allocatePortPair()
	e, f := s.allocatePortPair()
	s.mu.Unlock()
	if a == 0 || b == 0 || c == 0 || d == 0 {
		t.Fatalf("expected two pairs, got (%d,%d) (%d,%d)", a, b, c, d)
	}
	if e != 0 || f != 0 {
		t.Fatalf("expected exhausted range to return (0,0), got (%d,%d)", e, f)
	}
	if a == b || c == d || a == c || b == d {
		t.Fatalf("ports must be distinct: (%d,%d) (%d,%d)", a, b, c, d)
	}
}

func TestShutdownWithActiveRelay(t *testing.T) {
	s := newTestServer(t)
	mux := s.routes()
	_, gameAddr := startEchoServer(t)
	roomID, secret := createRoom(t, mux, "Shutdown")

	hostAddr, joinerAddr := relayAddrs(t, mux, roomID, secret)

	tunnel, _ := net.DialTimeout("tcp", hostAddr, 5*time.Second)
	defer func() { _ = tunnel.Close() }()
	writeHandshake(t, tunnel, 0x01, secret)
	time.Sleep(200 * time.Millisecond)

	joiner, _ := net.DialTimeout("tcp", joinerAddr, 5*time.Second)
	defer func() { _ = joiner.Close() }()
	_, _ = joiner.Write([]byte("ping"))
	time.Sleep(100 * time.Millisecond)

	dataConn, _ := net.DialTimeout("tcp", hostAddr, 5*time.Second)
	writeHandshake(t, dataConn, 0x02, secret)
	gameConn, _ := net.DialTimeout("tcp", gameAddr, 5*time.Second)
	go func() { _, _ = io.Copy(dataConn, gameConn); _ = dataConn.Close(); _ = gameConn.Close() }()
	go func() { _, _ = io.Copy(gameConn, dataConn); _ = dataConn.Close(); _ = gameConn.Close() }()

	buf := make([]byte, 4)
	_ = joiner.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(joiner, buf); err != nil {
		t.Fatalf("expected echo: %v", err)
	}

	done := make(chan struct{})
	go func() { s.closeAllRelays(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("closeAllRelays hung with an active session")
	}
}
