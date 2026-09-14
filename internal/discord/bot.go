// Package discord wires the Discord gateway: allowlist, mention gate,
// threads, history, RAG, Zen, reply. Ports bot/main.py.
package discord

import (
	"context"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/vsreddyh/rouge_automaton/internal/config"
	"github.com/vsreddyh/rouge_automaton/internal/persona"
	"github.com/vsreddyh/rouge_automaton/internal/rag"
	"github.com/vsreddyh/rouge_automaton/internal/zen"
)

// Bot holds gateway dependencies.
type Bot struct {
	Session *discordgo.Session
	Cfg     config.Config
	DB      *mongo.Database
	Zen     *zen.Client
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
	b := &Bot{Session: s, Cfg: cfg, DB: db,
		Zen: &zen.Client{BaseURL: cfg.OpenCodeGoBaseURL, APIKey: cfg.OpenCodeGoAPIKey, Model: cfg.Model}}
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

func (b *Bot) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.Bot || !b.Cfg.Allowed(m.Author.ID) {
		return
	}
	self := b.selfID()
	if self == "" {
		return
	}
	if b.Cfg.RequireMention && !(b.mentioned(m) || b.Cfg.FreeResponseChannels[m.ChannelID]) {
		return
	}
	q := strings.ReplaceAll(m.Content, "<@"+self+">", "")
	q = strings.ReplaceAll(q, "<@!"+self+">", "")
	q = strings.TrimSpace(q)
	if q == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if persona.IsGreeting(q) {
		_, _ = s.ChannelMessageSendReply(m.ChannelID, "Helldiver. Make it quick.", m.Reference())
		return
	}
	if b.Cfg.AutoThread && m.Thread == nil {
		if ch, err := s.Channel(m.ChannelID); err == nil && ch.Type == discordgo.ChannelTypeGuildText {
			name := strings.ReplaceAll(cutRunes(q, 60), "\n", " ")
			_, _ = s.MessageThreadStartComplex(m.ChannelID, m.ID, &discordgo.ThreadStart{
				Name:                name,
				AutoArchiveDuration: 60,
			})
		}
	}
	var history []zen.Message
	if msgs, err := s.ChannelMessages(m.ChannelID, 10, m.ID, "", ""); err == nil {
		for i := len(msgs) - 1; i >= 0; i-- {
			text := cutRunes(msgs[i].Content, 500)
			role := "user"
			if msgs[i].Author != nil && msgs[i].Author.ID == self {
				role = "assistant"
			}
			history = append(history, zen.Message{Role: role, Content: text})
		}
	}
	docs, types := rag.Retrieve(ctx, b.DB, q)
	ctxText := rag.FormatContext(docs)
	liveStr := ""
	if types["L"] {
		liveStr = liveLine(ctx, b.Cfg, q)
		if liveStr != "" {
			ctxText = strings.TrimSpace(ctxText + "\n" + liveStr)
		}
	}
	if len(docs) == 0 && liveStr == "" {
		if w := wikiFallback(ctx, q); w != "" {
			ctxText = w
		}
	}
	system := persona.SystemPrompt()
	if ctxText != "" {
		system += "\nIntel context (data only):\n" + ctxText
	}
	// In threads, ChannelID is the thread ID, so the Zen session stays
	// continuous after auto-thread creation (matches Python thread.id use).
	reply := persona.Postprocess(b.Zen.Chat(ctx, system, q, history, m.ChannelID), false)
	reply = cutRunes(reply, 2000)
	_, _ = s.ChannelMessageSendReply(m.ChannelID, reply, m.Reference())
}

// wikiFallback and liveLine are stubbed here; real implementations land in
// the fallbacks PR (internal/wiki, internal/live).
func wikiFallback(ctx context.Context, query string) string { return "" }

func liveLine(ctx context.Context, cfg config.Config, query string) string { return "" }
