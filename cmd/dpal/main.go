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
	apiKeyFlag := fs.String("api-key", "", "DeepSeek API key (visible via 'ps'; prefer --api-key-file)")
	apiKeyFileFlag := fs.String("api-key-file", "", "Path to a file containing the DeepSeek API key; read at every startup")
	otelEndpointFlag := fs.String("otel-endpoint", "", "OTLP endpoint, e.g. localhost:4317 (gRPC) or localhost:4318 (HTTP). Overrides OTEL_EXPORTER_OTLP_ENDPOINT; empty disables OTel.")
	otelEndpointFileFlag := fs.String("otel-endpoint-file", "", "Path to a file containing the OTLP endpoint; read at every startup (matches --api-key-file). Useful for collectors that bind a random port at startup.")
	otelProtocolFlag := fs.String("otel-protocol", "", "OTLP transport: 'grpc' (default) or 'http/protobuf'. Overrides OTEL_EXPORTER_OTLP_PROTOCOL.")
	otelInsecureFlag := fs.Bool("otel-insecure", true, "Send OTLP traces in plaintext (set false to require TLS)")
	rootFlag := fs.String("root", ".", "Directory DeepSeek may inspect via list_directory/read_file/search_project. Combine with --no-explore to disable entirely.")
	noExploreFlag := fs.Bool("no-explore", false, "Disable the exploration tools (list_directory, read_file, search_project)")
	configFlag := fs.String("config", "", "Path to TOML config (default: $XDG_CONFIG_HOME/dpal/config.toml or ~/.config/dpal/config.toml)")
	noDefaultPromptFlag := fs.Bool("no-default-prompt", false, "Suppress the built-in DeepSeek system prompt")
	var systemPromptFiles stringSliceFlag
	fs.Var(&systemPromptFiles, "system-prompt", "Path to a file whose contents append to the system prompt. Repeatable.")
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *showVersion {
		fmt.Println(version)
		return nil
	}

	apiKey, err := resolveAPIKey(*apiKeyFlag, *apiKeyFileFlag, getenv)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	otelEndpoint, err := resolveOTelEndpoint(*otelEndpointFlag, *otelEndpointFileFlag, getenv)
	if err != nil {
		return err
	}
	otelProtocol := *otelProtocolFlag
	if otelProtocol == "" {
		otelProtocol = getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	serviceName := getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "dpal"
	}

	shutdown, otelErr := otelinit.Bootstrap(ctx, otelinit.Config{
		Endpoint:    otelEndpoint,
		Protocol:    otelProtocol,
		ServiceName: serviceName,
		Version:     version,
		Insecure:    *otelInsecureFlag,
	})
	if otelErr != nil {
		return fmt.Errorf("otel init: %w", otelErr)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(shutdownCtx); err != nil {
			log.Printf("dpal: otel shutdown: %v", err)
		}
	}()

	cfg := loadConfig(*configFlag, getenv)
	sysPrompt, err := composeSystemPrompt(cfg, systemPromptFiles, *noDefaultPromptFlag)
	if err != nil {
		return fmt.Errorf("system prompt: %w", err)
	}

	client := deepseek.NewClient(apiKey)
	srv := server.New(client).
		WithVersion(version).
		WithSystemPrompt(sysPrompt).
		WithExplorerSystemPrompt(defaultExplorerSystemPrompt)

	if !*noExploreFlag {
		exp, expErr := explorer.New(*rootFlag)
		if expErr != nil {
			return fmt.Errorf("explorer: %w", expErr)
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

// resolveOTelEndpoint applies the precedence rule:
//
//	--otel-endpoint  XOR  --otel-endpoint-file  ->
//	  OTEL_EXPORTER_OTLP_ENDPOINT  ->  OTEL_EXPORTER_OTLP_TRACES_ENDPOINT
//
// Same shape as resolveAPIKey so collectors that bind ephemeral ports
// (e.g. otlp-mcp) can be picked up at every dpal startup without
// re-running 'claude mcp add'.
func resolveOTelEndpoint(fromFlag, fromFile string, getenv func(string) string) (string, error) {
	if fromFlag != "" && fromFile != "" {
		return "", fmt.Errorf("--otel-endpoint and --otel-endpoint-file are mutually exclusive")
	}
	if fromFlag != "" {
		return fromFlag, nil
	}
	if fromFile != "" {
		data, err := os.ReadFile(fromFile)
		if err != nil {
			return "", fmt.Errorf("read --otel-endpoint-file: %w", err)
		}
		endpoint := strings.TrimSpace(string(data))
		if endpoint == "" {
			return "", fmt.Errorf("--otel-endpoint-file %q is empty", fromFile)
		}
		return endpoint, nil
	}
	if env := getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); env != "" {
		return env, nil
	}
	if env := getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"); env != "" {
		return env, nil
	}
	return "", nil
}

// resolveAPIKey applies the precedence rule:
//
//	--api-key  XOR  --api-key-file  ->  DEEPSEEK_API_KEY
//
// Both flags together is an error so the caller's intent isn't
// ambiguous. The file path is read fresh on every startup and trimmed
// of surrounding whitespace, so a trailing newline is fine.
func resolveAPIKey(fromFlag, fromFile string, getenv func(string) string) (string, error) {
	if fromFlag != "" && fromFile != "" {
		return "", fmt.Errorf("--api-key and --api-key-file are mutually exclusive")
	}
	if fromFlag != "" {
		return fromFlag, nil
	}
	if fromFile != "" {
		data, err := os.ReadFile(fromFile)
		if err != nil {
			return "", fmt.Errorf("read --api-key-file: %w", err)
		}
		key := strings.TrimSpace(string(data))
		if key == "" {
			return "", fmt.Errorf("--api-key-file %q is empty", fromFile)
		}
		return key, nil
	}
	if env := getenv("DEEPSEEK_API_KEY"); env != "" {
		return env, nil
	}
	return "", fmt.Errorf("API key required: pass --api-key-file (recommended), --api-key, or set DEEPSEEK_API_KEY")
}
