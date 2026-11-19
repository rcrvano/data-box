package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	doneWindow   = 7 * 24 * time.Hour // done tasks stay visible this long
	maxAudioSize = 25 << 20
	maxJSONBody  = 64 << 10
	maxSubs      = 200 // open event streams
	// At most this many transcriptions run at once; a few more may wait,
	// the rest keep their placeholder text.
	transcribeWorkers = 2
	transcribeQueue   = 8
)

// Hub fans change events out to /api/events subscribers.
type Hub struct {
	mu     sync.Mutex
	subs   map[chan string]struct{}
	byUser map[string]int
}

const maxSubsPerUser = 10

func NewHub() *Hub { return &Hub{subs: map[chan string]struct{}{}, byUser: map[string]int{}} }

// Subscribe returns nil when there are too many subscribers in total or
// for this user (so one token cannot take every slot).
func (h *Hub) Subscribe(user string) (chan string, func()) {
	ch := make(chan string, 16)
	h.mu.Lock()
	if len(h.subs) >= maxSubs || h.byUser[user] >= maxSubsPerUser {
		h.mu.Unlock()
		return nil, nil
	}
	h.subs[ch] = struct{}{}
	h.byUser[user]++
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		if h.byUser[user]--; h.byUser[user] <= 0 {
			delete(h.byUser, user)
		}
		h.mu.Unlock()
	}
}

// Publish sends {"kind":kind,"id":id}; slow subscribers miss events (they
// refetch everything on the next one anyway).
func (h *Hub) Publish(kind string, id int64) {
	b, _ := json.Marshal(map[string]any{"kind": kind, "id": id})
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- string(b):
		default:
		}
	}
}

type App struct {
	store *Store
	hub   *Hub
	tr    Transcriber // nil = no server-side transcription

	trOnce  sync.Once
	trSlots chan struct{} // running + waiting transcriptions
	trRun   chan struct{} // running transcriptions

	TranscribeDailySec int // audio seconds per day sent for transcription; 0 = no limit
	trMu               sync.Mutex
	trDay              string
	trUsed             int
}

type ctxKey struct{}

func userOf(r *http.Request) string { s, _ := r.Context().Value(ctxKey{}).(string); return s }

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	mux.Handle("GET /api/me", a.auth(a.me))
	mux.Handle("GET /api/lists", a.auth(a.getLists))
	mux.Handle("POST /api/lists", a.auth(a.createList))
	mux.Handle("PATCH /api/lists/{id}", a.auth(a.patchList))
	mux.Handle("DELETE /api/lists/{id}", a.auth(a.deleteList))
	mux.Handle("GET /api/tasks", a.auth(a.getTasks))
	mux.Handle("POST /api/tasks", a.auth(a.createTask))
	mux.Handle("PATCH /api/tasks/{id}", a.auth(a.patchTask))
	mux.Handle("DELETE /api/tasks/{id}", a.auth(a.deleteTask))
	mux.Handle("GET /api/tasks/{id}/audio", a.auth(a.getAudio))
	mux.Handle("GET /api/events", a.auth(a.events))
	return mux
}

// auth takes the token only from "Authorization: Bearer …" — never from the
// URL, where it would end up in logs, caches and copied links.
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func (a *App) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Bodies must arrive in time (uploads extend this, event streams
		// clear it); the server has no global ReadTimeout because of them.
		http.NewResponseController(w).SetReadDeadline(time.Now().Add(30 * time.Second))
		name, err := a.store.UserByToken(bearer(r))
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "bad token")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, name)))
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// fail maps store errors to HTTP codes; unexpected errors are logged and
// not shown to the client.
func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrInvalid):
		writeErr(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), "invalid: "))
	default:
		slog.Error("request failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}

// decode reads a small JSON body.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return false
	}
	return true
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil
}

func (a *App) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"name": userOf(r), "transcribe": a.tr != nil})
}

func (a *App) getLists(w http.ResponseWriter, _ *http.Request) {
	l, err := a.store.Lists()
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (a *App) createList(w http.ResponseWriter, r *http.Request) {
	var in struct{ Name, Color string }
	if !decode(w, r, &in) {
		return
	}
	l, err := a.store.CreateList(in.Name, in.Color)
	if err != nil {
		fail(w, err)
		return
	}
	a.hub.Publish("list", l.ID)
	writeJSON(w, http.StatusCreated, l)
}

func (a *App) patchList(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var in struct{ Name, Color *string }
	if !decode(w, r, &in) {
		return
	}
	l, err := a.store.UpdateList(id, in.Name, in.Color)
	if err != nil {
		fail(w, err)
		return
	}
	a.hub.Publish("list", id)
	writeJSON(w, http.StatusOK, l)
}

func (a *App) deleteList(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := a.store.DeleteList(id); err != nil {
		fail(w, err)
		return
	}
	a.hub.Publish("list", id)
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) getTasks(w http.ResponseWriter, _ *http.Request) {
	t, err := a.store.Tasks(doneWindow)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// createTask takes JSON {text, list_id, pri} or multipart with the same
// fields plus "audio" (a recording) and "audio_sec". A recording without
// text gets a placeholder, replaced by a transcript when the server can
// transcribe.
func (a *App) createTask(w http.ResponseWriter, r *http.Request) {
	n := NewTask{Author: userOf(r), Source: "extension"}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		// Slow uploads get a few minutes; the server has no global ReadTimeout
		// because event streams stay open.
		http.NewResponseController(w).SetReadDeadline(time.Now().Add(3 * time.Minute))
		r.Body = http.MaxBytesReader(w, r.Body, maxAudioSize+1<<20)
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			writeErr(w, http.StatusBadRequest, "bad form")
			return
		}
		n.Text = r.FormValue("text")
		n.ListID, _ = strconv.ParseInt(r.FormValue("list_id"), 10, 64)
		n.Pri = r.FormValue("pri") == "1" || r.FormValue("pri") == "true"
		n.AudioSec, _ = strconv.Atoi(r.FormValue("audio_sec"))
		if f, h, err := r.FormFile("audio"); err == nil {
			n.Audio, err = io.ReadAll(io.LimitReader(f, maxAudioSize+1))
			f.Close()
			if err != nil || len(n.Audio) > maxAudioSize {
				writeErr(w, http.StatusRequestEntityTooLarge, "recording too large")
				return
			}
			n.AudioMime = AudioType(h.Header.Get("Content-Type"))
		}
	} else {
		var in struct {
			Text   string `json:"text"`
			ListID int64  `json:"list_id"`
			Pri    bool   `json:"pri"`
		}
		if !decode(w, r, &in) {
			return
		}
		n.Text, n.ListID, n.Pri = in.Text, in.ListID, in.Pri
	}
	if n.ListID == 0 {
		n.ListID = a.defaultList()
	}
	needText := len(n.Audio) > 0 && strings.TrimSpace(n.Text) == ""
	if needText {
		n.Text = voicePlaceholder(n.AudioSec)
	}
	t, err := a.store.AddTask(n)
	if err != nil {
		fail(w, err)
		return
	}
	a.hub.Publish("task", t.ID)
	if needText {
		a.transcribeLater(t.ID, n.Text, n.Audio, n.AudioMime, n.AudioSec)
	}
	writeJSON(w, http.StatusCreated, t)
}

func (a *App) defaultList() int64 {
	l, err := a.store.Lists()
	if err != nil || len(l) == 0 {
		return 0
	}
	return l[0].ID
}

func voicePlaceholder(sec int) string {
	if sec > 0 {
		return fmt.Sprintf("🎤 Голосовая заметка %d:%02d", sec/60, sec%60)
	}
	return "🎤 Голосовая заметка"
}

// transcribeLater replaces the voice task's placeholder with a transcript
// in the background (see Store.SetTranscript). Limits: a few at a time and
// TranscribeDailySec of audio per day — the provider bills by duration.
func (a *App) transcribeLater(id int64, placeholder string, audio []byte, mime string, sec int) {
	if a.tr == nil {
		return
	}
	if sec <= 0 {
		sec = len(audio)/2000 + 1 // ~16 kbit/s Opus
	}
	if !a.spendTranscribe(sec) {
		slog.Warn("transcribe: daily limit reached, keeping placeholder", "task", id)
		return
	}
	a.trOnce.Do(func() {
		a.trSlots = make(chan struct{}, transcribeWorkers+transcribeQueue)
		a.trRun = make(chan struct{}, transcribeWorkers)
	})
	select {
	case a.trSlots <- struct{}{}:
	default:
		slog.Warn("transcribe: queue full, keeping placeholder", "task", id)
		return
	}
	go func() {
		defer func() { <-a.trSlots }()
		a.trRun <- struct{}{}
		defer func() { <-a.trRun }()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		text, err := a.tr.Transcribe(ctx, audio, AudioType(mime))
		if err != nil {
			slog.Warn("transcribe", "task", id, "err", err)
			return
		}
		if text = strings.TrimSpace(text); text == "" {
			return
		}
		ok, err := a.store.SetTranscript(id, placeholder, text)
		if err != nil {
			slog.Warn("transcribe: save", "task", id, "err", err)
		}
		if ok {
			a.hub.Publish("task", id)
		}
	}()
}

// spendTranscribe takes sec seconds from today's budget.
func (a *App) spendTranscribe(sec int) bool {
	if a.TranscribeDailySec <= 0 {
		return true
	}
	a.trMu.Lock()
	defer a.trMu.Unlock()
	if day := time.Now().Format("2006-01-02"); day != a.trDay {
		a.trDay, a.trUsed = day, 0
	}
	if a.trUsed+sec > a.TranscribeDailySec {
		return false
	}
	a.trUsed += sec
	return true
}

func (a *App) patchTask(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var p TaskPatch
	if !decode(w, r, &p) {
		return
	}
	t, err := a.store.UpdateTask(id, p)
	if err != nil {
		fail(w, err)
		return
	}
	a.hub.Publish("task", id)
	writeJSON(w, http.StatusOK, t)
}

func (a *App) deleteTask(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := a.store.DeleteTask(id); err != nil {
		fail(w, err)
		return
	}
	a.hub.Publish("task", id)
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) getAudio(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	t, err := a.store.GetTask(id)
	if err != nil {
		fail(w, err)
		return
	}
	path, mime, ok := a.store.AudioPath(t)
	if !ok {
		writeErr(w, http.StatusNotFound, "no recording")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no recording")
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	// The type comes from an allow-list of audio formats; with nosniff, a
	// sandbox CSP and "attachment" nothing uploaded can run as a page here.
	hd := w.Header()
	hd.Set("Content-Type", mime)
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	hd.Set("Content-Disposition", "attachment")
	hd.Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, "", st.ModTime(), f) // supports Range (seeking)
}

// events is a Server-Sent Events stream of {"kind","id"} change notices.
func (a *App) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "no streaming")
		return
	}
	http.NewResponseController(w).SetReadDeadline(time.Time{}) // a long-lived stream
	ch, cancel := a.hub.Subscribe(userOf(r))
	if ch == nil {
		writeErr(w, http.StatusServiceUnavailable, "too many connections")
		return
	}
	defer cancel()
	tok := bearer(r)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	io.WriteString(w, "retry: 3000\n\n")
	fl.Flush()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", ev)
			fl.Flush()
		case <-ping.C:
			if _, err := a.store.UserByToken(tok); err != nil {
				return // token revoked
			}
			io.WriteString(w, ": ping\n\n")
			fl.Flush()
		}
	}
}
