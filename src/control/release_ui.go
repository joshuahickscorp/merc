package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const releaseUIHTML = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Merc Level-B release</title><style>body{font:16px system-ui;max-width:920px;margin:2rem auto;padding:0 1rem;background:#111;color:#eee}pre{white-space:pre-wrap;word-break:break-word;background:#1c1c1c;padding:1rem;border-radius:8px}strong{color:#ffb86c}</style></head><body><h1>Merc Level-B release</h1><p><strong>Loopback-only observer.</strong> This page never authorizes launch, Level C, live money, or public access.</p><pre id="release">Loading…</pre><script>fetch('/release.json',{cache:'no-store'}).then(r=>r.json()).then(x=>document.getElementById('release').textContent=JSON.stringify(x,null,2)).catch(e=>document.getElementById('release').textContent=String(e))</script></body></html>`

func releaseUIAddressAllowed(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("release UI listen address: %w", err)
	}
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("release UI must bind a loopback address only")
	}
	return nil
}

func releaseUIModel(root string) map[string]any {
	model := map[string]any{
		"schema_version": 1,
		"kind":           "merc_level_b_release_ui",
		"level_c":        "NO_GO_PROHIBITED",
		"authorization":  "OBSERVE_ONLY",
	}
	if state, err := readLaunchState(root); err == nil {
		model["release_state"] = state
	} else {
		model["release_state"] = map[string]any{"status": "UNAVAILABLE"}
	}
	if raw, err := os.ReadFile(releaseEvidencePath(root)); err == nil {
		var evidence releaseRootEvidence
		if json.Unmarshal(raw, &evidence) == nil && evidence.Kind == "merc_level_b_release_root_evidence" {
			model["root_evidence"] = evidence
		} else {
			model["root_evidence"] = map[string]any{"status": "INVALID"}
		}
	} else {
		model["root_evidence"] = map[string]any{"status": "UNAVAILABLE"}
	}
	var decision struct {
		ReadinessScore int               `json:"readiness_score"`
		Decisions      map[string]string `json:"decisions"`
		OpenP1         []struct {
			ID    string `json:"id"`
			Owner string `json:"owner"`
		} `json:"open_p1"`
	}
	if raw, err := os.ReadFile(filepath.Join(root, "ops", "go-no-go.json")); err == nil && json.Unmarshal(raw, &decision) == nil {
		model["go_no_go"] = decision
	} else {
		model["go_no_go"] = map[string]any{"status": "UNAVAILABLE"}
	}
	return model
}

func releaseUIHandler(root string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		switch r.URL.Path {
		case "/":
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(releaseUIHTML))
		case "/release.json":
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(w).Encode(releaseUIModel(root))
		default:
			http.NotFound(w, r)
		}
	})
}

func serveReleaseUI(root, address string) error {
	if err := releaseUIAddressAllowed(address); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer listener.Close()
	url := "http://" + listener.Addr().String() + "/"
	printLaunch(map[string]any{"schema_version": 1, "kind": "merc_level_b_release_ui", "status": "LISTENING", "url": url, "loopback_only": true})
	server := &http.Server{Handler: releaseUIHandler(root), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
