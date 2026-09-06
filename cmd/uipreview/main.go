// Command uipreview serves the six UI pages from fixtures so a human can
// eyeball the result. It is a DEVELOPMENT-ONLY binary: there is no auth, no
// persistence, no service. Run it with `go run ./cmd/uipreview` and open
// http://127.0.0.1:8099/.
//
// Why a binary and not just `go test` snapshots: the brief calls the launch
// GIF the money shot, and that GIF is filmed against this server. It also
// catches regressions in the CSS / template stack that golden tests would
// miss.
//
// The bind address is hard-coded to 127.0.0.1:8099 — the brief's spec. There
// is no flag, no env, no override: this server is meant for a developer's
// own machine, never a LAN.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/templates"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/view"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("uipreview: %v", err)
	}
}

func run() error {
	// Templates can be loaded either from the on-disk source (so a developer
	// can edit a .html and refresh the browser without rebuilding) or from
	// the embed.FS compiled into the binary (default, for installed
	// binaries). We prefer the on-disk source if it exists.
	templatesDir := templates.MirroredDir
	if _, err := os.Stat(templatesDir); err != nil {
		// Fall back to embedded.
		eng, err := templates.New()
		if err != nil {
			return fmt.Errorf("load embedded templates: %w", err)
		}
		return serve(eng)
	}
	eng, err := templates.NewFromDir(templatesDir)
	if err != nil {
		return fmt.Errorf("load on-disk templates: %w", err)
	}
	log.Printf("uipreview: serving templates from %s (edit and refresh)", templatesDir)
	return serve(eng)
}

func serve(eng *templates.Engine) error {
	mux := http.NewServeMux()

	// Static files — served from disk if the directory exists so a developer
	// can iterate on app.css and app.js, otherwise from the embed.FS bundled
	// in the binary.
	staticDir := filepath.Join("web", "static")
	if _, err := os.Stat(staticDir); err == nil {
		mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir(staticDir))))
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/index.html":
			page := view.Page{
				Title:       "Overview",
				Nav:         "overview",
				CurrentUser: "alex",
				BaseURL:     "http://127.0.0.1:8099",
				Model:       view.SampleOverviewModel(),
			}
			mustRender(w, eng, "page-overview", page)
		case "/p/BMB":
			model, _, _ := view.SampleBoardModel()
			page := view.Page{
				Title: "BMB · BeeMemoryBank", Subtitle: "BeeMemoryBank",
				Nav: "board", CurrentUser: "alex", BaseURL: "http://127.0.0.1:8099",
				Model: model,
			}
			mustRender(w, eng, "page-board", page)
		case "/p/BMB/activity":
			page := view.Page{
				Title: "Activity · BMB",
				Nav:   "activity", CurrentUser: "alex", BaseURL: "http://127.0.0.1:8099",
				Model: view.SampleActivityModel(),
			}
			mustRender(w, eng, "page-activity", page)
		case "/t/BMB-14":
			page := view.Page{
				Title: "BMB-14 · Fix WAL checkpoint race",
				Nav:   "task", CurrentUser: "alex", BaseURL: "http://127.0.0.1:8099",
				Model: view.SampleDrawerModel(),
			}
			mustRender(w, eng, "page-drawer", page)
		case "/login":
			page := view.Page{
				Title: "Sign in", Nav: "login", BaseURL: "http://127.0.0.1:8099",
				Model: view.SampleLoginModel("", ""),
			}
			mustRender(w, eng, "page-login", page)
		case "/firstrun":
			page := view.Page{
				Title: "Welcome", Nav: "login", BaseURL: "http://127.0.0.1:8099",
				Model: view.SampleFirstRunModel("kbn_demo_0123456789abcdef0123456789abcdef"),
			}
			mustRender(w, eng, "page-firstrun", page)
		case "/agent-setup":
			page := view.Page{
				Title: "Agent setup", Nav: "agent-setup", BaseURL: "http://127.0.0.1:8099",
				Model: view.SampleAgentSetup("http://127.0.0.1:8099", "admin"),
			}
			mustRender(w, eng, "page-agent-setup", page)
		case "/admin":
			page := view.Page{
				Title: "Admin", Nav: "admin", CurrentUser: "admin", BaseURL: "http://127.0.0.1:8099",
				Model: view.SampleAdminModel(),
			}
			mustRender(w, eng, "page-admin", page)
		default:
			http.NotFound(w, r)
		}
	})

	addr := "127.0.0.1:8099"
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("uipreview: http://%s/  (pages: /  /p/BMB  /p/BMB/activity  /t/BMB-14  /login  /firstrun  /agent-setup  /admin)", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("uipreview: server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("uipreview: shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutCtx)
}

func mustRender(w http.ResponseWriter, eng *templates.Engine, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := eng.Render(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
