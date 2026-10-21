// Local test door (backdoor.go) replays the gateway answer path over
// loopback HTTP so a conversation can be tested without Discord: cheap
// commands, the rate gate, the think-act loop, and the same reply shaping
// (bound + sign-off + Discord cut) the gateway delivers.
//
// Disabled by default: the door listens only when LOCAL_BACKDOOR_TOKEN is
// set. It binds loopback only (default 127.0.0.1:8099, override via
// LOCAL_BACKDOOR_ADDR) and refuses to boot on any non-loopback address —
// fail closed, never silently remote. Bearer auth uses a constant-time
// compare and the token never reaches logs or responses. The door sends
// nothing and opens no threads, so it can never double-reply or spam.
package discord

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
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"rouge_automaton/internal/gorelay"
)

// converseTimeout bounds one test-door turn like the gateway turn: the
// think-act loop may run up to maxToolRounds rounds at up to 60s each.
const converseTimeout = 300 * time.Second

// converseRequest is the POST /converse body: the question text, the
// caller's Discord user ID (allowlisted like the gateway), an optional
// session for relay continuity, and optional prior turns.
type converseRequest struct {
	Text    string            `json:"text"`
	User    string            `json:"user"`
	Session string            `json:"session,omitempty"`
	History []gorelay.Message `json:"history,omitempty"`
}

// converseResponse is the POST /converse answer: the reply shaped exactly
// as Discord delivery would carry it, plus the session to reuse for
// follow-ups (echoed when provided, generated when empty).
type converseResponse struct {
	Reply   string `json:"reply"`
	Session string `json:"session"`
}

// ValidateBackdoorAddr fails closed: addr must be host:port with an
// explicitly loopback host (127.0.0.0/8, ::1, or localhost). Empty hosts
// (":8099" — all interfaces), wildcards (0.0.0.0), LAN names, and
// unparseable values all refuse to boot.
func ValidateBackdoorAddr(addr string) error {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return fmt.Errorf("local backdoor: empty bind address")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("local backdoor: bad bind address %q: %v", addr, err)
	}
	host = strings.TrimSpace(host)
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("local backdoor: refusing non-loopback bind %q", addr)
	}
	return nil
}

// backdoorSession normalizes a client session ID for relay continuity: the
// relay session is "local:"+id so follow-ups stay continuous without
// touching Discord thread state. Blank IDs get a fresh random hex ID;
// overlong IDs truncate to 64 runes; CR/LF are stripped so the value is
// always header-safe. The returned pair is (relaySession, clientSession).
func backdoorSession(id string) (string, string) {
	id = strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(id), "\r", ""), "\n", "")
	if r := []rune(id); len(r) > 64 {
		id = string(r[:64])
	}
	if id == "" {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			// Crypto failure must never fail the turn: fall back to a
			// time-nonce ID rather than an empty session.
			id = fmt.Sprintf("fallback-%d", time.Now().UnixNano())
		} else {
			id = hex.EncodeToString(b[:])
		}
	}
	return "local:" + id, id
}

// sanitizeHistory mirrors the gateway's history budget for door-provided
// turns: blanks dropped, each turn cut to 500 runes, roles collapsed to
// the gateway's user/assistant pair (arbitrary roles from JSON must never
// reach the model as prompt-steering vectors), and the tail capped past
// the filter so thousands of small turns cannot inflate the turn.
func sanitizeHistory(in []gorelay.Message) []gorelay.Message {
	out := make([]gorelay.Message, 0, len(in))
	for _, m := range in {
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		if utf8.RuneCountInString(m.Content) > 500 {
			m.Content = cutRunes(m.Content, 500)
		}
		if !strings.EqualFold(strings.TrimSpace(m.Role), "assistant") {
			m.Role = "user"
		} else {
			m.Role = "assistant"
		}
		out = append(out, m)
	}
	if len(out) > 20 {
		out = out[len(out)-20:]
	}
	return out
}

// bearerOK constant-time compares the request's bearer token against the
// configured secret. The secret never reaches logs or responses — only the
// boolean outcome does.
func bearerOK(r *http.Request, token string) bool {
	const prefix = "Bearer "
	got := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(got, prefix) {
		return false
	}
	got = strings.TrimSpace(strings.TrimPrefix(got, prefix))
	if got == "" || token == "" {
		return false
	}
	// Length-mismatched inputs return 0 immediately; the equal-length path
	// is constant-time, which is the best the stdlib offers here.
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

func writeBackdoorJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// converseHandler serves POST /converse only: auth, allowlist, then the
// same answer path onMessage runs — cheap commands, rate gate, model
// think-act loop, bound + sign-off + Discord cut. It sends nothing and
// opens no threads.
func (b *Bot) converseHandler(token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/converse", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeBackdoorJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		if !bearerOK(r, token) {
			writeBackdoorJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		// Bound the request body (history rides here): 1MB like the
		// relay response cap, so a stray curl cannot exhaust memory.
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var req converseRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeBackdoorJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
			return
		}
		q := strings.TrimSpace(req.Text)
		user := strings.TrimSpace(req.User)
		if q == "" || user == "" {
			writeBackdoorJSON(w, http.StatusBadRequest, map[string]string{"error": "text and user are required"})
			return
		}
		if !b.Cfg.Allowed(user) {
			writeBackdoorJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
		relaySession, clientSession := backdoorSession(req.Session)
		history := sanitizeHistory(req.History)
		ctx, cancel := context.WithTimeout(r.Context(), converseTimeout)
		defer cancel()
		// Cheap commands burn nothing and ignore the belt, exactly like
		// the gateway: cooldown reads local quota, status probes + caches.
		if isCooldown(q) {
			writeBackdoorJSON(w, http.StatusOK, converseResponse{
				Reply:   cutRunes(b.cooldownReply(user, false), 2000),
				Session: clientSession,
			})
			return
		}
		if isStatus(q) {
			writeBackdoorJSON(w, http.StatusOK, converseResponse{
				Reply:   b.statusAnswer(ctx, user, false),
				Session: clientSession,
			})
			return
		}
		if b.Limiter != nil {
			if ok, eta := b.Limiter.Allow(user, false); !ok {
				secs := int(eta.Round(time.Second).Seconds())
				if secs < 1 {
					secs = 1
				}
				writeBackdoorJSON(w, http.StatusOK, converseResponse{
					Reply:   fmt.Sprintf(gorelay.Busy, strconv.Itoa(secs)+"s"),
					Session: clientSession,
				})
				return
			}
		}
		writeBackdoorJSON(w, http.StatusOK, converseResponse{
			Reply:   b.modelAnswer(ctx, q, history, relaySession),
			Session: clientSession,
		})
	})
	return mux
}

// StartBackdoor validates the bind address fail-closed and serves the test
// door until ctx ends. Callers check the token first: an empty token means
// the door stays shut (logged, not an error).
func (b *Bot) StartBackdoor(ctx context.Context, addr, token string) error {
	if err := ValidateBackdoorAddr(addr); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("local backdoor: listen: %w", err)
	}
	srv := &http.Server{
		Handler:           b.converseHandler(token),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	log.Printf("local backdoor listening on %s", ln.Addr())
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("local backdoor: serve: %w", err)
	}
	return nil
}
