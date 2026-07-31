// Package bidi is a minimal WebDriver BiDi client over WebSocket.
//
// It exists for one thing classic WebDriver cannot express: answering a proxy
// authentication challenge on Firefox. The HTTP auth dialog is a real window
// prompt (nsIPrompt_promptUsernameAndPassword), and Get Alert Text answers "no
// such alert" for it, so the navigation simply times out. BiDi exposes the
// challenge as an event and lets us supply credentials — the same job
// server/browser/cdpauth does over CDP for the two Chromium backends.
//
// Only the handful of commands that job needs are implemented; this is not a
// general BiDi client.
package bidi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"

	browserpkg "github.com/trinity-aml/flaresolverr-go/server/browser"
)

// Session is a live BiDi connection. It is safe for concurrent use.
type Session struct {
	conn net.Conn

	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan reply
	events  map[string]func(json.RawMessage)

	closeOnce sync.Once
	closed    chan struct{}
}

type reply struct {
	result json.RawMessage
	err    error
}

// Dial connects to the WebSocket URL geckodriver reports as the webSocketUrl
// capability.
func Dial(ctx context.Context, wsURL string) (*Session, error) {
	conn, _, _, err := ws.Dial(ctx, wsURL)
	if err != nil {
		return nil, fmt.Errorf("connect to bidi at %s: %w", wsURL, err)
	}

	s := &Session{
		conn:    conn,
		pending: map[int64]chan reply{},
		events:  map[string]func(json.RawMessage){},
		closed:  make(chan struct{}),
	}
	go s.readLoop()
	return s, nil
}

// On registers a handler for a BiDi event. Handlers run on their own goroutine:
// answering an event usually means sending a command, and doing that from the
// read loop would deadlock waiting for a reply only that loop can deliver.
func (s *Session) On(event string, fn func(params json.RawMessage)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events[event] = fn
}

// Send issues a command and waits for its reply.
func (s *Session) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if params == nil {
		params = struct{}{}
	}

	s.mu.Lock()
	s.nextID++
	id := s.nextID
	waiter := make(chan reply, 1)
	s.pending[id] = waiter
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()

	payload, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}

	s.writeMu.Lock()
	err = wsutil.WriteClientMessage(s.conn, ws.OpText, payload)
	s.writeMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("send %s: %w", method, err)
	}

	select {
	case got := <-waiter:
		if got.err != nil {
			return nil, fmt.Errorf("%s: %w", method, got.err)
		}
		return got.result, nil
	case <-s.closed:
		return nil, fmt.Errorf("%s: bidi connection closed", method)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close shuts the connection down. Safe to call more than once.
func (s *Session) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.closed)
		err = s.conn.Close()
	})
	return err
}

func (s *Session) readLoop() {
	defer s.Close()

	reader := &wsutil.Reader{
		Source:         s.conn,
		State:          ws.StateClientSide,
		OnIntermediate: wsutil.ControlFrameHandler(s.conn, ws.StateClientSide),
	}

	for {
		header, err := reader.NextFrame()
		if err != nil {
			return
		}
		if header.OpCode.IsControl() {
			if err := reader.OnIntermediate(header, reader); err != nil {
				return
			}
			continue
		}
		if header.OpCode != ws.OpText {
			if err := reader.Discard(); err != nil {
				return
			}
			continue
		}

		data, err := io.ReadAll(reader)
		if err != nil {
			return
		}
		s.dispatch(data)
	}
}

func (s *Session) dispatch(data []byte) {
	var message struct {
		Type    string          `json:"type"`
		ID      *int64          `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
		Result  json.RawMessage `json:"result"`
		Error   string          `json:"error"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(data, &message); err != nil {
		return
	}

	if message.Type == "event" {
		s.mu.Lock()
		handler := s.events[message.Method]
		s.mu.Unlock()
		if handler != nil {
			go handler(message.Params)
		}
		return
	}

	if message.ID == nil {
		return
	}
	s.mu.Lock()
	waiter := s.pending[*message.ID]
	s.mu.Unlock()
	if waiter == nil {
		return
	}

	if message.Type == "error" || message.Error != "" {
		waiter <- reply{err: fmt.Errorf("%s: %s", message.Error, message.Message)}
		return
	}
	waiter <- reply{result: message.Result}
}

const (
	// maxAuthAttempts is how many times one request may be answered with
	// credentials before it is cancelled instead.
	maxAuthAttempts = 1
	// maxTrackedRequests bounds the per-request attempt counters. A browser
	// facing this many distinct challenges is already pathological; resetting
	// only costs those requests one extra credential attempt.
	maxTrackedRequests = 4096
)

// InstallProxyAuth answers proxy authentication challenges with the given
// credentials for the lifetime of the session.
//
// The addIntercept call is not optional and not merely an optimisation: without
// an intercept registered for the authRequired phase the event still fires, but
// the request is never held, and continueWithAuth then fails with "no such
// request". Verified against a proxy requiring Basic auth on Firefox 153 and on
// camoufox (Firefox 135).
func InstallProxyAuth(ctx context.Context, s *Session, username, password string, logger browserpkg.Logger) error {
	if _, err := s.Send(ctx, "network.addIntercept", map[string]any{
		"phases": []string{"authRequired"},
	}); err != nil {
		return err
	}

	var (
		mu       sync.Mutex
		attempts = map[string]int{}
	)

	s.On("network.authRequired", func(params json.RawMessage) {
		var event struct {
			Request struct {
				Request string `json:"request"`
				URL     string `json:"url"`
			} `json:"request"`
		}
		if err := json.Unmarshal(params, &event); err != nil || event.Request.Request == "" {
			return
		}

		mu.Lock()
		if len(attempts) > maxTrackedRequests {
			attempts = map[string]int{}
		}
		attempts[event.Request.Request]++
		tries := attempts[event.Request.Request]
		mu.Unlock()

		// Credentials the proxy rejects would otherwise loop forever: Firefox
		// re-challenges, we answer again, and nothing ever fails. Measured with
		// a deliberately wrong password: 613 challenges in 45 seconds, ending
		// with a session too wedged to delete and an orphaned browser process.
		// Answer once, then cancel so the navigation fails cleanly instead.
		payload := map[string]any{"request": event.Request.Request}
		if tries > maxAuthAttempts {
			payload["action"] = "cancel"
			logger.Warn("proxy rejected the supplied credentials", "url", event.Request.URL)
		} else {
			payload["action"] = "provideCredentials"
			payload["credentials"] = map[string]any{
				"type":     "password",
				"username": username,
				"password": password,
			}
			// Logged because "the handler never fired" and "the credentials
			// were wrong" are indistinguishable from outside: both end as 407.
			logger.Debug("answering proxy auth challenge over bidi", "url", event.Request.URL)
		}

		if _, err := s.Send(context.Background(), "network.continueWithAuth", payload); err != nil {
			logger.Warn("could not answer proxy auth challenge", "err", err)
		}
	})

	if _, err := s.Send(ctx, "session.subscribe", map[string]any{
		"events": []string{"network.authRequired"},
	}); err != nil {
		return err
	}
	return nil
}
