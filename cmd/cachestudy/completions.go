package main

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

const (
	// completionTimeout bounds one generation. The intended upstream is a
	// small local model, so a request still running after this is stuck
	// rather than slow.
	completionTimeout time.Duration = 5 * time.Minute

	// maxCompletionErrorBody caps how much of a failure body is read.
	maxCompletionErrorBody int64 = 4 << 10
)

// completion is one generated answer and what it cost.
//
// Tokens is what turns a hit rate into money. A hit avoids generating
// this many completion tokens, so summing them over correct hits gives
// the saving and summing them over false hits gives the part of the
// saving that answered a different question.
type completion struct {
	Text   string `json:"text"`
	Tokens int    `json:"tokens"`
}

// completionOptions configures the generator.
type completionOptions struct {
	BaseURL     string
	Model       string
	MaxTokens   int
	Concurrency int
}

type completionRequest struct {
	Model       string        `json:"model"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
	Messages    []chatMessage `json:"messages"`
}

type completionResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// fetchCompletions generates a real answer for every question, so token
// counts come from a model rather than from an estimate.
//
// A failure fails the whole run. A partial map would silently value some
// hits at zero tokens, and a cost table with holes in it reads as a
// smaller saving rather than as a broken measurement.
func fetchCompletions(ctx context.Context, opts completionOptions, questions []string, progress func(done, total int)) (map[string]completion, error) {
	if opts.Concurrency <= 0 {
		return nil, fmt.Errorf("completion concurrency must be positive, got %d", opts.Concurrency)
	}
	client := &http.Client{Timeout: completionTimeout}

	var (
		mu      sync.Mutex
		out     = make(map[string]completion, len(questions))
		firstEr error
		done    int
	)
	work := make(chan string)

	var wg sync.WaitGroup
	for range opts.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for question := range work {
				comp, err := complete(ctx, client, opts, question)
				mu.Lock()
				if err != nil && firstEr == nil {
					firstEr = fmt.Errorf("completion for %q: %w", question, err)
				}
				if err == nil {
					out[question] = comp
				}
				done++
				if progress != nil {
					progress(done, len(questions))
				}
				mu.Unlock()
			}
		}()
	}

	for _, question := range questions {
		mu.Lock()
		failed := firstEr != nil
		mu.Unlock()
		if failed {
			break
		}
		work <- question
	}
	close(work)
	wg.Wait()

	if firstEr != nil {
		return nil, firstEr
	}
	return out, nil
}

func complete(ctx context.Context, client *http.Client, opts completionOptions, question string) (completion, error) {
	body, err := json.Marshal(completionRequest{
		Model:       opts.Model,
		Temperature: 0,
		MaxTokens:   opts.MaxTokens,
		Messages:    []chatMessage{{Role: "user", Content: question}},
	})
	if err != nil {
		return completion{}, fmt.Errorf("encode request: %w", err)
	}

	url := strings.TrimRight(opts.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return completion{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return completion{}, fmt.Errorf("call %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, maxCompletionErrorBody))
		return completion{}, fmt.Errorf("upstream returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}

	var decoded completionResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return completion{}, fmt.Errorf("decode response: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return completion{}, fmt.Errorf("upstream returned no choices")
	}
	if decoded.Usage.CompletionTokens <= 0 {
		// Without a token count this answer cannot be priced, and an
		// invented count is worse than no cost table at all.
		return completion{}, fmt.Errorf("upstream reported no completion tokens")
	}
	return completion{
		Text:   decoded.Choices[0].Message.Content,
		Tokens: decoded.Usage.CompletionTokens,
	}, nil
}
