package main

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// handleAdmin serves the operator endpoints. Requests are authenticated by
// adminMiddleware; the routes are unavailable when ADMIN_TOKEN is unset.
func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/admin")
	switch {
	case path == "/rooms" || path == "/rooms/":
		s.adminListRooms(w, r)
	case strings.HasPrefix(path, "/rooms/"):
		s.adminDeleteRoom(w, r, strings.TrimPrefix(path, "/rooms/"))
	case path == "/relays/close-all":
		s.adminCloseAllRelays(w, r)
	case path == "/relays" || path == "/relays/":
		s.adminListRelays(w, r)
	case path == "/bans" || path == "/bans/":
		s.adminBans(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) adminListRooms(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.mu.RLock()
	list := make([]*Room, 0, len(s.rooms))
	for _, room := range s.rooms {
		c := *room
		c.PasswordHash = ""
		c.Secret = ""
		list = append(list, &c)
	}
	s.mu.RUnlock()
	writeJSON(w, list)
}

func (s *Server) adminDeleteRoom(w http.ResponseWriter, r *http.Request, roomID string) {
	if r.Method != http.MethodDelete {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	_, exists := s.rooms[roomID]
	if exists {
		delete(s.rooms, roomID)
		s.closeRelayLocked(roomID)
	}
	s.mu.Unlock()
	if !exists {
		http.NotFound(w, r)
		return
	}
	s.clearJoinAuth(roomID)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) adminListRelays(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	type relayInfo struct {
		RoomID       string `json:"room_id"`
		HostPort     int    `json:"host_port"`
		JoinerPort   int    `json:"joiner_port"`
		Connections  int32  `json:"connections"`
		BytesProxied int64  `json:"bytes_proxied"`
	}
	s.mu.RLock()
	list := make([]relayInfo, 0, len(s.relays))
	for _, relay := range s.relays {
		list = append(list, relayInfo{
			RoomID:       relay.RoomID,
			HostPort:     relay.HostPort,
			JoinerPort:   relay.JoinerPort,
			Connections:  atomic.LoadInt32(&relay.connCount),
			BytesProxied: atomic.LoadInt64(&relay.bytesProxied),
		})
	}
	s.mu.RUnlock()
	writeJSON(w, list)
}

func (s *Server) adminCloseAllRelays(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.closeAllRelays()
	w.WriteHeader(http.StatusOK)
}

func (s *Server) adminBans(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.bans.active())

	case http.MethodPost:
		var in struct {
			IP      string `json:"ip"`
			Seconds int    `json:"seconds"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&in); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if net.ParseIP(in.IP) == nil {
			writeJSONError(w, http.StatusBadRequest, "invalid ip")
			return
		}
		var until time.Time
		if in.Seconds > 0 {
			until = time.Now().Add(time.Duration(in.Seconds) * time.Second)
		}
		s.bans.ban(in.IP, until)
		w.WriteHeader(http.StatusOK)

	case http.MethodDelete:
		var in struct {
			IP string `json:"ip"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&in); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if !s.bans.unban(in.IP) {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
