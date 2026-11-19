// data-box: a tiny shared to-do server for the Chrome extension.
//
//	data-box serve                 start the HTTP API
//	data-box token create NAME     add a person, print their token
//	data-box token list | revoke NAME
//	data-box ls                    print the lists and their tasks
//
// Settings come from the environment, see README.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "data-box:", err)
		os.Exit(1)
	}
}

const usage = "usage: data-box serve | ls | token create NAME | token list | token revoke NAME"

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	store, err := OpenStore(env("DATA_BOX_DIR", "./data"))
	if err != nil {
		return err
	}
	defer store.Close()

	switch args[0] {
	case "serve":
		return serve(store)
	case "ls":
		return printTasks(os.Stdout, store)
	case "token":
		if len(args) < 2 {
			return errors.New(usage)
		}
		switch {
		case args[1] == "create" && len(args) == 3:
			tok, err := store.CreateUser(args[2])
			if err != nil {
				return err
			}
			fmt.Println(tok)
			return nil
		case args[1] == "list":
			users, err := store.Users()
			for _, u := range users {
				fmt.Println(u)
			}
			return err
		case args[1] == "revoke" && len(args) == 3:
			return store.DeleteUser(args[2])
		}
	}
	return errors.New(usage)
}

func serve(store *Store) error {
	app := &App{store: store, hub: NewHub()}
	mb, err := strconv.Atoi(env("AUDIO_MAX_MB", "2048"))
	if err != nil || mb < 0 {
		return errors.New("AUDIO_MAX_MB must be a number of megabytes (0 = no limit)")
	}
	store.MaxAudioBytes = int64(mb) << 20
	// Transcription is opt-in: only TRANSCRIBE_KEY / TRANSCRIBE_URL turn it on
	// (an OPENAI_API_KEY set for something else does not).
	if key := os.Getenv("TRANSCRIBE_KEY"); key != "" || os.Getenv("TRANSCRIBE_URL") != "" {
		mins, err := strconv.Atoi(env("TRANSCRIBE_DAILY_MINUTES", "120"))
		if err != nil || mins < 0 {
			return errors.New("TRANSCRIBE_DAILY_MINUTES must be a number (0 = no limit)")
		}
		app.TranscribeDailySec = mins * 60
		app.tr = &WhisperAPI{
			URL:   env("TRANSCRIBE_URL", "https://api.openai.com/v1/audio/transcriptions"),
			Key:   key,
			Model: env("TRANSCRIBE_MODEL", "whisper-1"),
			Lang:  os.Getenv("TRANSCRIBE_LANG"),
			HTTP:  &http.Client{Timeout: 2 * time.Minute},
		}
		u, err := url.Parse(app.tr.(*WhisperAPI).URL)
		if err != nil || (u.Scheme != "https" && !isLocalHost(u.Hostname())) {
			return fmt.Errorf("TRANSCRIBE_URL must be https:// (http only for localhost/private hosts): %q", os.Getenv("TRANSCRIBE_URL"))
		}
		slog.Info("transcription on", "url", u.Redacted())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Done tasks are kept for 30 days.
	go func() {
		for {
			if n, err := store.PurgeDone(30 * 24 * time.Hour); err != nil {
				slog.Warn("purge", "err", err)
			} else if n > 0 {
				slog.Info("purged done tasks", "n", n)
			}
			sleep(ctx, time.Hour)
			if ctx.Err() != nil {
				return
			}
		}
	}()

	addr := env("DATA_BOX_ADDR", "127.0.0.1:8790")
	srv := &http.Server{Addr: addr, Handler: app.Handler(), ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 32 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx }} // ends event streams on shutdown
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sh)
	}()
	slog.Info("listening", "addr", addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// isLocalHost: loopback or private IPs, localhost, or a single-label name
// (a Docker service like "whisper"). IP literals are checked as IPs first,
// so an IPv6 address (no dots) is not mistaken for a service name.
func isLocalHost(h string) bool {
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate()
	}
	return h == "localhost" || (h != "" && !strings.ContainsAny(h, ".:"))
}

// printTasks writes every list with its open tasks (and the ones done in
// the last week) as plain text.
func printTasks(w io.Writer, store *Store) error {
	lists, err := store.Lists()
	if err != nil {
		return err
	}
	tasks, err := store.Tasks(doneWindow)
	if err != nil {
		return err
	}
	for i, l := range lists {
		if i > 0 {
			fmt.Fprintln(w)
		}
		open := 0
		for _, t := range tasks {
			if t.ListID == l.ID && t.DoneAt == 0 {
				open++
			}
		}
		fmt.Fprintf(w, "%s (%d)\n", l.Name, open)
		for _, t := range tasks {
			if t.ListID != l.ID {
				continue
			}
			mark := "[ ]"
			if t.DoneAt != 0 {
				mark = "[x]"
			}
			pri := ""
			if t.Pri {
				pri = "! "
			}
			lines := strings.Split(t.Text, "\n")
			extra := []string{t.Author}
			if t.HasAudio {
				extra = append(extra, fmt.Sprintf("🎤 %d:%02d", t.AudioSec/60, t.AudioSec%60))
			}
			fmt.Fprintf(w, "  %s %s%s  · %s\n", mark, pri, lines[0], strings.Join(extra, " · "))
			for _, more := range lines[1:] {
				fmt.Fprintf(w, "        %s\n", more)
			}
		}
	}
	return nil
}

// sleep waits d or until ctx ends.
func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
