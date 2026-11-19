package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
)

// Transcriber turns a recording into text.
type Transcriber interface {
	Transcribe(ctx context.Context, audio []byte, mime string) (string, error)
}

// WhisperAPI calls an OpenAI-compatible /audio/transcriptions endpoint
// (OpenAI, Groq, a local faster-whisper server, …).
type WhisperAPI struct {
	URL   string
	Key   string
	Model string
	Lang  string // optional ISO-639-1 hint, e.g. "ru"
	HTTP  *http.Client
}

func (w *WhisperAPI) Transcribe(ctx context.Context, audio []byte, mime string) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="file"; filename="voice`+audioExt(mime)+`"`)
	if mime == "" {
		mime = "application/octet-stream"
	}
	h.Set("Content-Type", mime)
	part, err := mw.CreatePart(h)
	if err != nil {
		return "", err
	}
	part.Write(audio)
	mw.WriteField("model", w.Model)
	if w.Lang != "" {
		mw.WriteField("language", w.Lang)
	}
	mw.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if w.Key != "" {
		req.Header.Set("Authorization", "Bearer "+w.Key)
	}
	cl := w.HTTP
	if cl == nil {
		cl = http.DefaultClient
	}
	resp, err := cl.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("transcription: HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(data))
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("transcription: %w", err)
	}
	return out.Text, nil
}
