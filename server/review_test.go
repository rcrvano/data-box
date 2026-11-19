package main

import (
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Deleted ids are never reused: a late transcript must not land in a
// different voice note.
func TestIDsNotReused(t *testing.T) {
	app, _, _ := newApp(t)
	l, _ := app.store.CreateList("Дом", "")
	app.store.DeleteList(l.ID)
	l2, _ := app.store.CreateList("Личное", "")
	if l2.ID == l.ID {
		t.Fatalf("list id %d reused", l.ID)
	}
	a, _ := app.store.AddTask(NewTask{ListID: 1, Text: "a"})
	app.store.DeleteTask(a.ID)
	b, _ := app.store.AddTask(NewTask{ListID: 1, Text: "b"})
	if b.ID == a.ID {
		t.Fatalf("task id %d reused", a.ID)
	}
}

// A database made by the first version (ids could be reused) is migrated.
func TestMigrateAutoincrement(t *testing.T) {
	dir := t.TempDir()
	db, _ := sql.Open("sqlite", "file:"+filepath.Join(dir, "data-box.db"))
	old := strings.ReplaceAll(schema, " AUTOINCREMENT", "")
	if _, err := db.Exec(old); err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO lists(id,name,color,created_at) VALUES(1,'Общие','',0),(2,'Дом','',0)`)
	db.Exec(`INSERT INTO tasks(id,list_id,text,created_at,updated_at) VALUES(5,2,'хлеб',0,0)`)
	db.Close()

	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tk, err := st.GetTask(5)
	if err != nil || tk.Text != "хлеб" || tk.ListID != 2 {
		t.Fatalf("data lost: %+v %v", tk, err)
	}
	st.DeleteTask(5)
	n, _ := st.AddTask(NewTask{ListID: 2, Text: "x"})
	if n.ID == 5 {
		t.Fatal("still reusing ids after migration")
	}
	if err := st.DeleteList(2); err != nil { // cascade still works
		t.Fatal(err)
	}
	if _, err := st.GetTask(n.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("cascade lost")
	}
}

// Two deletes at once cannot remove the last list.
func TestDeleteListRace(t *testing.T) {
	for i := 0; i < 20; i++ {
		app, _, _ := newApp(t)
		l, _ := app.store.CreateList("Дом", "")
		var wg sync.WaitGroup
		for _, id := range []int64{1, l.ID} {
			wg.Add(1)
			go func(id int64) { defer wg.Done(); app.store.DeleteList(id) }(id)
		}
		wg.Wait()
		if lists, _ := app.store.Lists(); len(lists) != 1 {
			t.Fatalf("lists left: %d", len(lists))
		}
	}
}

// PATCH writes only the fields it carries.
func TestPatchKeepsOtherFields(t *testing.T) {
	app, _, _ := newApp(t)
	tk, _ := app.store.AddTask(NewTask{ListID: 1, Text: "x"})
	tr, f := true, true
	app.store.UpdateTask(tk.ID, TaskPatch{Done: &tr})
	got, _ := app.store.UpdateTask(tk.ID, TaskPatch{Pri: &f})
	if got.DoneAt == 0 || !got.Pri {
		t.Fatalf("one change undid the other: %+v", got)
	}
	doneAt := got.DoneAt
	got, _ = app.store.UpdateTask(tk.ID, TaskPatch{Done: &tr}) // done again keeps the time
	if got.DoneAt != doneAt {
		t.Fatal("done_at changed")
	}
	no := false
	got, _ = app.store.UpdateTask(tk.ID, TaskPatch{Done: &no})
	if got.DoneAt != 0 || !got.Pri {
		t.Fatalf("undo: %+v", got)
	}
	if _, err := app.store.UpdateTask(999, TaskPatch{Done: &tr}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing task: %v", err)
	}
}

func TestSetTranscript(t *testing.T) {
	app, _, _ := newApp(t)
	ph := voicePlaceholder(5)
	tk, _ := app.store.AddTask(NewTask{ListID: 1, Text: ph, Audio: []byte("a"), AudioMime: "audio/ogg"})
	long := strings.Repeat("слово ", 2000) // 12000 characters
	if ok, err := app.store.SetTranscript(tk.ID, ph, long); !ok || err != nil {
		t.Fatalf("long transcript: %v %v", ok, err)
	}
	got, _ := app.store.GetTask(tk.ID)
	if n := len([]rune(got.Text)); n != maxTaskText {
		t.Fatalf("not cut to the limit: %d", n)
	}
	// Edited by a person meanwhile: the transcript must not overwrite it.
	tk2, _ := app.store.AddTask(NewTask{ListID: 1, Text: ph, Audio: []byte("a"), AudioMime: "audio/ogg"})
	edited := "моя правка"
	app.store.UpdateTask(tk2.ID, TaskPatch{Text: &edited})
	if ok, _ := app.store.SetTranscript(tk2.ID, ph, "транскрипт"); ok {
		t.Fatal("overwrote an edit")
	}
}

func TestAudioQuota(t *testing.T) {
	app, _, _ := newApp(t)
	app.store.MaxAudioBytes = 10
	a, err := app.store.AddTask(NewTask{ListID: 1, Text: "a", Audio: []byte("123456"), AudioMime: "audio/ogg"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.AddTask(NewTask{ListID: 1, Text: "b", Audio: []byte("123456"), AudioMime: "audio/ogg"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("over quota accepted: %v", err)
	}
	app.store.DeleteTask(a.ID) // frees the space
	if _, err := app.store.AddTask(NewTask{ListID: 1, Text: "c", Audio: []byte("123456"), AudioMime: "audio/ogg"}); err != nil {
		t.Fatalf("space not freed: %v", err)
	}
}

func TestTranscribeBudget(t *testing.T) {
	a := &App{TranscribeDailySec: 100}
	if !a.spendTranscribe(60) || a.spendTranscribe(60) || !a.spendTranscribe(40) {
		t.Fatal("budget")
	}
	a.trDay = "1999-01-01" // a new day resets it
	if !a.spendTranscribe(100) {
		t.Fatal("no reset")
	}
}

func TestFilePermissions(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0o755)
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	st.AddTask(NewTask{ListID: 1, Text: "x"})
	st.Close()
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir %v", fi.Mode().Perm())
	}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, _ error) error {
		if fi, _ := d.Info(); !d.IsDir() && fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is %v", p, fi.Mode().Perm())
		}
		return nil
	})
}

func TestIsLocalHost(t *testing.T) {
	for h, want := range map[string]bool{
		"localhost": true, "whisper": true, "127.0.0.1": true, "10.1.2.3": true, "::1": true, "fd00::1": true,
		"api.openai.com": false, "8.8.8.8": false, "2606:4700::1111": false, "2001:db8::1": false, "": false,
	} {
		if isLocalHost(h) != want {
			t.Errorf("%q: want %v", h, want)
		}
	}
}

func TestSubscribersPerUser(t *testing.T) {
	h := NewHub()
	for i := 0; i < maxSubsPerUser; i++ {
		if ch, _ := h.Subscribe("a"); ch == nil {
			t.Fatal("refused too early")
		}
	}
	if ch, _ := h.Subscribe("a"); ch != nil {
		t.Fatal("per-user cap not applied")
	}
	if ch, _ := h.Subscribe("b"); ch == nil {
		t.Fatal("other users must still connect")
	}
}
