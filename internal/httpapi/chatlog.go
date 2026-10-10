package httpapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"hao-agent-harness/internal/chat"
)

// Serialize complete turn appends; streaming only aggregates text in memory.
var chatLogWriteMu sync.Mutex

type logChoice struct {
	content strings.Builder
	role    string
	finish  *string
}

type chatLog struct {
	path      string
	sessionID string
	request   any
	response  chat.Response
	choices   map[int]*logChoice
	failure   any
	err       error
}

func newChatLog(base, sessionID string, body []byte) *chatLog {
	var request any = json.RawMessage(body)
	if !json.Valid(body) {
		request = string(body)
	}
	return &chatLog{path: filepath.Join(base, "session-"+sessionID+".log"),
		sessionID: sessionID, request: request, choices: make(map[int]*logChoice)}
}

func (l *chatLog) record(value any) {
	switch value := value.(type) {
	case chat.Response:
		l.response = value
	case chat.Event:
		if value.ID != "" {
			l.response.ID = value.ID
		}
		l.response.Object = "chat.completion"
		if value.Created != 0 {
			l.response.Created = value.Created
		}
		if value.Model != "" {
			l.response.Model = value.Model
		}
		for _, delta := range value.Choices {
			choice := l.choices[delta.Index]
			if choice == nil {
				choice = &logChoice{role: "assistant"}
				l.choices[delta.Index] = choice
			}
			if delta.Delta.Role != "" {
				choice.role = delta.Delta.Role
			}
			if delta.Delta.Content != nil {
				choice.content.WriteString(*delta.Delta.Content)
			}
			if delta.FinishReason != nil {
				choice.finish = delta.FinishReason
			}
		}
	default:
		l.failure = value
	}
}

func (l *chatLog) finish(t *trace, started time.Time) {
	if len(l.choices) > 0 {
		indexes := make([]int, 0, len(l.choices))
		for index := range l.choices {
			indexes = append(indexes, index)
		}
		sort.Ints(indexes)
		for _, index := range indexes {
			choice := l.choices[index]
			l.response.Choices = append(l.response.Choices, chat.Choice{Index: index,
				Message: chat.Message{Role: choice.role, Content: choice.content.String()}, FinishReason: choice.finish})
		}
	}
	l.response.Usage = t.usage
	entry := struct {
		SessionID  string        `json:"session_id"`
		RequestID  string        `json:"request_id"`
		StartedAt  string        `json:"started_at"`
		FinishedAt string        `json:"finished_at"`
		DurationMS int64         `json:"duration_ms"`
		Model      string        `json:"model"`
		Provider   string        `json:"provider"`
		Status     int           `json:"status"`
		Outcome    string        `json:"outcome"`
		Stream     bool          `json:"stream"`
		Request    any           `json:"request"`
		Response   chat.Response `json:"response"`
		Error      any           `json:"error,omitempty"`
	}{l.sessionID, t.id, started.Format(time.RFC3339Nano), time.Now().Format(time.RFC3339Nano),
		time.Since(started).Milliseconds(), t.labels.Model, t.labels.Provider, t.labels.Status,
		t.labels.Outcome, t.labels.Stream, l.request, l.response, l.failure}
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		l.err = err
		return
	}
	data = append(data, '\n', '\n')
	chatLogWriteMu.Lock()
	defer chatLogWriteMu.Unlock()
	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		l.err = err
		return
	}
	_, l.err = file.Write(data)
	if err := file.Sync(); l.err == nil {
		l.err = err
	}
	if err := file.Close(); l.err == nil {
		l.err = err
	}
}
