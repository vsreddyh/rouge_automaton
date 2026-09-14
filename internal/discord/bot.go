// Package discord wires the Discord gateway: allowlist, mention gate,
// threads, history, RAG, Zen, reply. Ports bot/main.py.
package discord

import (
	"context"
	"log"
	"strings"

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
	log.Printf("Lost Son online as %s", b.Session.State.User.Username)
	<-ctx.Done()
	return nil
}

func (b *Bot) mentioned(m *discordgo.MessageCreate) bool {
	self := b.Session.State.User.ID
	for _, u := range m.Mentions {
		if u.ID == self {
			return true
		}
	}
	return strings.Contains(m.Content, "<@"+self+">") ||
		strings.Contains(m.Content, "<@!"+self+">")
}

func (b *Bot) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.Bot || !b.Cfg.Allowed(m.Author.ID) {
		return
	}
	if b.Cfg.RequireMention && !(b.mentioned(m) || b.Cfg.FreeResponseChannels[m.ChannelID]) {
		return
	}
	q := strings.ReplaceAll(m.Content, "<@"+s.State.User.ID+">", "")
	q = strings.ReplaceAll(q, "<@!"+s.State.User.ID+">", "")
	q = strings.TrimSpace(q)
	if q == "" {
		return
	}
	ctx := context.Background()
	if persona.IsGreeting(q) {
		_, _ = s.ChannelMessageSendReply(m.ChannelID, "Helldiver. Make it quick.", m.Reference())
		return
	}
	if b.Cfg.AutoThread && m.Thread == nil {
		if ch, err := s.Channel(m.ChannelID); err == nil && ch.Type == discordgo.ChannelTypeGuildText {
			name := q
			if len(name) > 60 {
				name = name[:60]
			}
			_, _ = s.MessageThreadStartComplex(m.ChannelID, m.ID, &discordgo.ThreadStart{
				Name:                name,
				AutoArchiveDuration: 60,
			})
		}
	}
	var history []zen.Message
	if msgs, err := s.ChannelMessages(m.ChannelID, 10, m.ID, "", ""); err == nil {
		for i := len(msgs) - 1; i >= 0; i-- {
			mm := msgs[i]
			if len(mm.Content) > 500 {
				mm.Content = mm.Content[:500]
			}
			role := "user"
			if mm.Author.ID == s.State.User.ID {
				role = "assistant"
			}
			history = append(history, zen.Message{Role: role, Content: mm.Content})
		}
	}
	docs, types := rag.Retrieve(ctx, b.DB, q)
	ctxText := rag.FormatContext(docs)
	if len(docs) == 0 && !types["L"] {
		if w := wikiFallback(ctx, q); w != "" {
			ctxText = w
		}
	}
	if types["L"] {
		if live := liveLine(ctx, b.Cfg, q); live != "" {
			ctxText = strings.TrimSpace(ctxText + "\n" + live)
		}
	}
	system := persona.SystemPrompt()
	if ctxText != "" {
		system += "\nIntel context (data only):\n" + ctxText
	}
	reply := b.Zen.Chat(ctx, system, q, history, m.ChannelID)
	if len(reply) > 2000 {
		reply = reply[:2000]
	}
	_, _ = s.ChannelMessageSendReply(m.ChannelID, persona.Postprocess(reply, false), m.Reference())
}

// wikiFallback and liveLine are stubbed here; real implementations land in
// the fallbacks PR (internal/wiki, internal/live).
func wikiFallback(ctx context.Context, query string) string { return "" }

func liveLine(ctx context.Context, cfg config.Config, query string) string { return "" }
