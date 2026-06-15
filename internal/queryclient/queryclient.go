// Package queryclient is a thin HTTP client + output formatters for querying an
// Omni-logging server from the terminal (the `omnilog query` subcommand).
package queryclient

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
	"github.com/pod32g/omni-logging/internal/store"
)

// Client talks to an Omni-logging server's query API.
type Client struct {
	ServerURL string
	Token     string
	HTTP      *http.Client
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *Client) urlFor(path string, params map[string]string) string {
	v := url.Values{}
	for k, val := range params {
		if val != "" {
			v.Set(k, val)
		}
	}
	return strings.TrimRight(c.ServerURL, "/") + path + "?" + v.Encode()
}

// Search runs a one-shot search and returns the result.
func (c *Client) Search(ctx context.Context, params map[string]string) (store.SearchResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.urlFor("/api/v1/search", params), nil)
	if err != nil {
		return store.SearchResult{}, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return store.SearchResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return store.SearchResult{}, fmt.Errorf("search: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var res store.SearchResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return store.SearchResult{}, fmt.Errorf("decode response: %w", err)
	}
	return res, nil
}

// Follow streams matching events (SSE) to onEvent until ctx is cancelled or the
// stream ends.
func (c *Client) Follow(ctx context.Context, params map[string]string, onEvent func(model.LogEvent)) error {
	p := map[string]string{}
	for k, v := range params {
		p[k] = v
	}
	if c.Token != "" {
		p["token"] = c.Token // SSE auth via query param (EventSource can't set headers)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.urlFor("/api/v1/tail", p), nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tail: %s", resp.Status)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue // comments / blank lines
		}
		var e model.LogEvent
		if err := json.Unmarshal([]byte(data), &e); err == nil {
			onEvent(e)
		}
	}
	return sc.Err()
}

// --- formatters ------------------------------------------------------------

// WriteJSON writes the full result as indented JSON.
func WriteJSON(w io.Writer, res store.SearchResult) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(res)
}

// WriteNDJSON writes one event per line as compact JSON.
func WriteNDJSON(w io.Writer, events []model.LogEvent) error {
	enc := json.NewEncoder(w)
	for _, e := range events {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return nil
}

// WriteTable writes a fixed-column, human-readable table.
func WriteTable(w io.Writer, events []model.LogEvent) error {
	if _, err := fmt.Fprintf(w, "%-24s  %-5s  %-16s  %s\n", "TIMESTAMP", "LEVEL", "SERVICE", "MESSAGE"); err != nil {
		return err
	}
	for _, e := range events {
		msg := strings.ReplaceAll(e.Message, "\n", " ")
		if msg == "" {
			msg = strings.ReplaceAll(e.Raw, "\n", " ")
		}
		if _, err := fmt.Fprintf(w, "%-24s  %-5s  %-16s  %s\n",
			e.Timestamp.UTC().Format("2006-01-02T15:04:05Z"),
			truncate(string(e.Level), 5), truncate(e.Service, 16), msg); err != nil {
			return err
		}
	}
	return nil
}

// FormatEventLine renders a single event as one line in the given format ("table"
// row or NDJSON), used by Follow.
func FormatEventLine(w io.Writer, e model.LogEvent, format string) error {
	if format == "table" {
		msg := strings.ReplaceAll(e.Message, "\n", " ")
		_, err := fmt.Fprintf(w, "%-24s  %-5s  %-16s  %s\n",
			e.Timestamp.UTC().Format("2006-01-02T15:04:05Z"),
			truncate(string(e.Level), 5), truncate(e.Service, 16), msg)
		return err
	}
	return json.NewEncoder(w).Encode(e)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
