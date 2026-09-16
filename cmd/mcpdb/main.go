// Command mcpdb serves the intel store as an MCP server over stdio. Tool
// logic lives in internal/mcpdb (shared with the Discord bot); this binary
// is transport plus Mongo wiring only. Register it with any MCP client:
//
//	{"mcpServers": {"rouge": {"command": "/mcpdb",
//	  "env": {"MONGO_URI": "mongodb://127.0.0.1:27017/rouge"}}}}
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"

	"github.com/vsreddyh/rouge_automaton/internal/config"
	"github.com/vsreddyh/rouge_automaton/internal/mcpdb"
)

func main() {
	mongoURI := flag.String("mongo", os.Getenv("MONGO_URI"), "MongoDB URI (default: $MONGO_URI or config default)")
	flag.Parse()
	// Full env config (war-API base/headers included) with the flag winning
	// for Mongo. A hand-built Config would leave the live client headerless.
	cfg := config.Load()
	if strings.TrimSpace(*mongoURI) != "" {
		cfg.MongoURI = *mongoURI
	}
	client, err := mongo.Connect(options.Client().ApplyURI(cfg.MongoURI))
	if err != nil {
		log.Fatalf("mongo: %v", err)
	}
	defer client.Disconnect(context.Background())
	pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx, readpref.Primary()); err != nil {
		log.Fatalf("mongo ping: %v", err)
	}
	// Logs go to stderr: stdout is the JSON-RPC channel.
	log.SetOutput(os.Stderr)
	ex := mcpdb.NewExecutor(client.Database(cfg.DBName()), cfg)
	server := mcpdb.NewServer(ex)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	fmt.Fprintln(os.Stderr, "rouge-mcpdb serving on stdio")
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		log.Fatalf("server: %v", err)
	}
}
