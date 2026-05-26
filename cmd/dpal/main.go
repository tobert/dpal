package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	deepseek "github.com/cohesion-org/deepseek-go"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tobert/dpal/internal/explorer"
	"github.com/tobert/dpal/internal/otelinit"
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
	otelEndpointFlag := fs.String("otel-endpoint", "", "OTLP endpoint, e.g. localhost:4317 (gRPC) or localhost:4318 (HTTP). Overrides OTEL_EXPORTER_OTLP_ENDPOINT; empty disables OTel.")
	otelProtocolFlag := fs.String("otel-protocol", "", "OTLP transport: 'grpc' (default) or 'http/protobuf'. Overrides OTEL_EXPORTER_OTLP_PROTOCOL.")
	otelInsecureFlag := fs.Bool("otel-insecure", true, "Send OTLP traces in plaintext (set false to require TLS)")
	rootFlag := fs.String("root", ".", "Directory DeepSeek may inspect via list_directory/read_file/search_project. Combine with --no-explore to disable entirely.")
	noExploreFlag := fs.Bool("no-explore", false, "Disable the exploration tools (list_directory, read_file, search_project)")
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	otelEndpoint := *otelEndpointFlag
	if otelEndpoint == "" {
		otelEndpoint = getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	if otelEndpoint == "" {
		otelEndpoint = getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
	}
	otelProtocol := *otelProtocolFlag
	if otelProtocol == "" {
		otelProtocol = getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	serviceName := getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "dpal"
	}

	shutdown, err := otelinit.Bootstrap(ctx, otelinit.Config{
		Endpoint:    otelEndpoint,
		Protocol:    otelProtocol,
		ServiceName: serviceName,
		Version:     version,
		Insecure:    *otelInsecureFlag,
	})
	if err != nil {
		return fmt.Errorf("otel init: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(shutdownCtx)
	}()

	client := deepseek.NewClient(apiKey)
	srv := server.New(client).WithVersion(version)

	if !*noExploreFlag {
		exp, err := explorer.New(*rootFlag)
		if err != nil {
			return fmt.Errorf("explorer: %w", err)
		}
		srv = srv.WithExplorer(exp)
	}

	mcpServer := mcp.NewServer(&mcp.Implementation{
		Name:    "dpal",
		Version: version,
	}, nil)
	srv.Register(mcpServer)

	return mcpServer.Run(ctx, &mcp.StdioTransport{})
}
