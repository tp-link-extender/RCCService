package main

import (
	"fmt"
	"net"
	"sync"
	"time"

	c "github.com/TwiN/go-color"
	. "github.com/tp-link-extender/RCCService/Shared"
)

const (
	proto = "udp4"
	idleTimeout = 35 * time.Second

	// a game's relay listens at its gameserver's port plus this offset
	proxyOffset = 25000

	// relayBufferSize is the largest UDP datagram
	relayBufferSize = 65535
)

// udpRelay forwards one game's UDP traffic from its public port to the gameserver on 127.0.0.1
type udpRelay struct {
	port int
	conn *net.UDPConn
	stop sync.Once
}

// idToPort is the port a gameserver listens on for its place
func idToPort(id int) int {
	return 10000 + id%proxyOffset
}

// startRelay binds the relay for a place's gameserver
func startRelay(id int) (*udpRelay, error) {
	r := &udpRelay{port: idToPort(id) + proxyOffset}

	conn, err := net.ListenUDP(proto, &net.UDPAddr{IP: net.IPv4zero, Port: r.port})
	if err != nil {
		return nil, fmt.Errorf("bind relay port %d: %w", r.port, err)
	}

	r.conn = conn

	go r.run()

	Log(c.InGreen(fmt.Sprintf("[relay:%d] listener started", r.port)))

	return r, nil
}

// close shuts the relay down. Closing its socket ends the loop and every session under it
func (r *udpRelay) close() {
	r.stop.Do(func() {
		if r.conn != nil {
			r.conn.Close()
		}

		Log(c.InYellow(fmt.Sprintf("[relay:%d] stopped", r.port)))
	})
}

// udpSession is one client's path to the gameserver
type udpSession struct {
	client *net.UDPAddr
	gs *net.UDPConn
	last time.Time // guarded by mu, written by both directions of the relay
	mu sync.Mutex
}

func (s *udpSession) touch() {
	s.mu.Lock()

	s.last = time.Now()

	s.mu.Unlock()
}

func (s *udpSession) lastSeen() time.Time {
	s.mu.Lock()

	defer s.mu.Unlock()

	return s.last
}

// run relays packets until the socket closes
func (r *udpRelay) run() {
	gsPort := r.port - proxyOffset
	sessions := make(map[string]*udpSession)

	var mu sync.Mutex

	deleteSession := func(key string) {
		mu.Lock()

		delete(sessions, key)

		mu.Unlock()
	}

	// idle sessions are culled until the socket closes
	done := make(chan struct{})
	ticker := time.NewTicker(10 * time.Second)

	defer close(done)
	defer ticker.Stop()

	go func() {
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
			}

			cutoff := time.Now().Add(-idleTimeout)

			mu.Lock()

			for k, s := range sessions {
				if s.lastSeen().After(cutoff) {
					continue
				}

				Log(c.InYellow(fmt.Sprintf("[relay:%d] timing out session %s", r.port, k)))

				s.gs.Close()
				delete(sessions, k)
			}

			mu.Unlock()
		}
	}()

	buf := make([]byte, relayBufferSize)

	for {
		n, clientAddr, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			Log(c.InRed(fmt.Sprintf("[relay:%d] read error: %v", r.port, err)))

			break
		}

		// only IPv4 clients are allowed (:c)
		if clientAddr.IP.To4() == nil {
			continue
		}

		key := clientAddr.String()

		mu.Lock()

		s, exists := sessions[key]

		mu.Unlock()

		if !exists {
			gsAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: gsPort}

			gsConn, err := net.DialUDP(proto, nil, gsAddr)
			if err != nil {
				Log(c.InRed(fmt.Sprintf("[relay:%d] failed to dial gameserver %s: %v", r.port, gsAddr.String(), err)))

				continue
			}

			s = &udpSession{
				client: &net.UDPAddr{IP: clientAddr.IP, Port: clientAddr.Port},
				gs: gsConn,
				last: time.Now(),
			}

			mu.Lock()

			sessions[key] = s

			mu.Unlock()

			Log(c.InBlue(fmt.Sprintf("[relay:%d] new client session %s -> 127.0.0.1:%d", r.port, key, gsPort)))

			go func() {
				r.relayToClient(key, s)
				s.gs.Close()
				deleteSession(key)
			}()
		}

		// forward client's packet to gameserver
		if _, err := s.gs.Write(buf[:n]); err != nil {
			Log(c.InRed(fmt.Sprintf("[relay:%d] failed to send to gameserver for client %s: %v", r.port, key, err)))
			s.gs.Close()
			deleteSession(key)

			continue
		}

		s.touch()
	}

	// the socket closed: end every session
	mu.Lock()

	for k, s := range sessions {
		Log(c.InYellow(fmt.Sprintf("[relay:%d] closing session %s", r.port, k)))

		s.gs.Close()
		delete(sessions, k)
	}

	mu.Unlock()
}

// relayToClient forwards a gameserver's replies to the client until the gameserver side closes
func (r *udpRelay) relayToClient(key string, s *udpSession) {
	b := make([]byte, relayBufferSize)

	for {
		n, err := s.gs.Read(b)
		if err != nil {
			Log(c.InYellow(fmt.Sprintf("[relay:%d] gameserver connection closed for client %s: %v", r.port, key, err)))

			return
		}

		// the relay's own socket is the one that talks to the client, so it's still open here
		// unless the whole relay has closed
		if _, err := r.conn.WriteToUDP(b[:n], s.client); err != nil {
			Log(c.InRed(fmt.Sprintf("[relay:%d] failed to forward to client %s: %v", r.port, s.client.String(), err)))

			return
		}

		s.touch()
	}
}
