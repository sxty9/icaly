// Command icalyd is the holistic calendar service daemon. It exposes an HTTP surface under
// /api/services/icaly/, validates the shared holistic session (a signed JWT in the h_access
// cookie) without any RPC to the holistic backend, and enforces the holistic rights standard.
// Calendars are stored as per-event .ics files (single source of truth) indexed by an embedded
// SQLite database. It runs unprivileged behind the holistic Caddy proxy.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"icaly/internal/api"
	"icaly/internal/apppass"
	"icaly/internal/auth"
	"icaly/internal/geocode"
	"icaly/internal/imip"
	"icaly/internal/instance"
	"icaly/internal/push"
	"icaly/internal/scheduling"
	"icaly/internal/store"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8776", "address to listen on")
	flag.Parse()

	secret, err := auth.LoadSecret()
	if err != nil {
		log.Fatalf("icalyd: %v", err)
	}
	// Admin = membership in this group (the single Linux source of truth). The systemd unit
	// sets ICALY_ADMIN_GROUP; the verifier defaults to "sudo" when it is empty.
	v := auth.NewVerifier(secret, os.Getenv("ICALY_ADMIN_GROUP"))

	dataRoot := getenv("ICALY_DATA", "/var/lib/icaly")
	st, err := store.Open(dataRoot)
	if err != nil {
		log.Fatalf("icalyd: open store: %v", err)
	}
	defer st.Close()
	inst := instance.New()
	hub := push.New(st) // subscribes to the store's change stream; drives in-app SSE
	// App passwords authenticate native CalDAV clients over HTTP Basic (the session cookie only
	// exists in the browser); stored as SHA-256 hashes under the data root.
	ap := apppass.New(filepath.Join(dataRoot, "apppasswords"))

	// Calendar invitations (Phase 1b). The shared icaly↔maild secret authenticates both the
	// outbound internal-send and the inbound iMIP webhook. All optional: with no secret/URL,
	// internal scheduling still works and external iMIP is simply skipped.
	mailSecret := readSecret("ICALY_MAIL_SECRET", "ICALY_MAIL_SECRET_FILE")
	mailer := imip.New(getenv("ICALY_MAILD_URL", "http://127.0.0.1:8775"), mailSecret)
	sched := scheduling.New(st, inst, mailer)

	// Location picker: provider-agnostic geocoding proxy. With no key it uses Photon (free, no
	// key); drop a Google Places key into ICALY_GEOCODE_KEY_FILE and it switches automatically.
	// ICALY_GEOCODE_DAILY_CAP is an instance-wide hard ceiling on upstream calls/day (cost guard
	// on a shared billed key, and courtesy to the free Photon instance); 0 disables it.
	geoCap := 5000
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("ICALY_GEOCODE_DAILY_CAP"))); err == nil {
		geoCap = n
	}
	geo := geocode.New(getenv("ICALY_GEOCODE_PROVIDER", ""), readSecret("ICALY_GEOCODE_KEY", "ICALY_GEOCODE_KEY_FILE"), geoCap)

	srv := &http.Server{
		Handler:           api.New(v, st, inst, hub, sched, ap, geo, mailSecret).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Bind synchronously so an "address in use" surfaces here, not in a goroutine.
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("icalyd: listen %s: %v", *listen, err)
	}
	go func() {
		log.Printf("icalyd listening on %s (data=%s, mailDomain=%q)", *listen, dataRoot, inst.MailDomain())
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatalf("icalyd: %v", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Periodic change-log compaction advances each calendar's sync-token floor so stale
	// WebDAV-Sync tokens are eventually rejected with 409 + full resync (plan M2). 90-day
	// retention per the plan; tombstones older than that are dropped.
	go compactionLoop(ctx, st)

	<-ctx.Done()

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	log.Print("icalyd stopped")
}

// compactionLoop trims the change-log once a day, keeping a 90-day tombstone floor (plan M2).
func compactionLoop(ctx context.Context, st *store.Store) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := st.Compact(time.Now().AddDate(0, 0, -90)); err != nil {
				log.Printf("icalyd: changelog compaction: %v", err)
			}
		}
	}
}

func getenv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// readSecret returns a secret from the env var, else from the file named by fileEnv.
func readSecret(env, fileEnv string) string {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return v
	}
	if path := strings.TrimSpace(os.Getenv(fileEnv)); path != "" {
		if b, err := os.ReadFile(path); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}
