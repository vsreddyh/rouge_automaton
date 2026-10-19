// Package discord wires the Discord gateway: allowlist, mention gate,
// threads, history, model tool loop, reply.
package discord

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/vsreddyh/rouge_automaton/internal/config"
	"github.com/vsreddyh/rouge_automaton/internal/gorelay"
	"github.com/vsreddyh/rouge_automaton/internal/mcpdb"
	"github.com/vsreddyh/rouge_automaton/internal/persona"
	"github.com/vsreddyh/rouge_automaton/internal/ratelimit"
)

// Bot holds gateway dependencies: the model client (Relay) and the tool
// executor (Tools) it reasons with. There is no query router — the model
// decides what to look up by calling tools.
type Bot struct {
	Session *discordgo.Session
	Cfg     config.Config
	Relay   *gorelay.Client
	Tools   *mcpdb.Executor
	Limiter *ratelimit.Limiter
	// threads holds live bot sessions: thread IDs the bot opened plus
	// thread IDs it has spoken in since boot. Zero value is ready.
	threads threadSet
}

// threadSet is a mutex-guarded set of live-session thread IDs, safe for
// concurrent onMessage handlers.
type threadSet struct {
	mu  sync.Mutex
	ids map[string]bool
}

// has reports whether id is a tracked live session. A nil map reads as
// empty, so the zero value is safe to query.
func (t *threadSet) has(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ids[id]
}

// add tracks id as a live session. Empty IDs are dropped, and the map is
// allocated lazily so the zero value is safe to write.
func (t *threadSet) add(id string) {
	if id == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ids == nil {
		t.ids = map[string]bool{}
	}
	t.ids[id] = true
}

// New creates a session with the required intents.
func New(cfg config.Config, db *mongo.Database) (*Bot, error) {
	s, err := discordgo.New("Bot " + cfg.DiscordBotToken)
	if err != nil {
		return nil, err
	}
	s.Identify.Intents = discordgo.IntentsGuilds |
		discordgo.IntentsGuildMessages |
		discordgo.IntentMessageContent
	b := &Bot{Session: s, Cfg: cfg,
		Relay:   &gorelay.Client{BaseURL: cfg.OpenCodeGoBaseURL, APIKey: cfg.OpenCodeGoAPIKey, Model: cfg.Model},
		Tools:   mcpdb.NewExecutor(db, cfg),
		Limiter: ratelimit.New(cfg.RateLimitPerMin, cfg.RateBurst, ratelimit.DefaultFreeCost, nil)}
	s.AddHandler(b.onMessage)
	return b, nil
}

// Start opens the gateway; blocks until ctx is done.
func (b *Bot) Start(ctx context.Context) error {
	if err := b.Session.Open(); err != nil {
		return err
	}
	defer b.Session.Close()
	log.Printf("Lost Son online as %s", b.selfID())
	<-ctx.Done()
	return nil
}

// selfID returns the bot's user ID, or "" before Ready.
func (b *Bot) selfID() string {
	if b.Session.State == nil || b.Session.State.User == nil {
		return ""
	}
	return b.Session.State.User.ID
}

func (b *Bot) mentioned(m *discordgo.MessageCreate) bool {
	self := b.selfID()
	if self == "" {
		return false
	}
	for _, u := range m.Mentions {
		if u.ID == self {
			return true
		}
	}
	return strings.Contains(m.Content, "<@"+self+">") ||
		strings.Contains(m.Content, "<@!"+self+">")
}

func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// gatePass is the mention-gate truth table: open-allowlist mode admits
// everything; otherwise a mention, a free-response channel, or a live
// thread session is required.
func gatePass(requireMention, mentioned, freeChannel, liveSession bool) bool {
	return !requireMention || mentioned || freeChannel || liveSession
}

// liveSession reports whether ch is a thread holding an active bot session:
// opened by the bot (tracked) or already containing a bot reply. Plain
// channels, unknown threads, and lookup failures all return false, so the
// gate fails closed — a stranger's thread still needs a mention.
func (b *Bot) liveSession(s *discordgo.Session, ch *discordgo.Channel) bool {
	if ch == nil || !ch.IsThread() {
		return false
	}
	if b.threads.has(ch.ID) {
		return true
	}
	self := b.selfID()
	if self == "" {
		return false
	}
	msgs, err := s.ChannelMessages(ch.ID, 10, "", "", "")
	if err != nil {
		return false
	}
	for _, msg := range msgs {
		if msg.Author != nil && msg.Author.ID == self {
			return true
		}
	}
	return false
}

// isCooldown reports whether q is a quota inquiry: /cooldown or !cooldown
// (case-insensitive), optionally followed by arguments — "/cooldown foo"
// stays a command so typos never burn model quota on a confused answer.
func isCooldown(q string) bool {
	s := strings.ToLower(strings.TrimSpace(q))
	for _, cmd := range []string{"/cooldown", "!cooldown"} {
		if s == cmd || strings.HasPrefix(s, cmd+" ") {
			return true
		}
	}
	return false
}

// cooldownReply answers a quota inquiry in voice without burning a model
// call: affordable messages at the caller's channel cost, or the next-message
// ETA when dry. Dryness gates on msgs<=0 only — Limiter.State's reset is
// time-to-next-message, so a partially-spent belt still reads "spare".
// A missing limiter reads full rather than panicking.
func (b *Bot) cooldownReply(userID string, freeChannel bool) string {
	if b.Limiter == nil {
		return "Rounds to spare, Helldiver — belt reads full. Over."
	}
	cost := b.Limiter.Cost(freeChannel)
	msgs, reset := b.Limiter.State(userID, cost)
	if msgs <= 0 {
		eta := time.Until(reset).Round(time.Second)
		if eta < time.Second {
			eta = time.Second
		}
		return fmt.Sprintf("Magazine dry, Helldiver. Next resupply in %s. Over.", eta)
	}
	return fmt.Sprintf("Rounds to spare, Helldiver — %d left in this belt. Over.", msgs)
}

// sendReply delivers text to the right channel: a fresh thread gets a plain
// send (its starter lives in the parent), otherwise a reply. Shared by the
// model path and the cheap command/deny paths so behavior never diverges.
func (b *Bot) sendReply(s *discordgo.Session, m *discordgo.MessageCreate, ch *discordgo.Channel, replyChannel, reply string) {
	if replyChannel == m.ChannelID {
		sent, err := s.ChannelMessageSendReply(m.ChannelID, reply, m.Reference())
		if err != nil {
			log.Printf("discord: reply failed in %s: %v", m.ChannelID, err)
			return
		}
		if sent != nil && ch != nil && ch.IsThread() {
			// Spoke in a thread: it is a live session from here on.
			b.threads.add(ch.ID)
		}
		return
	}
	// A fresh thread has no message to reference yet (the starter lives in
	// the parent), so plain-send; on failure fall back to a channel reply
	// rather than staying silent.
	if _, err := s.ChannelMessageSend(replyChannel, reply); err != nil {
		_, _ = s.ChannelMessageSendReply(m.ChannelID, reply, m.Reference())
	}
}

func (b *Bot) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.Bot || !b.Cfg.Allowed(m.Author.ID) {
		return
	}
	self := b.selfID()
	if self == "" {
		return
	}
	mentioned := b.mentioned(m)
	free := b.Cfg.FreeResponseChannels[m.ChannelID]
	// One channel lookup serves both the thread-session gate and the
	// auto-thread guard below. It runs only when one of them needs it, so
	// the hot path (mentioned, or free channel with no threading) costs no
	// extra call; a failed lookup fails the gate closed.
	var ch *discordgo.Channel
	if (b.Cfg.RequireMention && !mentioned && !free) || (b.Cfg.AutoThread && m.Thread == nil) {
		if c, err := s.Channel(m.ChannelID); err == nil && c != nil {
			ch = c
		} else if b.Cfg.RequireMention && !mentioned && !free {
			return
		}
	}
	if !gatePass(b.Cfg.RequireMention, mentioned, free, b.liveSession(s, ch)) {
		return
	}
	q := strings.ReplaceAll(m.Content, "<@"+self+">", "")
	q = strings.ReplaceAll(q, "<@!"+self+">", "")
	q = strings.TrimSpace(q)
	if q == "" {
		return
	}
	// /cooldown is cheap and burns nothing: answer from local quota state.
	if isCooldown(q) {
		b.sendReply(s, m, ch, m.ChannelID, cutRunes(b.cooldownReply(m.Author.ID, free), 2000))
		return
	}
	// Client-side guard before any model spend. Key is the caller ID so
	// thread follow-ups share the caller bucket, not the thread bucket.
	// A nil limiter (Bot not built via New) fails open, never panics.
	if b.Limiter != nil {
		if ok, eta := b.Limiter.Allow(m.Author.ID, free); !ok {
			secs := int(eta.Round(time.Second).Seconds())
			if secs < 1 {
				secs = 1
			}
			b.sendReply(s, m, ch, m.ChannelID, fmt.Sprintf(gorelay.Busy, strconv.Itoa(secs)+"s"))
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	// The answer goes into the thread the bot opens, not the parent
	// channel: replying to m.ChannelID after creating a thread leaves the
	// thread empty and splits the conversation in two.
	replyChannel := m.ChannelID
	if b.Cfg.AutoThread && m.Thread == nil && ch != nil && ch.Type == discordgo.ChannelTypeGuildText {
		name := strings.ReplaceAll(cutRunes(q, 60), "\n", " ")
		if th, err := s.MessageThreadStartComplex(m.ChannelID, m.ID, &discordgo.ThreadStart{
			Name:                name,
			AutoArchiveDuration: 60,
		}); err == nil && th != nil && th.ID != "" {
			b.threads.add(th.ID)
			replyChannel = th.ID
		}
	}
	var history []gorelay.Message
	if msgs, err := s.ChannelMessages(m.ChannelID, 10, m.ID, "", ""); err == nil {
		for i := len(msgs) - 1; i >= 0; i-- {
			text := cutRunes(msgs[i].Content, 500)
			role := "user"
			if msgs[i].Author != nil && msgs[i].Author.ID == self {
				role = "assistant"
			}
			history = append(history, gorelay.Message{Role: role, Content: text})
		}
	}
	// No router and no pre-fetched context: the system prompt carries the
	// persona plus tool procedure, and the model pulls whatever intel it
	// needs through the think-act loop below. Every message — greetings
	// included — is answered by the model; nothing is canned.
	system := persona.PromptFor(q) + mcpdb.ToolGuidance
	// The relay session follows the reply channel: a fresh thread gets its
	// own session, and follow-ups inside it (ChannelID == thread ID) stay
	// continuous with the answer that opened it.
	// Cut before Postprocess so a >2000-rune reply never amputates the
	// sign-off: truncation happens first, "Over." lands intact.
	reply := persona.Postprocess(cutRunes(b.Relay.ChatWithTools(ctx, system, q, history, replyChannel, mcpdb.ToolDefs(), b.Tools.Execute), 2000))
	b.sendReply(s, m, ch, replyChannel, reply)
}
