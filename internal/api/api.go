// Package api serves cometduty's silence API on the prometheus listener. It
// speaks the Alertmanager v2 silence API, so amtool, Grafana and scripts
// written for Alertmanager work against cometduty unchanged:
//
//	POST   /api/v2/silences       create (or replace, with "id") → {"silenceID": …}
//	GET    /api/v2/silences       list
//	GET    /api/v2/silence/{id}   one
//	DELETE /api/v2/silence/{id}   expire now
//
// Every request needs "Authorization: Bearer <api_token>". With no token
// configured nothing is served.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/abhijitkrm/cometduty/internal/alert"
)

// Engine is the part of the alert engine the API drives.
type Engine interface {
	AddSilence(alert.Silence) (string, error)
	ExpireSilence(ctx context.Context, id string) error
	Silences() []alert.Silence
}

// Register mounts the API on mux when token is set.
func Register(mux *http.ServeMux, token string, eng Engine) {
	if token == "" {
		return
	}
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				httpError(w, http.StatusUnauthorized, "missing or wrong bearer token")
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("POST /api/v2/silences", auth(func(w http.ResponseWriter, r *http.Request) {
		var s alert.Silence
		dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
		if err := dec.Decode(&s); err != nil {
			httpError(w, http.StatusBadRequest, "bad silence: "+err.Error())
			return
		}
		id, err := eng.AddSilence(s)
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, map[string]string{"silenceID": id})
	}))
	mux.HandleFunc("GET /api/v2/silences", auth(func(w http.ResponseWriter, _ *http.Request) {
		now := time.Now()
		out := []gettable{}
		for _, s := range eng.Silences() {
			out = append(out, view(s, now))
		}
		writeJSON(w, out)
	}))
	mux.HandleFunc("GET /api/v2/silence/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		for _, s := range eng.Silences() {
			if s.ID == id {
				writeJSON(w, view(s, time.Now()))
				return
			}
		}
		httpError(w, http.StatusNotFound, "no silence "+id)
	}))
	mux.HandleFunc("DELETE /api/v2/silence/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		if err := eng.ExpireSilence(r.Context(), r.PathValue("id")); err != nil {
			httpError(w, http.StatusNotFound, err.Error())
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
}

// gettable is Alertmanager's GettableSilence.
type gettable struct {
	alert.Silence
	Status struct {
		State string `json:"state"`
	} `json:"status"`
}

func view(s alert.Silence, now time.Time) gettable {
	g := gettable{Silence: s}
	g.Status.State = s.State(now)
	return g
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(msg + "\n"))
}
