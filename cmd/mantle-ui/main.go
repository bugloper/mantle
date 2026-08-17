// Command mantle-ui serves Mantle's web interface.
//
// It is a separate artifact on purpose (§14.3). It has its own version and
// release cadence, it holds no database connection and no privileged access,
// and no registry function — including recovery from a broken install — may
// require it. A registry upgrade never requires a UI upgrade, and vice versa:
// the /api/v1 version is the contract between them.
//
// Everything it displays comes from the same public API the CLI uses. If this
// interface can ever do something `mantle` cannot, that is a bug in the CLI.
package main

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Version is set at build time.
var Version = "dev"

//go:embed assets
var embeddedAssets embed.FS

func main() {
	var (
		listen    = flag.String("listen", "127.0.0.1:5180", "address to serve the interface on")
		registry  = flag.String("registry", envOr("MANTLE_REGISTRY", ""), "registry base URL, e.g. https://registry.example.com")
		assetsDir = flag.String("assets", "", "serve assets from this directory instead of the embedded copy (development)")
		readOnly  = flag.Bool("read-only", false,
			"refuse every request that would change state; the CLI remains the way to make changes")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Usage = usage
	flag.Parse()

	if *showVersion {
		fmt.Println("mantle-ui", Version)
		return
	}
	if *registry == "" {
		fatal("no registry configured\n" +
			"  Pass --registry https://registry.example.com, or set MANTLE_REGISTRY.")
	}

	target, err := url.Parse(normaliseURL(*registry))
	if err != nil {
		fatal("%q is not a valid registry URL: %v", *registry, err)
	}
	if target.Host == "" {
		fatal("%q is not a valid registry URL: no host", *registry)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	assets, err := resolveAssets(*assetsDir)
	if err != nil {
		fatal("%v", err)
	}

	handler := newHandler(target, assets, logger, *readOnly)

	server := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("mantle-ui listening", "address", *listen, "registry", target.String(),
			"version", Version, "read_only", *readOnly)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("listener failed", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	logger.Info("mantle-ui stopped")
}

// newHandler builds the routing tree: static assets, and a proxy to the admin
// API.
func newHandler(target *url.URL, assets fs.FS, logger *slog.Logger, readOnly bool) http.Handler {
	mux := http.NewServeMux()

	// The admin API is proxied rather than called cross-origin from the browser.
	//
	// The alternative — enabling CORS on the registry's admin API — would widen
	// the registry's own attack surface to benefit one optional client. Proxying
	// keeps everything same-origin, which also means the strict Content-Security
	// -Policy below can forbid connections to anywhere else.
	//
	// The proxy holds no credential of its own. It forwards whatever
	// Authorization header the browser sent and nothing more, so this process
	// is not a privileged component and compromising it grants nothing that
	// compromising the browser session would not.
	mux.Handle("/api/v1/", newAPIProxy(target, logger, readOnly))

	// The interface asks what it is allowed to do rather than assuming. Without
	// this it would render create and delete controls that the proxy then
	// refuses with a 405 — offering a button that cannot work is worse than not
	// offering it.
	mux.HandleFunc("/ui-config.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprintf(w, `{"read_only":%t,"version":%q,"registry":%q}`,
			readOnly, Version, target.String())
	})

	mux.Handle("/", staticHandler(assets, logger))
	return securityHeaders(mux)
}

// newAPIProxy forwards /api/v1 requests to the registry.
func newAPIProxy(target *url.URL, logger *slog.Logger, readOnly bool) http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = target.Host
			// The inbound Authorization header is preserved by SetURL; nothing
			// else from the browser needs to reach the registry.
			r.Out.Header.Del("Cookie")
			r.Out.Header.Del("Referer")
			r.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// A browser that navigates away, reloads, or is closed cancels its
			// in-flight requests, and that surfaces here as context.Canceled.
			// It is not a fault and must not be logged as one — an operator
			// scanning for errors should find real ones.
			if errors.Is(err, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled) {
				logger.Debug("client cancelled a request", "path", r.URL.Path)
				return
			}

			logger.Error("the registry could not be reached",
				"path", r.URL.Path, "registry", target.String(), "error", err)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, `{"error":{"code":"registry_unreachable",`+
				`"message":"the registry at %s could not be reached",`+
				`"remedy":"Check that mantled is running and answering: curl %s/readyz"}}`,
				target.String(), target.String())
		},
		Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
			// Admin API calls are small and fast. A long ceiling here does not
			// make a hung registry work — it only delays saying so, which turned
			// a dead daemon into a minute of an apparently frozen page.
			ResponseHeaderTimeout: 15 * time.Second,
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// --read-only restores the posture §14.3 recommends: the interface
		// becomes a dashboard and every change goes through the CLI. It is a
		// deployment choice rather than a property of the code, because an
		// operator who wants a browser-visible registry without a browser-
		// writable one should not have to run a different build to get it.
		//
		// Note there is no CSRF token anywhere here, and none is needed: the
		// credential travels in an Authorization header the page sets from
		// sessionStorage, not in a cookie, so a cross-site page cannot make the
		// browser attach it. Adding cookie auth later would change that.
		if readOnly && r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			fmt.Fprintf(w, `{"error":{"code":"read_only",`+
				`"message":"this mantle-ui is running in read-only mode",`+
				`"remedy":"Use the mantle CLI, or restart mantle-ui without --read-only."}}`)
			return
		}

		// Everything else is the registry's decision, not ours. The proxy does
		// not decide who may create or delete anything — it forwards the
		// caller's credential and lets the admin API authorise it, so the two
		// surfaces cannot disagree about permissions.
		proxy.ServeHTTP(w, r)
	})
}

// staticHandler serves the interface's assets.
//
// It reads and serves each file directly rather than delegating to
// http.FileServer. FileServer canonicalises "/index.html" to "/" with a 301,
// which turns serving the root into a redirect loop — and the assets are a few
// kilobytes, so reading them per request costs nothing worth avoiding.
func staticHandler(assets fs.FS, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		// Routing happens in the URL fragment, so every real path is a file.
		// Anything else is a 404 rather than a silent fallback to index.html: a
		// mistyped asset path should say so, not return HTML that then fails to
		// parse as JavaScript.
		if !fs.ValidPath(path) {
			http.NotFound(w, r)
			return
		}

		content, err := fs.ReadFile(assets, path)
		if err != nil {
			http.NotFound(w, r)
			return
		}

		if contentType := mime.TypeByExtension(filepath.Ext(path)); contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		// Assets are versioned with the binary, so a short cache is safe while a
		// long one would serve stale JavaScript after an upgrade.
		if path == "index.html" {
			w.Header().Set("Cache-Control", "no-cache")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=300")
		}
		http.ServeContent(w, r, path, time.Time{}, bytes.NewReader(content))
	})
}

// securityHeaders applies a strict policy to every response.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Everything is same-origin: assets are local and the API is proxied.
		// That lets the policy forbid inline script and any external
		// connection, which is the strongest form this can take and the reason
		// the interface uses no inline handlers or CDN.
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self'; "+
				"img-src 'self' data:; connect-src 'self'; "+
				"frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// resolveAssets returns the embedded assets, or a directory for development.
func resolveAssets(dir string) (fs.FS, error) {
	if dir != "" {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("--assets %s is not a readable directory", dir)
		}
		return os.DirFS(dir), nil
	}
	sub, err := fs.Sub(embeddedAssets, "assets")
	if err != nil {
		return nil, fmt.Errorf("reading embedded assets: %w", err)
	}
	return sub, nil
}

func normaliseURL(raw string) string {
	if !strings.Contains(raw, "://") {
		return "https://" + raw
	}
	return strings.TrimSuffix(raw, "/")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func usage() {
	fmt.Fprintf(os.Stderr, `mantle-ui — the Mantle web interface

Usage:
  mantle-ui --registry https://registry.example.com [flags]

Flags:
`)
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, `
mantle-ui is optional and read-only. It is an ordinary client of the registry's
public API: it holds no database connection and no privileged access, and the
registry runs perfectly well without it. Use the 'mantle' CLI for anything that
changes state.

Examples:
  mantle-ui --registry http://127.0.0.1:5100
  mantle-ui --registry https://registry.example.com --listen 0.0.0.0:8080
`)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "mantle-ui: "+format+"\n", args...)
	os.Exit(1)
}
