// Package hcapi is a minimal Hardcover GraphQL client with rate limiting
// tuned to Hardcover's published limits (burst 10, 60 req/min, 5000/day).
package hcapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const endpoint = "https://api.hardcover.app/v1/graphql"

// ErrDailyExhausted is returned when the daily request budget (minus the
// configured reserve) has been consumed. Callers should checkpoint and exit.
var ErrDailyExhausted = errors.New("hardcover daily request budget exhausted")

type Client struct {
	http         *http.Client
	token        string
	minInterval  time.Duration
	dailyReserve int64

	lastRequest    time.Time
	dailyRemaining int64
	dailyReset     time.Time
}

type Option func(*Client)

// WithDailyReserve stops the client once fewer than n daily requests remain,
// leaving headroom for manual queries against the same token.
func WithDailyReserve(n int64) Option {
	return func(c *Client) { c.dailyReserve = n }
}

func NewClient(token string, opts ...Option) *Client {
	c := &Client{
		http:           &http.Client{Timeout: 90 * time.Second},
		token:          token,
		minInterval:    time.Second, // 60/min
		dailyReserve:   100,
		dailyRemaining: -1, // unknown until first response
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// DailyRemaining reports the last daily-remaining value seen, or -1 before
// the first request.
func (c *Client) DailyRemaining() int64 { return c.dailyRemaining }

type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type gqlError struct {
	Message string `json:"message"`
}

type gqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []gqlError      `json:"errors"`
	// Non-GraphQL API errors use these fields.
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// Query runs one GraphQL query and returns the raw data payload.
// It paces requests, honors 429s, and retries transient failures.
func (c *Client) Query(ctx context.Context, query string, variables map[string]any) (json.RawMessage, error) {
	if c.dailyRemaining >= 0 && c.dailyRemaining <= c.dailyReserve {
		if wait := time.Until(c.dailyReset); wait > 0 {
			return nil, fmt.Errorf("%w (resets in %s)", ErrDailyExhausted, wait.Round(time.Minute))
		}
		c.dailyRemaining = -1
	}

	body, err := json.Marshal(gqlRequest{Query: query, Variables: variables})
	if err != nil {
		return nil, err
	}

	const maxAttempts = 5
	backoff := 2 * time.Second
	for attempt := 1; ; attempt++ {
		if err := c.pace(ctx); err != nil {
			return nil, err
		}
		data, retryable, err := c.do(ctx, body)
		if err == nil {
			return data, nil
		}
		if !retryable || attempt == maxAttempts {
			return nil, err
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		backoff *= 2
	}
}

func (c *Client) pace(ctx context.Context) error {
	if wait := c.minInterval - time.Since(c.lastRequest); wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.lastRequest = time.Now()
	return nil
}

func (c *Client) do(ctx context.Context, body []byte) (data json.RawMessage, retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, true, err
	}
	defer resp.Body.Close()
	c.recordLimits(resp.Header)

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		if c.dailyRemaining == 0 {
			return nil, false, fmt.Errorf("%w (resets at %s)", ErrDailyExhausted, c.dailyReset.Format(time.RFC3339))
		}
		return nil, true, fmt.Errorf("rate limited (429)")
	case resp.StatusCode >= 500:
		return nil, true, fmt.Errorf("server error: %s", resp.Status)
	case resp.StatusCode != http.StatusOK:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, false, fmt.Errorf("unexpected status %s: %s", resp.Status, b)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, true, err
	}
	var gr gqlResponse
	if err := json.Unmarshal(raw, &gr); err != nil {
		return nil, true, fmt.Errorf("decoding response: %w", err)
	}
	if gr.Error != "" {
		return nil, false, fmt.Errorf("api error: %s: %s", gr.Error, gr.ErrorDescription)
	}
	if len(gr.Errors) > 0 {
		return nil, false, fmt.Errorf("graphql error: %s", gr.Errors[0].Message)
	}
	return gr.Data, false, nil
}

func (c *Client) recordLimits(h http.Header) {
	if v := h.Get("x-ratelimit-daily-remaining"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			c.dailyRemaining = n
		}
	}
	if v := h.Get("x-ratelimit-daily-reset"); v != "" {
		if sec, err := strconv.ParseInt(v, 10, 64); err == nil {
			c.dailyReset = time.Unix(sec, 0)
		}
	}
}

// TokenFromEnv reads HARDCOVER_TOKEN from the environment, falling back to
// a .env file in the working directory. Empty when neither is set.
func TokenFromEnv() string {
	if t := os.Getenv("HARDCOVER_TOKEN"); t != "" {
		return t
	}
	f, err := os.Open(".env")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if v, ok := strings.CutPrefix(line, "HARDCOVER_TOKEN="); ok {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}
