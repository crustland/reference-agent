// The planner's model adapter: one Messages-API request over plain HTTP, used by planner.FromEnv when the planner is turned on.

package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Doer is the part of *http.Client the Anthropic adapter uses.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// anthropicVersion is the API version header the Messages API requires.
const anthropicVersion = "2023-06-01"

// maxReply caps what is read from a reply.
const maxReply = 1 << 20

// Anthropic is the Completer that calls the Messages API over plain HTTP, so this directory needs no dependency. It sends
// one request and returns the text of the reply and the tokens it reports; it does not retry, stream or use tools, and it
// never writes the key anywhere.
//
// It has been checked against recorded replies only (anthropic_test.go). It has not been run against the live service.
type Anthropic struct {
	baseURL string
	apiKey  string
	http    Doer
}

// NewAnthropic returns a Completer for the Messages API at baseURL. A nil Doer means a client with no timeout beyond the
// request's context, so give the context a deadline.
func NewAnthropic(baseURL, apiKey string, doer Doer) *Anthropic {
	if doer == nil {
		doer = http.DefaultClient
	}
	return &Anthropic{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, http: doer}
}

// String never shows the key.
func (a *Anthropic) String() string { return "anthropic{" + a.baseURL + ", key hidden}" }

type messagesRequest struct {
	Model     string         `json:"model"`
	MaxTokens int            `json:"max_tokens"`
	System    string         `json:"system"`
	Messages  []messagesTurn `json:"messages"`
}

type messagesTurn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type messagesReply struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Usage struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Complete sends the request.
func (a *Anthropic) Complete(ctx context.Context, r Request) (Response, error) {
	body, err := json.Marshal(messagesRequest{Model: r.Model, MaxTokens: r.MaxTokens, System: r.System, Messages: []messagesTurn{{Role: "user", Content: r.User}}})
	if err != nil {
		return Response{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", a.apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	resp, err := a.http.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("the request could not be sent: %w", redact(err, a.apiKey))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxReply))
	if err != nil {
		return Response{}, fmt.Errorf("reading the reply: %w", err)
	}
	var reply messagesReply
	_ = json.Unmarshal(raw, &reply)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := http.StatusText(resp.StatusCode)
		if reply.Error != nil && reply.Error.Message != "" {
			msg = reply.Error.Type + ": " + reply.Error.Message
		}
		return Response{}, fmt.Errorf("the service answered %d: %s", resp.StatusCode, redactText(msg, a.apiKey))
	}
	var text strings.Builder
	for _, c := range reply.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	if text.Len() == 0 {
		return Response{}, fmt.Errorf("the reply has no text")
	}
	return Response{Text: text.String(), InputTokens: reply.Usage.InputTokens, OutputTokens: reply.Usage.OutputTokens}, nil
}

// redact keeps a key out of an error that echoes the request.
func redact(err error, key string) error {
	if key == "" {
		return err
	}
	return fmt.Errorf("%s", redactText(err.Error(), key))
}

func redactText(s, key string) string {
	if key == "" {
		return s
	}
	return strings.ReplaceAll(s, key, "[key hidden]")
}
