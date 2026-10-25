// Command hawk-mcp-server exposes the hawk-apiserver `mcp-api` branch's /api/mcp/v1
// endpoints as MCP tools.
//
// It is deliberately thin: openapi.yaml (embedded into the binary) defines the
// generated tools, github.com/evcc-io/openapi-mcp turns every operation in it
// into one MCP tool that proxies the real HTTP call, and resource_explain /
// node_drain are registered by hand in main.go.
//
// Typical use:
//
//	hawk-mcp-server -hawk-url https://127.0.0.1:7630 -http 127.0.0.1:9000
//
// then point an MCP client at http://127.0.0.1:9000/mcp. Without -http the
// server speaks stdio instead.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	openapi2mcp "github.com/evcc-io/openapi-mcp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

//go:embed openapi.yaml
var openAPISpec []byte

var (
	httpAddr  = flag.String("http", "", "Streamable HTTP address to listen on (for example 127.0.0.1:9000). Without it, stdio transport is used")
	hawkURL   = flag.String("hawk-url", "", "base URL of the hawk-apiserver `mcp-api` branch, for example https://127.0.0.1:7630")
	tokenFile = flag.String("token-file", "/etc/hawk/mcp.token", "file containing the bearer token for /api/mcp/v1")
	caFile    = flag.String("ca-file", "/etc/hawk/hawk.pem", "PEM file with the certificate to trust for hawk-apiserver's TLS endpoint; empty string disables custom trust")
	insecure  = flag.Bool("insecure", false, "skip TLS certificate verification (testing only)")
)

func main() {
	flag.Parse()

	if *hawkURL == "" {
		slog.Error("-hawk-url must be set (for example https://127.0.0.1:7630)")
		flag.Usage()
		os.Exit(1)
	}

	token, err := readToken(*tokenFile)
	if err != nil {
		slog.Error("reading bearer token", "file", *tokenFile, "err", err)
		os.Exit(1)
	}

	// openapi-mcp reads both the base URL and the credential from the
	// environment (it fulfils a `http`/`bearer` security scheme by setting
	// "Authorization: Bearer $BEARER_TOKEN").
	if err := os.Setenv("OPENAPI_BASE_URL", *hawkURL); err != nil {
		slog.Error("setting OPENAPI_BASE_URL", "err", err)
		os.Exit(1)
	}
	if err := os.Setenv("BEARER_TOKEN", token); err != nil {
		slog.Error("setting BEARER_TOKEN", "err", err)
		os.Exit(1)
	}

	// hawk-apiserver serves HTTPS with a self-signed certificate by default.
	// openapi-mcp issues its requests through http.DefaultClient, so installing
	// a trust configuration here is enough — no fork or wrapper needed.
	if err := configureTLS(*caFile, *insecure); err != nil {
		slog.Error("configuring TLS trust", "ca_file", *caFile, "err", err)
		os.Exit(1)
	}

	doc, err := openapi2mcp.LoadOpenAPISpecFromBytes(openAPISpec)
	if err != nil {
		slog.Error("loading embedded OpenAPI document", "err", err)
		os.Exit(1)
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:  "hawk-mcp-server",
		Title: "Hawk MCP Server",
	}, nil)

	ops := openapi2mcp.ExtractOpenAPIOperations(doc)
	names := openapi2mcp.RegisterOpenAPITools(server, ops, doc, nil)

	// Hand-written tools. Auto-generated tools are a poor fit for answering one
	// question from several responses (resource_explain) and for operations that
	// need to wait and verify (node_drain).
	hawk := &hawkClient{baseURL: *hawkURL, token: token}
	mcp.AddTool(server, &mcp.Tool{
		Name: "resource_explain",
		Description: "Explain one resource in one call: its agent, current role, node, " +
			"parameters and meta attributes, plus a hint for each matching condition " +
			"(failed, maintenance, orphaned, blocked), a hint when the resource is " +
			"unmanaged or started without a node, and the last recorded failure. " +
			"Use it instead of joining resources_list and " +
			"crm_status_get by hand, and before deciding whether to call resource_cleanup, " +
			"resource_maintenance_off or resource_parameter_set.",
	}, hawk.resourceExplain)
	mcp.AddTool(server, &mcp.Tool{
		Name: "node_drain",
		Description: "Put a node into standby and wait until its resources have moved away. " +
			"Reports drained=true when the node runs nothing, or the remaining resource ids " +
			"when the wait times out. Requires confirm=true; without it nothing is changed " +
			"and the result explains what would happen. Use it for planned maintenance of a " +
			"node; use node_standby_on directly when you only want to start the operation. The node stays in standby afterwards; call node_standby_off when the maintenance is done.",
	}, hawk.nodeDrain)

	allTools := append(names, "info", "resource_explain", "node_drain")
	slog.Info("registered MCP tools", "tools", strings.Join(allTools, ","), "hawk_url", *hawkURL)

	if *httpAddr != "" {
		handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
			return server
		}, nil)
		slog.Info("serving MCP over Streamable HTTP", "addr", *httpAddr, "path", "/mcp")
		if err := http.ListenAndServe(*httpAddr, handler); err != nil {
			slog.Error("HTTP server failed", "err", err)
			os.Exit(1)
		}
		return
	}

	slog.Info("serving MCP over stdio")
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		slog.Error("stdio server failed", "err", err)
		os.Exit(1)
	}
}

// readToken returns the trimmed content of the bearer token file.
func readToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	return token, nil
}

// configureTLS makes http.DefaultClient trust hawk-apiserver's certificate.
//
// hawk generates a self-signed certificate whose subject is the node's
// hostname and which carries no subjectAltName extension, so the standard
// chain-plus-hostname validation can never succeed when connecting by IP or by
// a name the certificate does not mention. Instead of disabling verification,
// we pin: the default hostname check is skipped, and VerifyConnection requires
// the peer certificate to chain to the certificate given in caFile.
func configureTLS(caFile string, insecure bool) error {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}

	if insecure {
		tlsConfig.InsecureSkipVerify = true
		transport.TLSClientConfig = tlsConfig
		http.DefaultClient.Transport = transport
		return nil
	}

	if caFile == "" {
		// Keep the system trust store and the default hostname validation.
		return nil
	}

	pem, err := os.ReadFile(caFile)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return errors.New("no PEM certificate found in " + caFile)
	}

	tlsConfig.InsecureSkipVerify = true
	tlsConfig.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("server presented no certificate")
		}
		opts := x509.VerifyOptions{
			Roots:         pool,
			Intermediates: x509.NewCertPool(),
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		for _, intermediate := range cs.PeerCertificates[1:] {
			opts.Intermediates.AddCert(intermediate)
		}
		_, err := cs.PeerCertificates[0].Verify(opts)
		return err
	}
	transport.TLSClientConfig = tlsConfig
	http.DefaultClient.Transport = transport
	return nil
}
