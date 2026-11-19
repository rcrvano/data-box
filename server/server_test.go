package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newApp(t *testing.T) (*App, string, *httptest.Server) {
	t.Helper()
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tok, err := st.CreateUser("Иван")
	if err != nil {
		t.Fatal(err)
	}
	app := &App{store: st, hub: NewHub()}
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)
	return app, tok, srv
}

func do(t *testing.T, srv *httptest.Server, tok, method, path string, body any, out any) int {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, srv.URL+path, r)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func TestAuth(t *testing.T) {
	_, tok, srv := newApp(t)
	if c := do(t, srv, "", "GET", "/api/tasks", nil, nil); c != 401 {
		t.Fatalf("no token: %d", c)
	}
	if c := do(t, srv, "db_wrong", "GET", "/api/tasks", nil, nil); c != 401 {
		t.Fatalf("bad token: %d", c)
	}
	var me struct{ Name string }
	if c := do(t, srv, tok, "GET", "/api/me", nil, &me); c != 200 || me.Name != "Иван" {
		t.Fatalf("me: %d %+v", c, me)
	}
	// The token is never taken from the URL (logs, caches, shared links).
	resp, _ := http.Get(srv.URL + "/api/me?token=" + tok)
	if resp.StatusCode != 401 {
		t.Fatalf("query token must not work: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/api/me", nil)
	req.Header.Set("Authorization", "bearer "+tok)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 200 {
		t.Fatalf("lower-case bearer: %d", resp.StatusCode)
	}
}

func TestValidation(t *testing.T) {
	_, tok, srv := newApp(t)
	cases := []struct {
		method, path string
		body         any
		code         int
	}{
		{"POST", "/api/tasks", map[string]any{"text": strings.Repeat("я", maxTaskText+1)}, 400},
		{"POST", "/api/tasks", map[string]any{"text": strings.Repeat("я", maxTaskText)}, 201},
		{"POST", "/api/lists", map[string]any{"name": strings.Repeat("x", maxListName+1)}, 400},
		{"POST", "/api/lists", map[string]any{"name": "x", "color": "red;background:url(x)"}, 400},
		{"POST", "/api/lists", map[string]any{"name": "x", "color": "#A1b2C3"}, 201},
		{"PATCH", "/api/lists/1", map[string]any{"color": "#fff"}, 400},
		{"POST", "/api/tasks", map[string]any{"text": strings.Repeat("x", maxJSONBody)}, 400}, // body over the JSON limit
	}
	for _, c := range cases {
		if got := do(t, srv, tok, c.method, c.path, c.body, nil); got != c.code {
			t.Errorf("%s %s: got %d, want %d", c.method, c.path, got, c.code)
		}
	}
}

func TestTaskLifecycle(t *testing.T) {
	_, tok, srv := newApp(t)
	var lists []List
	do(t, srv, tok, "GET", "/api/lists", nil, &lists)
	if len(lists) != 1 || lists[0].Name != "Общие" {
		t.Fatalf("default list: %+v", lists)
	}
	var home List
	if c := do(t, srv, tok, "POST", "/api/lists", map[string]string{"name": "Дом"}, &home); c != 201 || home.Color == "" {
		t.Fatalf("create list: %d %+v", c, home)
	}

	var a, b, c Task
	do(t, srv, tok, "POST", "/api/tasks", map[string]any{"text": "  купить молоко "}, &a)
	do(t, srv, tok, "POST", "/api/tasks", map[string]any{"text": "позвонить", "list_id": home.ID}, &b)
	do(t, srv, tok, "POST", "/api/tasks", map[string]any{"text": "срочно", "pri": true}, &c)
	if a.Text != "купить молоко" || a.ListID != lists[0].ID || a.Author != "Иван" || a.Source != "extension" {
		t.Fatalf("task a: %+v", a)
	}
	if code := do(t, srv, tok, "POST", "/api/tasks", map[string]any{"text": "  "}, nil); code != 400 {
		t.Fatalf("empty task: %d", code)
	}
	if code := do(t, srv, tok, "POST", "/api/tasks", map[string]any{"text": "x", "list_id": 999}, nil); code != 404 {
		t.Fatalf("bad list: %d", code)
	}

	var all []Task
	do(t, srv, tok, "GET", "/api/tasks", nil, &all)
	if got := ids(all); !reflect.DeepEqual(got, []int64{c.ID, a.ID, b.ID}) {
		t.Fatalf("order: %v", got)
	}

	var done Task
	do(t, srv, tok, "PATCH", "/api/tasks/"+itoa(a.ID), map[string]any{"done": true}, &done)
	if done.DoneAt == 0 {
		t.Fatal("not done")
	}
	do(t, srv, tok, "GET", "/api/tasks", nil, &all)
	if got := ids(all); !reflect.DeepEqual(got, []int64{c.ID, b.ID, a.ID}) {
		t.Fatalf("done goes last: %v", got)
	}
	var moved Task
	do(t, srv, tok, "PATCH", "/api/tasks/"+itoa(a.ID), map[string]any{"done": false, "list_id": home.ID, "text": "молоко"}, &moved)
	if moved.DoneAt != 0 || moved.ListID != home.ID || moved.Text != "молоко" {
		t.Fatalf("patch: %+v", moved)
	}

	if code := do(t, srv, tok, "DELETE", "/api/tasks/"+itoa(b.ID), nil, nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if code := do(t, srv, tok, "DELETE", "/api/lists/"+itoa(home.ID), nil, nil); code != 204 {
		t.Fatalf("delete list: %d", code)
	}
	do(t, srv, tok, "GET", "/api/tasks", nil, &all)
	if got := ids(all); !reflect.DeepEqual(got, []int64{c.ID}) {
		t.Fatalf("after deletes: %v", got)
	}
	if code := do(t, srv, tok, "DELETE", "/api/lists/"+itoa(lists[0].ID), nil, nil); code != 400 {
		t.Fatalf("last list must stay: %d", code)
	}
}

type fakeTr struct{ text string }

func (f fakeTr) Transcribe(context.Context, []byte, string) (string, error) { return f.text, nil }

func TestVoiceTask(t *testing.T) {
	app, tok, srv := newApp(t)
	app.tr = fakeTr{"распознанный текст"}
	audio := []byte("OggS fake opus data")

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("audio_sec", "75")
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{`form-data; name="audio"; filename="v.webm"`}
	h["Content-Type"] = []string{"audio/webm"}
	p, _ := mw.CreatePart(h)
	p.Write(audio)
	mw.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/api/tasks", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var task Task
	json.NewDecoder(resp.Body).Decode(&task)
	resp.Body.Close()
	if resp.StatusCode != 201 || !task.HasAudio || task.AudioSec != 75 || task.Text != "🎤 Голосовая заметка 1:15" {
		t.Fatalf("voice task: %d %+v", resp.StatusCode, task)
	}

	// The transcript replaces the placeholder in the background.
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, _ := app.store.GetTask(task.ID)
		if got.Text == "распознанный текст" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not transcribed: %q", got.Text)
		}
		time.Sleep(20 * time.Millisecond)
	}

	data, hdr := getAudio(t, srv, tok, task.ID)
	if !bytes.Equal(data, audio) || hdr.Get("Content-Type") != "audio/webm" {
		t.Fatalf("audio: %q %s", data, hdr.Get("Content-Type"))
	}
	if hdr.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(hdr.Get("Content-Security-Policy"), "sandbox") ||
		hdr.Get("Content-Disposition") != "attachment" {
		t.Fatalf("audio headers: %v", hdr)
	}
	path, _, _ := app.store.AudioPath(task)
	do(t, srv, tok, "DELETE", "/api/tasks/"+itoa(task.ID), nil, nil)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("recording not removed with the task")
	}
}

func getAudio(t *testing.T, srv *httptest.Server, tok string, id int64) ([]byte, http.Header) {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+"/api/tasks/"+itoa(id)+"/audio", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return data, resp.Header
}

func uploadAudio(t *testing.T, srv *httptest.Server, tok, mime string, audio []byte) Task {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{`form-data; name="audio"; filename="x"`}
	if mime != "" {
		h["Content-Type"] = []string{mime}
	}
	p, _ := mw.CreatePart(h)
	p.Write(audio)
	mw.WriteField("text", "t")
	mw.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/api/tasks", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var task Task
	json.NewDecoder(resp.Body).Decode(&task)
	return task
}

// An uploaded "recording" must never come back as something a browser
// would render (stored XSS on the server's domain).
func TestAudioCannotBeHTML(t *testing.T) {
	_, tok, srv := newApp(t)
	page := []byte("<html><script>alert(1)</script></html>")
	for _, mime := range []string{"text/html", "image/svg+xml", "", "AUDIO/OGG; codecs=opus"} {
		task := uploadAudio(t, srv, tok, mime, page)
		_, hdr := getAudio(t, srv, tok, task.ID)
		ct := hdr.Get("Content-Type")
		want := "application/octet-stream"
		if strings.HasPrefix(strings.ToLower(mime), "audio/ogg;") {
			want = "audio/ogg"
		}
		if ct != want || hdr.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("upload as %q served as %q", mime, ct)
		}
	}
}

func TestEvents(t *testing.T) {
	_, tok, srv := newApp(t)
	req, _ := http.NewRequest("GET", srv.URL+"/api/events", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 10)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if l := sc.Text(); strings.HasPrefix(l, "data: ") {
				lines <- l
			}
		}
	}()
	time.Sleep(50 * time.Millisecond) // let the subscription register
	var task Task
	do(t, srv, tok, "POST", "/api/tasks", map[string]any{"text": "x"}, &task)
	select {
	case l := <-lines:
		if l != `data: {"id":`+itoa(task.ID)+`,"kind":"task"}` {
			t.Fatalf("event: %s", l)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
	}
}

func TestPurgeDone(t *testing.T) {
	app, _, _ := newApp(t)
	tk, _ := app.store.AddTask(NewTask{ListID: app.defaultList(), Text: "old"})
	app.store.db.Exec(`UPDATE tasks SET done_at=? WHERE id=?`, time.Now().Add(-40*24*time.Hour).Unix(), tk.ID)
	open, _ := app.store.AddTask(NewTask{ListID: app.defaultList(), Text: "open"})
	n, err := app.store.PurgeDone(30 * 24 * time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if _, err := app.store.GetTask(open.ID); err != nil {
		t.Fatal("open task purged")
	}
}

func ids(ts []Task) []int64 {
	out := []int64{}
	for _, t := range ts {
		out = append(out, t.ID)
	}
	return out
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestPrintTasks(t *testing.T) {
	app, _, _ := newApp(t)
	home, _ := app.store.CreateList("Home", "")
	app.store.AddTask(NewTask{ListID: 1, Text: "отчёт\nhttps://x.y", Author: "Иван", Pri: true})
	app.store.AddTask(NewTask{ListID: home.ID, Text: "хлеб", Author: "Маша", Source: "extension", Audio: []byte("x"), AudioMime: "audio/ogg", AudioSec: 7})
	var b bytes.Buffer
	if err := printTasks(&b, app.store); err != nil {
		t.Fatal(err)
	}
	want := "Общие (1)\n  [ ] ! отчёт  · Иван\n        https://x.y\n\nHome (1)\n  [ ] хлеб  · Маша · 🎤 0:07\n"
	if b.String() != want {
		t.Fatalf("got:\n%s", b.String())
	}
}
