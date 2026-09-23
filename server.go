package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/time/rate"
)

const maxBody int64 = 64 * 1024 // 64KB

type Server struct {
	cfg       Config
	mu        sync.RWMutex
	rooms     map[string]*Room
	relays    map[string]*Relay
	usedPorts map[int]bool
	relaysWg  sync.WaitGroup

	ipMu       sync.Mutex
	ipLimiters map[string]*ipLimiterEntry

	relayConnMu       sync.Mutex
	relayConnLimiters map[string]*ipLimiterEntry

	bytesProxied int64
	bcryptSem    chan struct{}

	joinMu   sync.Mutex
	joinAuth map[string]map[string]time.Time

	bans *banStore

	xffWarnOnce sync.Once
}

type ipLimiterEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func NewServer(cfg Config) *Server {
	n := runtime.NumCPU()
	if n < 1 {
		n = 1
	}
	return &Server{
		cfg:               cfg,
		rooms:             make(map[string]*Room),
		relays:            make(map[string]*Relay),
		usedPorts:         make(map[int]bool),
		ipLimiters:        make(map[string]*ipLimiterEntry),
		relayConnLimiters: make(map[string]*ipLimiterEntry),
		bcryptSem:         make(chan struct{}, n),
		joinAuth:          make(map[string]map[string]time.Time),
		bans:              newBanStore(),
	}
}

// acquireBcrypt bounds concurrent bcrypt work so hashing/verification floods
// cannot exhaust the CPU.
func (s *Server) acquireBcrypt() bool {
	select {
	case s.bcryptSem <- struct{}{}:
		return true
	case <-time.After(3 * time.Second):
		return false
	}
}

func (s *Server) releaseBcrypt() { <-s.bcryptSem }

// recordJoinAuth authorizes ip to join roomID for a short window after a
// successful password check.
func (s *Server) recordJoinAuth(roomID, ip string) {
	if !s.cfg.RequireJoinAuth || ip == "" {
		return
	}
	s.joinMu.Lock()
	m := s.joinAuth[roomID]
	if m == nil {
		m = make(map[string]time.Time)
		s.joinAuth[roomID] = m
	}
	m[ip] = time.Now().Add(s.cfg.JoinAuthTTL)
	s.joinMu.Unlock()
}

func (s *Server) isJoinAuthorized(roomID, ip string) bool {
	if !s.cfg.RequireJoinAuth {
		return true
	}
	s.joinMu.Lock()
	defer s.joinMu.Unlock()
	m := s.joinAuth[roomID]
	if m == nil {
		return false
	}
	exp, ok := m[ip]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(m, ip)
		return false
	}
	return true
}

// allowRelayConn applies a per-IP rate limit to relay (joiner) connections.
func (s *Server) allowRelayConn(ip string) bool {
	if s.cfg.RelayConnRPM <= 0 {
		return true
	}
	s.relayConnMu.Lock()
	entry, ok := s.relayConnLimiters[ip]
	if !ok {
		rps := float64(s.cfg.RelayConnRPM) / 60.0
		entry = &ipLimiterEntry{
			limiter:  rate.NewLimiter(rate.Limit(rps), s.cfg.RateLimitBurst),
			lastSeen: time.Now(),
		}
		s.relayConnLimiters[ip] = entry
	} else {
		entry.lastSeen = time.Now()
	}
	s.relayConnMu.Unlock()
	return entry.limiter.Allow()
}

func (s *Server) Start() *http.Server {
	go s.cleanupLoop()
	go s.periodicCleanup()

	srv := &http.Server{
		Addr:              ":" + s.cfg.Port,
		Handler:           s.routes(),
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 * 1024,
	}

	go func() {
		portNum := 8080
		if n, err := strconv.Atoi(s.cfg.Port); err == nil {
			portNum = n
		}
		log.Printf("open-lobby-relay listening on port %d (relay ports %d-%d)...", portNum, s.cfg.RelayPortMin, s.cfg.RelayPortMax)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Listen error: %v", err)
		}
	}()

	return srv
}

func (s *Server) Shutdown(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("HTTP shutdown error: %v", err)
	}
	s.closeAllRelays()
	log.Println("Server stopped")
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.Handle("/metrics", s.rateLimitMiddleware(http.HandlerFunc(s.handleMetrics)))
	mux.Handle("/rooms", s.rateLimitMiddleware(http.HandlerFunc(s.handleRooms)))
	mux.Handle("/rooms/", s.rateLimitMiddleware(http.HandlerFunc(s.handleRoomsByID)))
	mux.Handle("/heartbeat", s.rateLimitMiddleware(http.HandlerFunc(s.handleHeartbeat)))
	mux.Handle("/relay", s.rateLimitMiddleware(http.HandlerFunc(s.handleRelay)))
	mux.Handle("/admin/", s.adminMiddleware(http.HandlerFunc(s.handleAdmin)))
	return mux
}

// --- Background loops ---

func (s *Server) cleanupLoop() {
	for {
		time.Sleep(10 * time.Second)
		s.mu.Lock()
		now := time.Now()
		for id, room := range s.rooms {
			if now.Sub(room.LastSeen) > 25*time.Second {
				log.Printf("Cleaning up inactive room: %q (%s)", room.Name, id)
				delete(s.rooms, id)
				s.closeRelayLocked(id)
			}
		}
		s.mu.Unlock()
	}
}

func (s *Server) periodicCleanup() {
	for {
		time.Sleep(5 * time.Minute)
		cutoff := time.Now().Add(-10 * time.Minute)

		s.ipMu.Lock()
		for ip, entry := range s.ipLimiters {
			if entry.lastSeen.Before(cutoff) {
				delete(s.ipLimiters, ip)
			}
		}
		s.ipMu.Unlock()

		s.relayConnMu.Lock()
		for ip, entry := range s.relayConnLimiters {
			if entry.lastSeen.Before(cutoff) {
				delete(s.relayConnLimiters, ip)
			}
		}
		s.relayConnMu.Unlock()

		s.joinMu.Lock()
		now := time.Now()
		for roomID, ips := range s.joinAuth {
			for ip, exp := range ips {
				if now.After(exp) {
					delete(ips, ip)
				}
			}
			if len(ips) == 0 {
				delete(s.joinAuth, roomID)
			}
		}
		s.joinMu.Unlock()
	}
}

// --- Helpers ---

func (s *Server) extractIP(r *http.Request) string {
	xff := r.Header.Get("X-Forwarded-For")
	if s.cfg.TrustProxy {
		if xff != "" {
			// Rightmost entry: the value a single trusted proxy appended.
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	} else if xff != "" {
		// A proxy is forwarding the client address but we were told not to
		// trust it, so every request looks like the proxy: per-IP rate limits
		// and join authorization will be keyed on the wrong address.
		s.xffWarnOnce.Do(func() {
			log.Printf("Warning: received X-Forwarded-For but TRUST_PROXY is false; " +
				"if the API is behind a reverse proxy, set TRUST_PROXY=true, or join " +
				"authorization and rate limits will use the proxy's IP")
		})
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return strings.TrimSpace(ip)
}

func isPrivateOrReserved(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

func (s *Server) resolveRelayHost(r *http.Request) string {
	if s.cfg.PublicHost != "" {
		return s.cfg.PublicHost
	}
	if h := r.Host; h != "" {
		host, _, err := net.SplitHostPort(h)
		if err == nil && host != "" {
			return host
		}
		return h
	}
	return s.extractIP(r)
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("json encode error: %v", err)
	}
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(map[string]string{"error": msg}); err != nil {
		log.Printf("json encode error: %v", err)
	}
}

func writeJSONStatus(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("json encode error: %v", err)
	}
}

// --- Middleware ---

func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
	if s.cfg.RateLimitRPM <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := s.extractIP(r)
		s.ipMu.Lock()
		entry, ok := s.ipLimiters[ip]
		if !ok {
			rps := float64(s.cfg.RateLimitRPM) / 60.0
			entry = &ipLimiterEntry{
				limiter:  rate.NewLimiter(rate.Limit(rps), s.cfg.RateLimitBurst),
				lastSeen: time.Now(),
			}
			s.ipLimiters[ip] = entry
		} else {
			entry.lastSeen = time.Now()
		}
		s.ipMu.Unlock()

		if entry.limiter.Allow() {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Retry-After", "60")
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
	})
}

func (s *Server) adminMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AdminToken == "" {
			http.NotFound(w, r)
			return
		}
		const prefix = "Bearer "
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, prefix) ||
			subtle.ConstantTimeCompare([]byte(auth[len(prefix):]), []byte(s.cfg.AdminToken)) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- Handlers ---

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	roomCount := len(s.rooms)
	relayCount := len(s.relays)
	activeConns := int32(0)
	for _, relay := range s.relays {
		activeConns += atomic.LoadInt32(&relay.connCount)
	}
	s.mu.RUnlock()

	writeJSON(w, map[string]interface{}{
		"rooms":              roomCount,
		"relays":             relayCount,
		"active_connections": activeConns,
		"bytes_proxied":      atomic.LoadInt64(&s.bytesProxied),
	})
}

func (s *Server) handleRooms(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		list := make([]*Room, 0, len(s.rooms))
		for _, room := range s.rooms {
			roomCopy := *room
			roomCopy.PasswordHash = ""
			list = append(list, &roomCopy)
		}
		s.mu.RUnlock()
		writeJSON(w, list)

	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)

		var input struct {
			Name        string `json:"name"`
			Mode        string `json:"mode"`
			Version     string `json:"version"`
			HasPassword bool   `json:"has_password"`
			Password    string `json:"password"`
			MaxPlayers  int    `json:"max_players"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}

		if len(input.Name) == 0 || len(input.Name) > 100 {
			writeJSONError(w, http.StatusBadRequest, "name must be 1-100 characters")
			return
		}
		if len(input.Mode) > 50 {
			writeJSONError(w, http.StatusBadRequest, "mode must be 50 characters or fewer")
			return
		}
		if len(input.Version) > 50 {
			writeJSONError(w, http.StatusBadRequest, "version must be 50 characters or fewer")
			return
		}
		if len(input.Password) > 72 {
			writeJSONError(w, http.StatusBadRequest, "password must be 72 characters or fewer")
			return
		}
		if input.MaxPlayers < 1 || input.MaxPlayers > 64 {
			writeJSONError(w, http.StatusBadRequest, "max_players must be 1-64")
			return
		}

		hostIP := s.extractIP(r)
		if s.bans.isBanned(hostIP) {
			writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		if !s.cfg.AllowPrivateHostIP && isPrivateOrReserved(hostIP) {
			writeJSONError(w, http.StatusBadRequest, "host IP not allowed")
			return
		}

		s.mu.RLock()
		full := len(s.rooms) >= s.cfg.MaxRooms
		s.mu.RUnlock()
		if full {
			writeJSONError(w, http.StatusServiceUnavailable, "max rooms reached")
			return
		}

		idBytes := make([]byte, 16)
		if _, err := rand.Read(idBytes); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to generate id")
			return
		}
		secretBytes := make([]byte, 32)
		if _, err := rand.Read(secretBytes); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to generate secret")
			return
		}

		var room Room
		room.ID = hex.EncodeToString(idBytes)
		room.Secret = hex.EncodeToString(secretBytes)
		room.Name = input.Name
		room.Mode = input.Mode
		room.Version = input.Version
		room.HasPassword = input.HasPassword
		room.MaxPlayers = input.MaxPlayers
		room.LastSeen = time.Now()

		if input.Password != "" {
			if !s.acquireBcrypt() {
				writeJSONError(w, http.StatusServiceUnavailable, "server busy")
				return
			}
			hash, err := bcrypt.GenerateFromPassword([]byte(input.Password), s.cfg.BcryptCost)
			s.releaseBcrypt()
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, "failed to hash password")
				return
			}
			room.PasswordHash = string(hash)
			room.HasPassword = true
		} else if input.HasPassword {
			room.HasPassword = false
		}

		s.mu.Lock()
		if len(s.rooms) >= s.cfg.MaxRooms {
			s.mu.Unlock()
			writeJSONError(w, http.StatusServiceUnavailable, "max rooms reached")
			return
		}
		s.rooms[room.ID] = &room
		s.mu.Unlock()

		writeJSONStatus(w, http.StatusCreated, map[string]interface{}{
			"id":           room.ID,
			"name":         room.Name,
			"mode":         room.Mode,
			"version":      room.Version,
			"has_password": room.HasPassword,
			"max_players":  room.MaxPlayers,
			"secret":       room.Secret,
		})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleRoomsByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/rooms/")
	parts := strings.SplitN(path, "/", 2)
	roomID := parts[0]

	if len(parts) > 1 && parts[1] == "verify" {
		s.handleVerifyPassword(w, r, roomID)
		return
	}
	if r.Method == http.MethodDelete {
		s.handleDeleteRoom(w, r, roomID)
		return
	}
	w.WriteHeader(http.StatusMethodNotAllowed)
}

func (s *Server) handleVerifyPassword(w http.ResponseWriter, r *http.Request, roomID string) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var payload struct {
		Password string `json:"password"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ip := s.extractIP(r)

	s.mu.RLock()
	room, exists := s.rooms[roomID]
	s.mu.RUnlock()
	if !exists {
		writeJSONError(w, http.StatusNotFound, "room not found")
		return
	}

	if !room.HasPassword {
		s.recordJoinAuth(roomID, ip)
		writeJSON(w, map[string]bool{"valid": true})
		return
	}

	if !s.acquireBcrypt() {
		writeJSONError(w, http.StatusServiceUnavailable, "server busy")
		return
	}
	err := bcrypt.CompareHashAndPassword([]byte(room.PasswordHash), []byte(payload.Password))
	s.releaseBcrypt()

	if err == nil {
		s.recordJoinAuth(roomID, ip)
		writeJSON(w, map[string]bool{"valid": true})
	} else {
		writeJSONStatus(w, http.StatusForbidden, map[string]bool{"valid": false})
	}
}

func (s *Server) handleDeleteRoom(w http.ResponseWriter, r *http.Request, roomID string) {
	token := r.Header.Get("X-Room-Token")
	if token == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	s.mu.Lock()
	room, exists := s.rooms[roomID]
	if !exists {
		s.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if !validToken(token, room.Secret) {
		s.mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
		return
	}

	delete(s.rooms, roomID)
	s.closeRelayLocked(roomID)
	s.mu.Unlock()

	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var payload struct {
		ID          string `json:"id"`
		PlayerCount int    `json:"player_count"`
		Mode        string `json:"mode"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if payload.PlayerCount < 0 || payload.PlayerCount > 64 {
		writeJSONError(w, http.StatusBadRequest, "player_count must be 0-64")
		return
	}
	if len(payload.Mode) > 50 {
		writeJSONError(w, http.StatusBadRequest, "mode must be 50 characters or fewer")
		return
	}

	token := r.Header.Get("X-Room-Token")
	if token == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	s.mu.Lock()
	room, exists := s.rooms[payload.ID]
	if !exists {
		s.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if !validToken(token, room.Secret) {
		s.mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
		return
	}
	room.LastSeen = time.Now()
	room.PlayerCount = payload.PlayerCount
	if payload.Mode != "" {
		room.Mode = payload.Mode
	}
	w.WriteHeader(http.StatusOK)
	s.mu.Unlock()
}

// allocatePortPair returns two free relay ports (host, joiner), or (0, 0).
// Caller must hold s.mu.
func (s *Server) allocatePortPair() (int, int) {
	first := 0
	for port := s.cfg.RelayPortMin; port <= s.cfg.RelayPortMax; port++ {
		if s.usedPorts[port] {
			continue
		}
		if first == 0 {
			first = port
			continue
		}
		s.usedPorts[first] = true
		s.usedPorts[port] = true
		return first, port
	}
	return 0, 0
}

func (s *Server) handleRelay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	var input struct {
		RoomID string `json:"room_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if input.RoomID == "" {
		writeJSONError(w, http.StatusBadRequest, "room_id required")
		return
	}

	token := r.Header.Get("X-Room-Token")
	if token == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	s.mu.Lock()

	room, exists := s.rooms[input.RoomID]
	if !exists {
		s.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if !validToken(token, room.Secret) {
		s.mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
		return
	}

	s.closeRelayLocked(input.RoomID)

	if len(s.relays) >= s.cfg.MaxRelays {
		s.mu.Unlock()
		writeJSONError(w, http.StatusServiceUnavailable, "max relays reached")
		return
	}

	hostPort, joinerPort := s.allocatePortPair()
	if hostPort == 0 {
		s.mu.Unlock()
		writeJSONError(w, http.StatusServiceUnavailable, "no ports available")
		return
	}

	hostLn, err := net.Listen("tcp", fmt.Sprintf(":%d", hostPort))
	if err != nil {
		delete(s.usedPorts, hostPort)
		delete(s.usedPorts, joinerPort)
		s.mu.Unlock()
		writeJSONError(w, http.StatusInternalServerError, "failed to bind relay ports")
		return
	}
	joinerLn, err := net.Listen("tcp", fmt.Sprintf(":%d", joinerPort))
	if err != nil {
		_ = hostLn.Close()
		delete(s.usedPorts, hostPort)
		delete(s.usedPorts, joinerPort)
		s.mu.Unlock()
		writeJSONError(w, http.StatusInternalServerError, "failed to bind relay ports")
		return
	}

	relayHost := s.resolveRelayHost(r)
	relay := s.startRelay(room, relayHost, hostLn, joinerLn)
	s.relays[input.RoomID] = relay

	s.mu.Unlock()

	writeJSONStatus(w, http.StatusCreated, map[string]interface{}{
		"relay_host": relayHost,
		"host_port":  hostPort,
		"relay_port": joinerPort,
	})
}

func validToken(token, secret string) bool {
	return len(token) == len(secret) && subtle.ConstantTimeCompare([]byte(token), []byte(secret)) == 1
}
