package main

import (
	"context"
	"fmt"
	"io"
	stdlog "log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/irfansofyana/linkwarden-mcp-server/pkg/linkwarden"
	"github.com/irfansofyana/linkwarden-mcp-server/pkg/linkwardenmcp"
	"github.com/irfansofyana/linkwarden-mcp-server/pkg/log"
	"github.com/irfansofyana/linkwarden-mcp-server/pkg/mcpgo"
	"github.com/irfansofyana/linkwarden-mcp-server/pkg/observability"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	version = "version"
	commit  = "commit"
	date    = "date"
)

var cfgFile string

var rootCmd = &cobra.Command{
	Use:     "server",
	Short:   "Linkwarden MCP Server",
	Version: fmt.Sprintf("%s\ncommit %s\ndate %s", version, commit, date),
}

// initConfig reads in config file and ENV variables if set.
func initConfig() {
	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	} else {
		home, err := os.UserHomeDir()
		cobra.CheckErr(err)

		viper.AddConfigPath(home)
		viper.SetConfigType("yaml")
		viper.SetConfigName("linkwarden-mcp-server")
	}

	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err == nil {
		fmt.Fprintln(os.Stderr, "Using config file:", viper.ConfigFileUsed())
	}
}

// newLinkwardenClient builds an API client that authenticates every request
// with the configured token
func newLinkwardenClient() (*linkwarden.ClientWithResponses, error) {
	token := viper.GetString("token")
	baseUrl := viper.GetString("base_url")

	return linkwarden.NewClientWithResponses(baseUrl, linkwarden.WithRequestEditorFn(
		func(ctx context.Context, req *http.Request) error {
			req.Header.Set("Authorization", "Bearer "+token)
			return nil
		},
	))
}

// stdioCmd starts the mcp server in stdio transport mode
var stdioCmd = &cobra.Command{
	Use:   "stdio",
	Short: "start the stdio server",
	Run: func(cmd *cobra.Command, args []string) {
		logPath := viper.GetString("log_file")

		config := log.NewConfig(
			log.WithMode(log.ModeStdio),
			log.WithLogLevel(slog.LevelInfo),
			log.WithLogPath(logPath),
		)

		ctx, logger := log.New(context.Background(), config)

		// Create observability with logging
		obs := observability.New(
			observability.WithLogging(logger),
		)

		client, err := newLinkwardenClient()
		if err != nil {
			obs.Logger.Errorf(ctx,
				"error running stdio server", "error", err)
			stdlog.Fatalf("failed to run stdio server: %v", err)
		}

		// Get toolsets to enable from config
		enabledToolsets := viper.GetStringSlice("toolsets")

		// Get read-only mode from config
		readOnly := viper.GetBool("read_only")

		if err := runStdioServer(ctx, obs, client, enabledToolsets, readOnly); err != nil {
			obs.Logger.Errorf(ctx,
				"error running stdio server", "error", err)
			stdlog.Fatalf("failed to run stdio server: %v", err)
		}
	},
}

// httpCmd starts the mcp server in streamable HTTP transport mode
var httpCmd = &cobra.Command{
	Use:   "http",
	Short: "start the streamable HTTP server",
	Long: "Start the MCP server over streamable HTTP so it can run as a " +
		"long-lived service and be reached by remote MCP clients.",
	Run: func(cmd *cobra.Command, args []string) {
		logPath := viper.GetString("log_file")

		config := log.NewConfig(
			log.WithMode(log.ModeStdio),
			log.WithLogLevel(slog.LevelInfo),
			log.WithLogPath(logPath),
		)

		ctx, logger := log.New(context.Background(), config)

		obs := observability.New(
			observability.WithLogging(logger),
		)

		client, err := newLinkwardenClient()
		if err != nil {
			obs.Logger.Errorf(ctx, "error running http server", "error", err)
			stdlog.Fatalf("failed to run http server: %v", err)
		}

		oauthCfg := mcpgo.OAuthConfig{
			Enabled:      viper.GetBool("mcp_oauth_enabled"),
			Issuer:       viper.GetString("mcp_oauth_issuer"),
			ServerURL:    viper.GetString("mcp_server_url"),
			MCPPath:      viper.GetString("mcp_path"),
			Audience:     viper.GetString("mcp_oauth_audience"),
			JWKSCacheTTL: time.Duration(viper.GetInt("mcp_oauth_jwks_cache_ttl")) * time.Second,
			Groups: mcpgo.GroupPolicy{
				Claim:       viper.GetString("mcp_groups_claim"),
				ReadGroups:  mcpgo.ParseGroupList(viper.GetString("mcp_read_groups")),
				WriteGroups: mcpgo.ParseGroupList(viper.GetString("mcp_write_groups")),
			},
		}

		addr := net.JoinHostPort(
			viper.GetString("mcp_host"),
			viper.GetString("mcp_port"),
		)

		enabledToolsets := viper.GetStringSlice("toolsets")
		readOnly := viper.GetBool("read_only")

		if err := runHTTPServer(
			ctx, obs, client, enabledToolsets, readOnly, addr, oauthCfg,
		); err != nil {
			obs.Logger.Errorf(ctx, "error running http server", "error", err)
			stdlog.Fatalf("failed to run http server: %v", err)
		}
	},
}

func runStdioServer(
	ctx context.Context,
	obs *observability.Observability,
	client *linkwarden.ClientWithResponses,
	enabledToolsets []string,
	readOnly bool,
) error {
	ctx, stop := signal.NotifyContext(
		ctx,
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stop()

	srv, err := linkwardenmcp.NewLinkwardenMcpServer(obs, client, enabledToolsets, readOnly)
	if err != nil {
		return fmt.Errorf("failed to create server: %w", err)
	}

	stdioSrv, err := mcpgo.NewStdioServer(srv)
	if err != nil {
		return fmt.Errorf("failed to create stdio server: %w", err)
	}

	in, out := io.Reader(os.Stdin), io.Writer(os.Stdout)
	errC := make(chan error, 1)
	go func() {
		obs.Logger.Infof(ctx, "starting server")
		errC <- stdioSrv.Listen(ctx, in, out)
	}()

	_, _ = fmt.Fprintf(
		os.Stderr,
		"Linkwarden MCP Server running on stdio\n",
	)

	// Wait for shutdown signal
	select {
	case <-ctx.Done():
		obs.Logger.Infof(ctx, "shutting down server...")
		return nil
	case err := <-errC:
		if err != nil {
			obs.Logger.Errorf(ctx, "server error", "error", err)
			return err
		}
		return nil
	}
}

func runHTTPServer(
	ctx context.Context,
	obs *observability.Observability,
	client *linkwarden.ClientWithResponses,
	enabledToolsets []string,
	readOnly bool,
	addr string,
	oauthCfg mcpgo.OAuthConfig,
) error {
	ctx, stop := signal.NotifyContext(
		ctx,
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stop()

	srv, err := linkwardenmcp.NewLinkwardenMcpServer(obs, client, enabledToolsets, readOnly)
	if err != nil {
		return fmt.Errorf("failed to create server: %w", err)
	}

	mcpPath := oauthCfg.MCPPath
	if mcpPath == "" {
		mcpPath = "/mcp"
	}

	transportCfg := mcpgo.StreamableHTTPConfig{
		EndpointPath: mcpPath,
		ExtraHandlers: map[string]http.Handler{
			"/healthz": http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"status":"ok"}`))
				}),
		},
	}

	if oauthCfg.Enabled {
		resourceSrv, err := mcpgo.NewResourceServer(oauthCfg, obs)
		if err != nil {
			return fmt.Errorf("failed to configure oauth: %w", err)
		}

		transportCfg.Middleware = resourceSrv.Middleware
		for path, handler := range resourceSrv.Handlers() {
			transportCfg.ExtraHandlers[path] = handler
		}

		obs.Logger.Infof(ctx, "OAUTH_ENABLED",
			"issuer", resourceSrv.Issuer(),
			"resource", resourceSrv.ResourceURL())
	} else {
		obs.Logger.Warningf(ctx, "OAUTH_DISABLED",
			"detail", "the mcp endpoint is served without authentication")
	}

	httpSrv, err := mcpgo.NewStreamableHTTPServer(srv, transportCfg)
	if err != nil {
		return fmt.Errorf("failed to create http server: %w", err)
	}

	errC := make(chan error, 1)
	go func() {
		obs.Logger.Infof(ctx, "starting server", "address", addr, "path", mcpPath)
		errC <- httpSrv.Start(addr)
	}()

	_, _ = fmt.Fprintf(
		os.Stderr,
		"Linkwarden MCP Server running on http://%s%s\n", addr, mcpPath,
	)

	// Wait for shutdown signal
	select {
	case <-ctx.Done():
		obs.Logger.Infof(ctx, "shutting down server...")

		shutdownCtx, cancel := context.WithTimeout(
			context.Background(), 10*time.Second)
		defer cancel()

		return httpSrv.Shutdown(shutdownCtx)
	case err := <-errC:
		if err != nil {
			obs.Logger.Errorf(ctx, "server error", "error", err)
			return err
		}
		return nil
	}
}

func init() {
	cobra.OnInitialize(initConfig)

	rootCmd.PersistentFlags().StringP("base-url", "b", "", "your linkwarden base url")
	rootCmd.PersistentFlags().StringP("token", "s", "", "your linkwarden secret / token")
	rootCmd.PersistentFlags().StringP("log-file", "l", "", "path to the log file")
	rootCmd.PersistentFlags().StringSliceP("toolsets", "t", []string{}, "comma-separated list of toolsets to enable")
	rootCmd.PersistentFlags().Bool("read-only", false, "run server in read-only mode")

	_ = viper.BindPFlag("base_url", rootCmd.PersistentFlags().Lookup("base-url"))
	_ = viper.BindPFlag("token", rootCmd.PersistentFlags().Lookup("token"))
	_ = viper.BindPFlag("log_file", rootCmd.PersistentFlags().Lookup("log-file"))
	_ = viper.BindPFlag("toolsets", rootCmd.PersistentFlags().Lookup("toolsets"))
	_ = viper.BindPFlag("read_only", rootCmd.PersistentFlags().Lookup("read-only"))

	_ = viper.BindEnv("base_url", "LINKWARDEN_BASE_URL")
	_ = viper.BindEnv("token", "LINKWARDEN_TOKEN")

	// http transport flags
	httpCmd.Flags().String("host", "0.0.0.0", "address to bind the http server to")
	httpCmd.Flags().String("port", "8080", "port to listen on")
	httpCmd.Flags().String("mcp-path", "/mcp", "path the mcp endpoint is served on")
	httpCmd.Flags().Bool("oauth-enabled", false, "require OAuth 2.1 bearer tokens")
	httpCmd.Flags().String("oauth-issuer", "", "OIDC provider url (required when oauth is enabled)")
	httpCmd.Flags().String("server-url", "", "public base url of this server (required when oauth is enabled)")
	httpCmd.Flags().String("oauth-audience", "", "expected token audience (optional)")
	httpCmd.Flags().Int("oauth-jwks-cache-ttl", 3600, "seconds to cache provider discovery")
	httpCmd.Flags().String("groups-claim", "groups", "token claim holding the caller's groups")
	httpCmd.Flags().String("read-groups", "", "comma-separated groups granted read tools only")
	httpCmd.Flags().String("write-groups", "", "comma-separated groups granted every tool")

	_ = viper.BindPFlag("mcp_host", httpCmd.Flags().Lookup("host"))
	_ = viper.BindPFlag("mcp_port", httpCmd.Flags().Lookup("port"))
	_ = viper.BindPFlag("mcp_path", httpCmd.Flags().Lookup("mcp-path"))
	_ = viper.BindPFlag("mcp_oauth_enabled", httpCmd.Flags().Lookup("oauth-enabled"))
	_ = viper.BindPFlag("mcp_oauth_issuer", httpCmd.Flags().Lookup("oauth-issuer"))
	_ = viper.BindPFlag("mcp_server_url", httpCmd.Flags().Lookup("server-url"))
	_ = viper.BindPFlag("mcp_oauth_audience", httpCmd.Flags().Lookup("oauth-audience"))
	_ = viper.BindPFlag("mcp_oauth_jwks_cache_ttl", httpCmd.Flags().Lookup("oauth-jwks-cache-ttl"))
	_ = viper.BindPFlag("mcp_groups_claim", httpCmd.Flags().Lookup("groups-claim"))
	_ = viper.BindPFlag("mcp_read_groups", httpCmd.Flags().Lookup("read-groups"))
	_ = viper.BindPFlag("mcp_write_groups", httpCmd.Flags().Lookup("write-groups"))

	_ = viper.BindEnv("mcp_host", "MCP_HOST")
	_ = viper.BindEnv("mcp_port", "MCP_PORT")
	_ = viper.BindEnv("mcp_path", "MCP_PATH")
	_ = viper.BindEnv("mcp_oauth_enabled", "MCP_OAUTH_ENABLED")
	_ = viper.BindEnv("mcp_oauth_issuer", "MCP_OAUTH_ISSUER")
	_ = viper.BindEnv("mcp_server_url", "MCP_SERVER_URL")
	_ = viper.BindEnv("mcp_oauth_audience", "MCP_OAUTH_AUDIENCE")
	_ = viper.BindEnv("mcp_oauth_jwks_cache_ttl", "MCP_OAUTH_JWKS_CACHE_TTL")
	_ = viper.BindEnv("mcp_groups_claim", "MCP_GROUPS_CLAIM")
	_ = viper.BindEnv("mcp_read_groups", "MCP_READ_GROUPS")
	_ = viper.BindEnv("mcp_write_groups", "MCP_WRITE_GROUPS")

	// Enable environment variable reading
	viper.AutomaticEnv()

	// subcommands
	rootCmd.AddCommand(stdioCmd)
	rootCmd.AddCommand(httpCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
