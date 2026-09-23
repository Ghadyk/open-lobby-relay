package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Host connections frame their role in the first byte, followed by the room
// secret. Only the host connects to the host port, so any byte after
// authentication is forwarded verbatim — the relay makes no assumption about
// the game's protocol.
const (
	roleHostTunnel byte = 0x01 // persistent control connection
	roleHostData   byte = 0x02 // per-joiner data connection
	hostSecretLen       = 64   // hex-encoded 32-byte room secret
)

// hostAuthTimeout bounds how long an unauthenticated host connection may sit on
// the host port before it must prove possession of the room secret.
const hostAuthTimeout = 3 * time.Second

var errByteQuota = errors.New("relay byte quota exceeded")

// Relay is a reverse-tunnel TCP relay for one room:
//   - the host port accepts the host's tunnel and data connections;
//   - the joiner port accepts joiners, which need no handshake at all.
type Relay struct {
	RoomID         string
	Secret         string
	RelayHost      string
	HostListener   net.Listener
	JoinerListener net.Listener
	HostPort       int
	JoinerPort     int
	dataConnCh     chan net.Conn
	hostHandshakes chan struct{}
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	connCount      int32
	bytesProxied   int64

	mu         sync.Mutex
	hostTunnel net.Conn
}

func (s *Server) startRelay(room *Room, relayHost string, hostLn, joinerLn net.Listener) *Relay {
	ctx, cancel := context.WithCancel(context.Background())
	relay := &Relay{
		RoomID:         room.ID,
		Secret:         room.Secret,
		RelayHost:      relayHost,
		HostListener:   hostLn,
		JoinerListener: joinerLn,
		HostPort:       hostLn.Addr().(*net.TCPAddr).Port,
		JoinerPort:     joinerLn.Addr().(*net.TCPAddr).Port,
		dataConnCh:     make(chan net.Conn, s.cfg.MaxConnsPerRelay),
		hostHandshakes: make(chan struct{}, s.cfg.MaxConnsPerRelay+1),
		cancel:         cancel,
	}

	s.relaysWg.Add(1)
	go func() {
		defer s.relaysWg.Done()
		s.runRelay(ctx, relay)
	}()

	return relay
}

func (s *Server) runRelay(ctx context.Context, relay *Relay) {
	log.Printf("Relay %s: host port %d, joiner port %d", relay.RoomID, relay.HostPort, relay.JoinerPort)

	var loops sync.WaitGroup
	loops.Add(2)
	go func() { defer loops.Done(); s.acceptHostLoop(ctx, relay) }()
	go func() { defer loops.Done(); s.acceptJoinerLoop(ctx, relay) }()
	loops.Wait()

	relay.cancel()
	relay.wg.Wait()

	relay.mu.Lock()
	if relay.hostTunnel != nil {
		_ = relay.hostTunnel.Close()
		relay.hostTunnel = nil
	}
	relay.mu.Unlock()

	s.cleanupRelay(relay)
}

// --- Host side (framed) ---

func (s *Server) acceptHostLoop(ctx context.Context, relay *Relay) {
	for {
		conn, err := relay.HostListener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("Relay %s: host accept error: %v", relay.RoomID, err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		// Bound unauthenticated host connections so a flood of silent
		// connections to the host port cannot spawn unbounded goroutines. The
		// slot is released once the connection authenticates or is dropped.
		select {
		case relay.hostHandshakes <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}
		relay.wg.Add(1)
		go func(c net.Conn) {
			defer relay.wg.Done()
			s.handleHostConn(ctx, relay, c)
		}(conn)
	}
}

func (s *Server) handleHostConn(ctx context.Context, relay *Relay, conn net.Conn) {
	role, ok := readHostRole(conn, relay.Secret, hostAuthTimeout)
	<-relay.hostHandshakes // authenticated or dropped; free the handshake slot
	if !ok {
		log.Printf("Relay %s: rejecting unauthenticated host connection from %s", relay.RoomID, remoteIP(conn))
		_ = conn.Close()
		return
	}

	switch role {
	case roleHostTunnel:
		relay.mu.Lock()
		if relay.hostTunnel != nil {
			_ = relay.hostTunnel.Close() // host reconnected; replace the dead tunnel
		}
		relay.hostTunnel = conn
		relay.mu.Unlock()
		log.Printf("Relay %s: host tunnel established from %s", relay.RoomID, remoteIP(conn))
		s.markRelayReady(relay)
		// conn stays open: it is the host's control connection. Watch it so a
		// dropped tunnel unadvertises the relay until the host reconnects.
		go s.watchHostTunnel(relay, conn)
	case roleHostData:
		select {
		case relay.dataConnCh <- conn:
		default:
			log.Printf("Relay %s: data connection with no waiting joiner, closing", relay.RoomID)
			_ = conn.Close()
		}
	}
}

// readHostRole reads the role byte and verifies the room secret.
func readHostRole(conn net.Conn, secret string, timeout time.Duration) (byte, bool) {
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()

	header := make([]byte, 1)
	if _, err := io.ReadFull(conn, header); err != nil {
		return 0, false
	}
	if header[0] != roleHostTunnel && header[0] != roleHostData {
		return 0, false
	}
	got := make([]byte, hostSecretLen)
	if _, err := io.ReadFull(conn, got); err != nil {
		return 0, false
	}
	if len(secret) != hostSecretLen || subtle.ConstantTimeCompare(got, []byte(secret)) != 1 {
		return 0, false
	}
	return header[0], true
}

// --- Joiner side (no handshake) ---

func (s *Server) acceptJoinerLoop(ctx context.Context, relay *Relay) {
	for {
		conn, err := relay.JoinerListener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("Relay %s: joiner accept error: %v", relay.RoomID, err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		relay.wg.Add(1)
		go func(c net.Conn) {
			defer relay.wg.Done()
			s.handleJoiner(ctx, relay, c)
		}(conn)
	}
}

func (s *Server) handleJoiner(ctx context.Context, relay *Relay, conn net.Conn) {
	ip := remoteIP(conn)
	if s.bans.isBanned(ip) {
		_ = conn.Close()
		return
	}
	if !s.allowRelayConn(ip) {
		_ = conn.Close()
		return
	}
	if !s.isJoinAuthorized(relay.RoomID, ip) {
		log.Printf("Relay %s: rejecting unauthorized joiner %s", relay.RoomID, ip)
		_ = conn.Close()
		return
	}
	if int(atomic.LoadInt32(&relay.connCount)) >= s.cfg.MaxConnsPerRelay {
		log.Printf("Relay %s: max connections (%d) reached, rejecting joiner", relay.RoomID, s.cfg.MaxConnsPerRelay)
		_ = conn.Close()
		return
	}

	relay.mu.Lock()
	hostTunnel := relay.hostTunnel
	relay.mu.Unlock()
	if hostTunnel == nil {
		log.Printf("Relay %s: no host tunnel, rejecting joiner", relay.RoomID)
		_ = conn.Close()
		return
	}
	if _, err := hostTunnel.Write([]byte{roleHostTunnel}); err != nil {
		log.Printf("Relay %s: failed to signal host: %v", relay.RoomID, err)
		_ = conn.Close()
		return
	}

	atomic.AddInt32(&relay.connCount, 1)
	defer atomic.AddInt32(&relay.connCount, -1)

	select {
	case dataConn := <-relay.dataConnCh:
		s.bridge(ctx, relay, conn, dataConn)
	case <-time.After(15 * time.Second):
		log.Printf("Relay %s: host did not open a data connection in time", relay.RoomID)
		_ = conn.Close()
	case <-ctx.Done():
		_ = conn.Close()
	}
}

// bridge copies bytes both ways until either side closes, the relay is
// cancelled, the connection goes idle for RelayIdleTimeout, or the byte quota
// is exhausted.
func (s *Server) bridge(ctx context.Context, relay *Relay, joinerConn, dataConn net.Conn) {
	log.Printf("Relay %s: bridging joiner %s", relay.RoomID, remoteIP(joinerConn))

	var remaining int64
	if s.cfg.RelayMaxBytes > 0 {
		remaining = s.cfg.RelayMaxBytes
	}

	done := make(chan struct{}, 2)
	finished := make(chan struct{})

	copyDir := func(dst, src net.Conn) {
		reader := &idleReader{Conn: src, timeout: s.cfg.RelayIdleTimeout}
		writer := &capWriter{w: dst, remaining: &remaining, unlimited: s.cfg.RelayMaxBytes <= 0}
		n, err := io.Copy(writer, reader)
		atomic.AddInt64(&relay.bytesProxied, n)
		atomic.AddInt64(&s.bytesProxied, n)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && !errors.Is(err, errByteQuota) {
			log.Printf("Relay %s: copy error: %v", relay.RoomID, err)
		}
		done <- struct{}{}
	}

	go copyDir(dataConn, joinerConn)
	go copyDir(joinerConn, dataConn)

	go func() {
		select {
		case <-ctx.Done():
			_ = joinerConn.Close()
			_ = dataConn.Close()
		case <-finished:
		}
	}()

	<-done
	_ = joinerConn.Close()
	_ = dataConn.Close()
	<-done
	close(finished)
	log.Printf("Relay %s: joiner session ended", relay.RoomID)
}

// idleReader resets the read deadline before every read, so a connection with
// no traffic for the timeout errors out.
type idleReader struct {
	net.Conn
	timeout time.Duration
}

func (r *idleReader) Read(p []byte) (int, error) {
	_ = r.SetReadDeadline(time.Now().Add(r.timeout))
	return r.Conn.Read(p)
}

// capWriter errors once the shared byte budget is exhausted.
type capWriter struct {
	w         io.Writer
	remaining *int64
	unlimited bool
}

func (c *capWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if !c.unlimited && n > 0 {
		if atomic.AddInt64(c.remaining, -int64(n)) < 0 {
			return n, errByteQuota
		}
	}
	return n, err
}

// --- Lifecycle ---

// markRelayReady advertises the relay on the room once the host tunnel is up,
// so joiners never see an unusable relay.
func (s *Server) markRelayReady(relay *Relay) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.relays[relay.RoomID]; !ok || r != relay {
		return
	}
	if room, ok := s.rooms[relay.RoomID]; ok {
		room.RelayHost = relay.RelayHost
		room.RelayPort = relay.JoinerPort
		room.UseRelay = true
	}
}

// watchHostTunnel blocks reading the host's control connection. The host never
// writes on the tunnel, so a read returning an error means the tunnel is gone.
// The relay then stops advertising itself until the host reconnects, so joiners
// never see a relay that cannot serve them.
func (s *Server) watchHostTunnel(relay *Relay, conn net.Conn) {
	buf := make([]byte, 1)
	for {
		if _, err := conn.Read(buf); err != nil {
			break
		}
	}

	relay.mu.Lock()
	stillCurrent := relay.hostTunnel == conn
	if stillCurrent {
		relay.hostTunnel = nil
	}
	relay.mu.Unlock()
	_ = conn.Close()

	if stillCurrent {
		log.Printf("Relay %s: host tunnel lost; unadvertising until the host reconnects", relay.RoomID)
		s.clearAdvertisement(relay.RoomID, relay)
	}
}

// clearRoomRelayLocked resets a room's relay advertisement. Caller must hold s.mu.
func (s *Server) clearRoomRelayLocked(roomID string) {
	if room, ok := s.rooms[roomID]; ok {
		room.UseRelay = false
		room.RelayHost = ""
		room.RelayPort = 0
	}
}

// clearAdvertisement unadvertises a room's relay if relay is still the room's
// current relay. Safe to call without holding s.mu.
func (s *Server) clearAdvertisement(roomID string, relay *Relay) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.relays[roomID]; !ok || r != relay {
		return
	}
	s.clearRoomRelayLocked(roomID)
}

func (s *Server) cleanupRelay(relay *Relay) {
	s.mu.Lock()
	if r, exists := s.relays[relay.RoomID]; exists && r == relay {
		delete(s.relays, relay.RoomID)
		delete(s.usedPorts, relay.HostPort)
		delete(s.usedPorts, relay.JoinerPort)
	}
	s.mu.Unlock()

	_ = relay.HostListener.Close()
	_ = relay.JoinerListener.Close()
}

// closeRelayLocked removes the relay, frees its ports, stops advertising it,
// and cancels active connections. Caller must hold s.mu.
func (s *Server) closeRelayLocked(roomID string) {
	relay, exists := s.relays[roomID]
	if !exists {
		return
	}
	delete(s.relays, roomID)
	delete(s.usedPorts, relay.HostPort)
	delete(s.usedPorts, relay.JoinerPort)
	s.clearRoomRelayLocked(roomID)
	_ = relay.HostListener.Close()
	_ = relay.JoinerListener.Close()
	relay.cancel()
}

// closeAllRelays closes every relay and waits for all relay goroutines.
func (s *Server) closeAllRelays() {
	s.mu.Lock()
	for _, relay := range s.relays {
		delete(s.relays, relay.RoomID)
		delete(s.usedPorts, relay.HostPort)
		delete(s.usedPorts, relay.JoinerPort)
		s.clearRoomRelayLocked(relay.RoomID)
		_ = relay.HostListener.Close()
		_ = relay.JoinerListener.Close()
		relay.cancel()
	}
	s.mu.Unlock()

	s.relaysWg.Wait()
}

func remoteIP(conn net.Conn) string {
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return conn.RemoteAddr().String()
	}
	return host
}
