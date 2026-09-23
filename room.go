package main

import "time"

// Room is a game advertised in the lobby. Secret and PasswordHash are never
// serialized.
type Room struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Mode         string    `json:"mode"`
	Version      string    `json:"version"`
	HasPassword  bool      `json:"has_password"`
	PasswordHash string    `json:"-"`
	Secret       string    `json:"-"`
	PlayerCount  int       `json:"player_count"`
	MaxPlayers   int       `json:"max_players"`
	RelayHost    string    `json:"relay_host"`
	RelayPort    int       `json:"relay_port"`
	UseRelay     bool      `json:"use_relay"`
	LastSeen     time.Time `json:"-"`
}
