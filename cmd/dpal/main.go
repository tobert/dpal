package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	deepseek "github.com/cohesion-org/deepseek-go"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tobert/dpal/internal/server"
)

const version = "0.1.0"

func main() {
	if err := run(os.Args[1:], os.Getenv); err != nil {
		log.Fatalf("dpal: %v", err)
	}
}

func run(args []string, getenv func(string) string) error {
	fs := flag.NewFlagSet("dpal", flag.ContinueOnError)
	apiKeyFlag := fs.String("api-key", "", "DeepSeek API key (overrides DEEPSEEK_API_KEY)")
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *showVersion {
		fmt.Println(version)
		return nil
	}

	apiKey := *apiKeyFlag
	if apiKey == "" {
		apiKey = getenv("DEEPSEEK_API_KEY")
	}
	if apiKey == "" {
		return fmt.Errorf("API key required: pass --api-key or set DEEPSEEK_API_KEY")
	}

	client := deepseek.NewClient(apiKey)
	srv := server.New(client)

	mcpServer := mcp.NewServer(&mcp.Implementation{
		Name:    "dpal",
		Version: version,
	}, nil)
	srv.Register(mcpServer)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return mcpServer.Run(ctx, &mcp.StdioTransport{})
}
