package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

// Store keeps users, lists and tasks in SQLite and voice recordings as files
// in <dir>/audio. Ids are AUTOINCREMENT: a deleted list's or task's id is
// never given to a new one (background transcriptions refer to ids).
type Store struct {
	db  *sql.DB
	dir string

	MaxAudioBytes int64 // total size of recordings; 0 = no limit
	audioMu       sync.Mutex
	audioBytes    int64
}

var (
	ErrNotFound = errors.New("not found")
	// ErrInvalid wraps errors caused by bad input (shown to the client as 400).
	ErrInvalid = errors.New("invalid")
)

const (
	maxTaskText = 4000 // characters
	maxListName = 100
)

func invalid(msg string) error { return fmt.Errorf("%w: %s", ErrInvalid, msg) }

var colorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// audioTypes are the recording formats accepted; anything else is stored as
// application/octet-stream, so a "recording" can never be served as HTML/SVG.
var audioTypes = map[string]string{
	"audio/ogg": ".ogg", "audio/opus": ".ogg", "audio/webm": ".webm", "audio/mpeg": ".mp3", "audio/mp3": ".mp3",
	"audio/mp4": ".m4a", "audio/x-m4a": ".m4a", "audio/aac": ".m4a", "audio/wav": ".wav", "audio/x-wav": ".wav",
}

// AudioType normalises a client-supplied MIME type to an allowed one.
func AudioType(mime string) string {
	base := strings.ToLower(strings.TrimSpace(strings.SplitN(mime, ";", 2)[0]))
	if _, ok := audioTypes[base]; ok {
		return base
	}
	return "application/octet-stream"
}

const schema = `
CREATE TABLE IF NOT EXISTS users(
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	token_hash TEXT NOT NULL UNIQUE,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS lists(
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	color TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tasks(
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	list_id INTEGER NOT NULL REFERENCES lists(id) ON DELETE CASCADE,
	text TEXT NOT NULL,
	pri INTEGER NOT NULL DEFAULT 0,
	done_at INTEGER,
	author TEXT NOT NULL DEFAULT '',
	source TEXT NOT NULL DEFAULT '',
	audio TEXT NOT NULL DEFAULT '',
	audio_mime TEXT NOT NULL DEFAULT '',
	audio_sec INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS tasks_list ON tasks(list_id);
`

var palette = []string{"#6366f1", "#22c55e", "#f59e0b", "#ec4899", "#14b8a6", "#8b5cf6", "#ef4444", "#0ea5e9"}

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "audio"), 0o700); err != nil {
		return nil, err
	}
	// Only the server's user may read the data (SQLite gives -wal/-shm the
	// database file's mode).
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(dir, "data-box.db")
	if f, err := os.OpenFile(dbPath, os.O_CREATE|os.O_RDWR, 0o600); err != nil {
		return nil, err
	} else {
		f.Close()
	}
	for _, f := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Chmod(f, 0o600); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	dsn := "file:" + dbPath +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateAutoincrement(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	s := &Store{db: db, dir: dir}
	if err := s.countAudio(); err != nil {
		db.Close()
		return nil, err
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM lists`).Scan(&n); err != nil {
		db.Close()
		return nil, err
	}
	if n == 0 {
		if _, err := s.CreateList("Общие", ""); err != nil {
			db.Close()
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// migrateAutoincrement rebuilds lists/tasks created by the first version
// (plain INTEGER PRIMARY KEY, which reuses the highest deleted id).
func migrateAutoincrement(db *sql.DB) error {
	var ddl string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='tasks'`).Scan(&ddl); err != nil {
		return err
	}
	if strings.Contains(ddl, "AUTOINCREMENT") {
		return nil
	}
	if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	defer db.Exec(`PRAGMA foreign_keys=ON`)
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`ALTER TABLE tasks RENAME TO tasks_old`,
		`ALTER TABLE lists RENAME TO lists_old`,
		`DROP INDEX IF EXISTS tasks_list`,
		schema,
		`INSERT INTO lists SELECT * FROM lists_old`,
		`INSERT INTO tasks SELECT * FROM tasks_old`,
		`DROP TABLE tasks_old`,
		`DROP TABLE lists_old`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- recordings: total size limit ----

func (s *Store) countAudio() error {
	entries, err := os.ReadDir(filepath.Join(s.dir, "audio"))
	if err != nil {
		return err
	}
	var n int64
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
			n += info.Size()
		}
	}
	s.audioMu.Lock()
	s.audioBytes = n
	s.audioMu.Unlock()
	return nil
}

// reserveAudio counts size bytes against MaxAudioBytes, or fails when full.
func (s *Store) reserveAudio(size int64) error {
	s.audioMu.Lock()
	defer s.audioMu.Unlock()
	if s.MaxAudioBytes > 0 && s.audioBytes+size > s.MaxAudioBytes {
		return invalid("хранилище записей заполнено")
	}
	s.audioBytes += size
	return nil
}

func now() int64 { return time.Now().Unix() }

// ---- users / tokens ----

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// CreateUser makes a user and returns its token (shown once).
func (s *Store) CreateUser(name string) (string, error) {
	name = strings.TrimSpace(name)
	if err := checkListName(name); err != nil {
		return "", err
	}
	b := make([]byte, 24)
	rand.Read(b)
	tok := "db_" + base64.RawURLEncoding.EncodeToString(b)
	_, err := s.db.Exec(`INSERT INTO users(name, token_hash, created_at) VALUES(?,?,?)`, name, hashToken(tok), now())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return "", fmt.Errorf("user %q already exists", name)
		}
		return "", err
	}
	return tok, nil
}

// UserByToken returns the user's name, or ErrNotFound.
func (s *Store) UserByToken(tok string) (string, error) {
	if tok == "" {
		return "", ErrNotFound
	}
	var name string
	err := s.db.QueryRow(`SELECT name FROM users WHERE token_hash=?`, hashToken(tok)).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return name, err
}

func (s *Store) Users() ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) DeleteUser(name string) error {
	r, err := s.db.Exec(`DELETE FROM users WHERE name=?`, name)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- lists ----

type List struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

func (s *Store) Lists() ([]List, error) {
	rows, err := s.db.Query(`SELECT id, name, color FROM lists ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []List{}
	for rows.Next() {
		var l List
		if err := rows.Scan(&l.ID, &l.Name, &l.Color); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) GetList(id int64) (List, error) {
	var l List
	err := s.db.QueryRow(`SELECT id, name, color FROM lists WHERE id=?`, id).Scan(&l.ID, &l.Name, &l.Color)
	if errors.Is(err, sql.ErrNoRows) {
		return l, ErrNotFound
	}
	return l, err
}

func (s *Store) CreateList(name, color string) (List, error) {
	name = strings.TrimSpace(name)
	if err := checkListName(name); err != nil {
		return List{}, err
	}
	if color != "" && !colorRe.MatchString(color) {
		return List{}, invalid("color must be #rrggbb")
	}
	if color == "" {
		var n int
		s.db.QueryRow(`SELECT COUNT(*) FROM lists`).Scan(&n)
		color = palette[n%len(palette)]
	}
	r, err := s.db.Exec(`INSERT INTO lists(name, color, created_at) VALUES(?,?,?)`, name, color, now())
	if err != nil {
		return List{}, err
	}
	id, _ := r.LastInsertId()
	return List{ID: id, Name: name, Color: color}, nil
}

func (s *Store) UpdateList(id int64, name, color *string) (List, error) {
	l, err := s.GetList(id)
	if err != nil {
		return l, err
	}
	if name != nil {
		if err := checkListName(strings.TrimSpace(*name)); err != nil {
			return l, err
		}
		l.Name = strings.TrimSpace(*name)
	}
	if color != nil {
		if !colorRe.MatchString(*color) {
			return l, invalid("color must be #rrggbb")
		}
		l.Color = *color
	}
	_, err = s.db.Exec(`UPDATE lists SET name=?, color=? WHERE id=?`, l.Name, l.Color, id)
	return l, err
}

func checkListName(name string) error {
	if name == "" {
		return invalid("empty name")
	}
	if utf8.RuneCountInString(name) > maxListName {
		return invalid("name too long")
	}
	return nil
}

func checkTaskText(text string) error {
	if text == "" {
		return invalid("empty task")
	}
	if utf8.RuneCountInString(text) > maxTaskText {
		return invalid(fmt.Sprintf("task longer than %d characters", maxTaskText))
	}
	return nil
}

// DeleteList removes a list with its tasks and their recordings. The last
// list cannot be deleted. Everything happens in one transaction, so two
// deletes at once cannot remove the last list and the removed files are
// exactly those of the deleted tasks.
func (s *Store) DeleteList(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists, n int
	if err := tx.QueryRow(`SELECT COUNT(*) FILTER (WHERE id=?), COUNT(*) FROM lists`, id).Scan(&exists, &n); err != nil {
		return err
	}
	if exists == 0 {
		return ErrNotFound
	}
	if n <= 1 {
		return invalid("cannot delete the last list")
	}
	files, err := audioFiles(tx, `SELECT audio FROM tasks WHERE list_id=? AND audio<>''`, id)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM lists WHERE id=?`, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.removeAudio(files)
	return nil
}

// ---- tasks ----

type Task struct {
	ID        int64  `json:"id"`
	ListID    int64  `json:"list_id"`
	Text      string `json:"text"`
	Pri       bool   `json:"pri"`
	DoneAt    int64  `json:"done_at,omitempty"` // 0 = open
	Author    string `json:"author"`
	Source    string `json:"source"`
	HasAudio  bool   `json:"audio"`
	AudioSec  int    `json:"audio_sec,omitempty"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`

	audio     string
	audioMime string
}

const taskCols = `id, list_id, text, pri, COALESCE(done_at,0), author, source, audio, audio_mime, audio_sec, created_at, updated_at`

func scanTask(sc interface{ Scan(...any) error }) (Task, error) {
	var t Task
	err := sc.Scan(&t.ID, &t.ListID, &t.Text, &t.Pri, &t.DoneAt, &t.Author, &t.Source, &t.audio, &t.audioMime, &t.AudioSec, &t.CreatedAt, &t.UpdatedAt)
	t.HasAudio = t.audio != ""
	return t, err
}

// Tasks returns open tasks and the ones done within doneWindow, ordered:
// open before done, priority first, then oldest first; done ones newest first.
func (s *Store) Tasks(doneWindow time.Duration) ([]Task, error) {
	since := time.Now().Add(-doneWindow).Unix()
	rows, err := s.db.Query(`SELECT `+taskCols+` FROM tasks
		WHERE done_at IS NULL OR done_at >= ?
		ORDER BY done_at IS NOT NULL, pri DESC, COALESCE(-done_at, created_at), id`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) GetTask(id int64) (Task, error) {
	t, err := scanTask(s.db.QueryRow(`SELECT `+taskCols+` FROM tasks WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

type NewTask struct {
	ListID    int64
	Text      string
	Pri       bool
	Author    string
	Source    string
	Audio     []byte // optional recording
	AudioMime string
	AudioSec  int
}

func (s *Store) AddTask(n NewTask) (Task, error) {
	n.Text = strings.TrimSpace(n.Text)
	if err := checkTaskText(n.Text); err != nil {
		return Task{}, err
	}
	if _, err := s.GetList(n.ListID); err != nil {
		return Task{}, fmt.Errorf("list %d: %w", n.ListID, err)
	}
	var file string
	if len(n.Audio) > 0 {
		n.AudioMime = AudioType(n.AudioMime)
		if err := s.reserveAudio(int64(len(n.Audio))); err != nil {
			return Task{}, err
		}
		b := make([]byte, 12)
		rand.Read(b)
		file = hex.EncodeToString(b) + audioExt(n.AudioMime)
		if err := os.WriteFile(filepath.Join(s.dir, "audio", file), n.Audio, 0o600); err != nil {
			os.Remove(filepath.Join(s.dir, "audio", file))
			s.reserveAudio(-int64(len(n.Audio)))
			return Task{}, err
		}
	}
	ts := now()
	r, err := s.db.Exec(`INSERT INTO tasks(list_id, text, pri, author, source, audio, audio_mime, audio_sec, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, n.ListID, n.Text, n.Pri, n.Author, n.Source, file, n.AudioMime, n.AudioSec, ts, ts)
	if err != nil {
		s.removeAudio([]string{file})
		return Task{}, err
	}
	id, _ := r.LastInsertId()
	return s.GetTask(id)
}

func audioExt(mime string) string {
	if ext, ok := audioTypes[AudioType(mime)]; ok {
		return ext
	}
	return ".bin"
}

type TaskPatch struct {
	Text   *string `json:"text"`
	ListID *int64  `json:"list_id"`
	Pri    *bool   `json:"pri"`
	Done   *bool   `json:"done"`
}

// UpdateTask changes only the fields set in p, in one statement, so two
// people editing different fields at once don't undo each other.
func (s *Store) UpdateTask(id int64, p TaskPatch) (Task, error) {
	var text any
	if p.Text != nil {
		t := strings.TrimSpace(*p.Text)
		if err := checkTaskText(t); err != nil {
			return Task{}, err
		}
		text = t
	}
	var list any
	if p.ListID != nil {
		if _, err := s.GetList(*p.ListID); err != nil {
			return Task{}, fmt.Errorf("list %d: %w", *p.ListID, err)
		}
		list = *p.ListID
	}
	var pri, done any
	if p.Pri != nil {
		pri = *p.Pri
	}
	if p.Done != nil {
		done = *p.Done
	}
	ts := now()
	r, err := s.db.Exec(`UPDATE tasks SET
		text = COALESCE(?, text),
		list_id = COALESCE(?, list_id),
		pri = COALESCE(?, pri),
		done_at = CASE WHEN ? IS NULL THEN done_at WHEN ? THEN COALESCE(done_at, ?) ELSE NULL END,
		updated_at = ?
		WHERE id = ?`, text, list, pri, done, done, ts, ts, id)
	if err != nil {
		if strings.Contains(err.Error(), "FOREIGN KEY") {
			return Task{}, ErrNotFound // the list was deleted meanwhile
		}
		return Task{}, err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return Task{}, ErrNotFound
	}
	return s.GetTask(id)
}

// SetTranscript replaces a voice task's placeholder with the transcript —
// only if it is still the same recording and nobody has edited the text.
func (s *Store) SetTranscript(id int64, placeholder, text string) (bool, error) {
	if r := []rune(text); len(r) > maxTaskText {
		text = string(r[:maxTaskText-1]) + "…"
	}
	r, err := s.db.Exec(`UPDATE tasks SET text=?, updated_at=? WHERE id=? AND text=? AND audio<>''`,
		text, now(), id, placeholder)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n == 1, nil
}

func (s *Store) DeleteTask(id int64) error {
	t, err := s.GetTask(id)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM tasks WHERE id=?`, id); err != nil {
		return err
	}
	s.removeAudio([]string{t.audio})
	return nil
}

// AudioPath returns the recording's file and MIME type.
func (s *Store) AudioPath(t Task) (string, string, bool) {
	if t.audio == "" {
		return "", "", false
	}
	return filepath.Join(s.dir, "audio", t.audio), AudioType(t.audioMime), true
}

// PurgeDone deletes tasks done before the cutoff (and their recordings).
func (s *Store) PurgeDone(olderThan time.Duration) (int, error) {
	cut := time.Now().Add(-olderThan).Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	files, err := audioFiles(tx, `SELECT audio FROM tasks WHERE done_at < ? AND audio<>''`, cut)
	if err != nil {
		return 0, err
	}
	r, err := tx.Exec(`DELETE FROM tasks WHERE done_at < ?`, cut)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	s.removeAudio(files)
	n, _ := r.RowsAffected()
	return int(n), nil
}

func audioFiles(tx *sql.Tx, q string, args ...any) ([]string, error) {
	rows, err := tx.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) removeAudio(files []string) {
	for _, f := range files {
		if f == "" {
			continue
		}
		path := filepath.Join(s.dir, "audio", filepath.Base(f))
		if info, err := os.Stat(path); err == nil && os.Remove(path) == nil {
			s.audioMu.Lock()
			s.audioBytes -= info.Size()
			s.audioMu.Unlock()
		}
	}
}
