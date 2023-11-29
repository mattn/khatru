package khatru

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fasthttp/websocket"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip42"
	"github.com/rs/cors"
)

// ServeHTTP implements http.Handler interface.
func (rl *Relay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Upgrade") == "websocket" {
		rl.HandleWebsocket(w, r)
	} else if r.Header.Get("Accept") == "application/nostr+json" {
		cors.AllowAll().Handler(http.HandlerFunc(rl.HandleNIP11)).ServeHTTP(w, r)
	} else {
		rl.serveMux.ServeHTTP(w, r)
	}
}

func (rl *Relay) HandleWebsocket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	conn, err := rl.upgrader.Upgrade(w, r, nil)
	if err != nil {
		rl.Log.Printf("failed to upgrade websocket: %v\n", err)
		return
	}
	rl.clients.Store(conn, struct{}{})
	ticker := time.NewTicker(rl.PingPeriod)

	// NIP-42 challenge
	challenge := make([]byte, 8)
	rand.Read(challenge)

	ws := &WebSocket{
		conn:           conn,
		Challenge:      hex.EncodeToString(challenge),
		WaitingForAuth: make(chan struct{}),
	}

	ctx = context.WithValue(ctx, WS_KEY, ws)

	// reader
	go func() {
		defer func() {
			ticker.Stop()
			if _, ok := rl.clients.Load(conn); ok {
				conn.Close()
				rl.clients.Delete(conn)
				removeListener(ws)
			}
		}()

		conn.SetReadLimit(rl.MaxMessageSize)
		conn.SetReadDeadline(time.Now().Add(rl.PongWait))
		conn.SetPongHandler(func(string) error {
			conn.SetReadDeadline(time.Now().Add(rl.PongWait))
			return nil
		})

		for _, onconnect := range rl.OnConnect {
			onconnect(ctx)
		}

		for {
			typ, message, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(
					err,
					websocket.CloseNormalClosure,    // 1000
					websocket.CloseGoingAway,        // 1001
					websocket.CloseNoStatusReceived, // 1005
					websocket.CloseAbnormalClosure,  // 1006
				) {
					rl.Log.Printf("unexpected close error from %s: %v\n", r.Header.Get("X-Forwarded-For"), err)
				}
				break
			}

			if typ == websocket.PingMessage {
				ws.WriteMessage(websocket.PongMessage, nil)
				continue
			}

			go func(message []byte) {
				ctx = context.Background()

				envelope := nostr.ParseMessage(message)
				if envelope == nil {
					// stop silently
					return
				}

				switch env := envelope.(type) {
				case *nostr.EventEnvelope:
					// check id
					hash := sha256.Sum256(env.Event.Serialize())
					id := hex.EncodeToString(hash[:])
					if id != env.Event.ID {
						ws.WriteJSON(nostr.OKEnvelope{EventID: env.Event.ID, OK: false, Reason: "invalid: id is computed incorrectly"})
						return
					}

					// check signature
					if ok, err := env.Event.CheckSignature(); err != nil {
						ws.WriteJSON(nostr.OKEnvelope{EventID: env.Event.ID, OK: false, Reason: "error: failed to verify signature"})
						return
					} else if !ok {
						ws.WriteJSON(nostr.OKEnvelope{EventID: env.Event.ID, OK: false, Reason: "invalid: signature is invalid"})
						return
					}

					var ok bool
					if env.Event.Kind == 5 {
						err = rl.handleDeleteRequest(ctx, &env.Event)
					} else {
						err = rl.AddEvent(ctx, &env.Event)
					}

					var reason string
					if err == nil {
						ok = true
					} else {
						reason = nostr.NormalizeOKMessage(err.Error(), "blocked")
					}
					ws.WriteJSON(nostr.OKEnvelope{EventID: env.Event.ID, OK: ok, Reason: reason})
				case *nostr.CountEnvelope:
					if rl.CountEvents == nil {
						ws.WriteJSON(nostr.ClosedEnvelope{SubscriptionID: env.SubscriptionID, Reason: "unsupported: this relay does not support NIP-45"})
						return
					}
					var total int64
					for _, filter := range env.Filters {
						total += rl.handleCountRequest(ctx, ws, filter)
					}
					ws.WriteJSON(nostr.CountEnvelope{SubscriptionID: env.SubscriptionID, Count: &total})
				case *nostr.ReqEnvelope:
					eose := sync.WaitGroup{}
					eose.Add(len(env.Filters))

					isFullyRejected := true
					var reason string
					for _, filter := range env.Filters {
						err := rl.handleRequest(ctx, env.SubscriptionID, &eose, ws, filter)
						if err == nil {
							isFullyRejected = false
						} else {
							reason = err.Error()
						}
					}
					if isFullyRejected {
						// this will be called only if all the filters were invalidated
						reason = nostr.NormalizeOKMessage(reason, "blocked")
						ws.WriteJSON(nostr.ClosedEnvelope{SubscriptionID: env.SubscriptionID, Reason: reason})
						return
					}

					go func() {
						eose.Wait()
						ws.WriteJSON(nostr.EOSEEnvelope(env.SubscriptionID))
					}()

					setListener(env.SubscriptionID, ws, env.Filters)
				case *nostr.CloseEnvelope:
					removeListenerId(ws, string(*env))
				case *nostr.AuthEnvelope:
					if rl.ServiceURL != "" {
						if pubkey, ok := nip42.ValidateAuthEvent(&env.Event, ws.Challenge, rl.ServiceURL); ok {
							ws.Authed = pubkey
							close(ws.WaitingForAuth)
							ctx = context.WithValue(ctx, AUTH_CONTEXT_KEY, pubkey)
							ws.WriteJSON(nostr.OKEnvelope{EventID: env.Event.ID, OK: true})
						} else {
							ws.WriteJSON(nostr.OKEnvelope{EventID: env.Event.ID, OK: false, Reason: "error: failed to authenticate"})
						}
					}
				}
			}(message)
		}
	}()

	// writer
	go func() {
		defer func() {
			ticker.Stop()
			conn.Close()
		}()

		for {
			select {
			case <-ticker.C:
				err := ws.WriteMessage(websocket.PingMessage, nil)
				if err != nil {
					if !strings.HasSuffix(err.Error(), "use of closed network connection") {
						rl.Log.Printf("error writing ping: %v; closing websocket\n", err)
					}
					return
				}
			}
		}
	}()
}

func (rl *Relay) HandleNIP11(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/nostr+json")

	info := *rl.Info
	for _, ovw := range rl.OverwriteRelayInformation {
		info = ovw(r.Context(), r, info)
	}

	json.NewEncoder(w).Encode(info)
}
