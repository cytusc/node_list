// Package notify sends Feishu custom-bot alerts.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// throttleWindow collapses repeated identical messages so a prolonged outage
// cannot flood the chat.
const throttleWindow = 30 * time.Minute

// Feishu builds a notifier. An empty webhook yields a no-op notifier, which
// keeps every call site unconditional.
func Feishu(webhook string) func(context.Context, string) error {
	if strings.TrimSpace(webhook) == "" {
		return func(context.Context, string) error { return nil }
	}
	var mu sync.Mutex
	var lastSentAt time.Time
	var lastMessage string
	client := &http.Client{Timeout: 10 * time.Second}
	return func(ctx context.Context, message string) error {
		if strings.TrimSpace(message) == "" {
			return nil
		}
		mu.Lock()
		suppressed := message == lastMessage && time.Since(lastSentAt) < throttleWindow
		mu.Unlock()
		if suppressed {
			return nil
		}
		body, err := json.Marshal(map[string]any{
			"msg_type": "text",
			"content":  map[string]string{"text": message},
		})
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			fmt.Printf("Feishu notify failed: %v\n", err)
			return err
		}
		defer func() {
			io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))
			response.Body.Close()
		}()
		var payload struct {
			Code          int    `json:"code"`
			StatusCode    int    `json:"StatusCode"`
			Msg           string `json:"msg"`
			StatusMessage string `json:"StatusMessage"`
		}
		_ = json.NewDecoder(io.LimitReader(response.Body, 1<<16)).Decode(&payload)
		ok := response.StatusCode >= 200 && response.StatusCode < 300 &&
			(payload.Code == 0 || payload.StatusCode == 0)
		if !ok {
			detail := payload.Msg
			if detail == "" {
				detail = payload.StatusMessage
			}
			if detail == "" {
				detail = "unknown error"
			}
			err = fmt.Errorf("Feishu notify rejected (HTTP %d): %s", response.StatusCode, detail)
			fmt.Println(err)
			return err
		}
		mu.Lock()
		lastSentAt = time.Now()
		lastMessage = message
		mu.Unlock()
		return nil
	}
}
