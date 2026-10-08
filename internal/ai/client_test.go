package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var fixture = Snapshot{ID: "snapshot-1", Evidence: []Evidence{{ID: "E1", Kind: "read", Data: json.RawMessage(`{"slave":1,"timeout_ms":1000}`)}}}

const goodResult = `{"summary":"应答较慢","observations":[{"text":"超时设置为1000ms","evidence_ids":["E1"]}],"hypotheses":[{"text":"设备可能需要更长时间","evidence_ids":["E1"]}],"next_checks":["对照晚到响应记录检查延迟"]}`

func envelope(result string) string {
	b, _ := json.Marshal(result)
	return fmt.Sprintf(`{"model":"deepseek-flash","choices":[{"message":{"content":%s},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":34,"total_tokens":46}}`, b)
}
func TestAnalyzeSendsJSONAndValidatesEvidence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Error("wrong request credentials or route")
		}
		var req struct {
			Model          string
			Messages       []struct{ Role, Content string }
			ResponseFormat struct{ Type string } `json:"response_format"`
			Stream         bool
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.ResponseFormat.Type != "json_object" || req.Stream || req.Model != "deepseek-flash" || len(req.Messages) != 2 || !strings.Contains(req.Messages[1].Content, "snapshot-1") {
			t.Error("wrong JSON mode or context")
		}
		fmt.Fprint(w, envelope(goodResult))
	}))
	defer srv.Close()
	c := Client{Endpoint: srv.URL, Key: "test-secret", Model: "deepseek-flash"}
	out, err := c.Analyze(context.Background(), fixture, "分析超时")
	if err != nil {
		t.Fatal(err)
	}
	if out.Result.Summary != "应答较慢" || out.Usage.TotalTokens != 46 || out.Model != "deepseek-flash" {
		t.Fatalf("unexpected response: %+v", out)
	}
}
func TestAnalyzeRejectsInvalidOrTruncatedResults(t *testing.T) {
	cases := []string{envelope(strings.ReplaceAll(goodResult, "E1", "unknown")), envelope(`{"summary":"","observations":[],"hypotheses":[],"next_checks":[]}`), strings.Replace(envelope(goodResult), `"stop"`, `"length"`, 1), `{"choices":[]}`, envelope(`{`), strings.Repeat(" ", MaxResponseBytes+1)}
	for i, body := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer srv.Close()
			_, err := (&Client{Endpoint: srv.URL, Key: "test-secret", Model: "deepseek-flash"}).Analyze(context.Background(), fixture, "分析")
			if err == nil {
				t.Fatal("invalid result accepted")
			}
		})
	}
}
func TestAnalyzeErrorsNeverDiscloseSecrets(t *testing.T) {
	for _, status := range []int{401, 402, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				fmt.Fprint(w, "echoed test-secret device-private-data")
			}))
			defer srv.Close()
			_, err := (&Client{Endpoint: srv.URL, Key: "test-secret", Model: "deepseek-flash"}).Analyze(context.Background(), fixture, "分析")
			if err == nil || strings.Contains(err.Error(), "test-secret") || strings.Contains(err.Error(), "device-private-data") {
				t.Fatalf("unsafe error %v", err)
			}
		})
	}
}
func TestAnalyzeCancellationAndDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer srv.Close()
	c := Client{Endpoint: srv.URL, Key: "test-secret", Model: "deepseek-flash"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Analyze(ctx, fixture, "分析"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.Analyze(ctx, fixture, "分析"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline %v", err)
	}
}
func TestAnalyzeRefusesRedirectAndOversizedContext(t *testing.T) {
	var leaked bool
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer dst.Close()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dst.URL, http.StatusTemporaryRedirect)
	}))
	defer src.Close()
	c := Client{Endpoint: src.URL, Key: "test-secret", Model: "deepseek-flash"}
	if _, err := c.Analyze(context.Background(), fixture, "分析"); err == nil || leaked {
		t.Fatal("redirect followed")
	}
	large := Snapshot{ID: "large", Evidence: []Evidence{{ID: "E1", Kind: "read", Data: json.RawMessage(`{"value":"` + strings.Repeat("x", MaxContextBytes) + `"}`)}}}
	if _, err := c.Analyze(context.Background(), large, "分析"); err == nil {
		t.Fatal("oversized context sent")
	}
}

func TestAnalyzeMarksMissingUsageAndModelUnknown(t *testing.T) {
	body := strings.Replace(envelope(goodResult), `"model":"deepseek-flash",`, "", 1)
	body = strings.Replace(body, `"usage":{"prompt_tokens":12,"completion_tokens":34,"total_tokens":46}`, `"usage":null`, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
	defer srv.Close()
	out, err := (&Client{Endpoint: srv.URL, Key: "test-secret", Model: "deepseek-flash"}).Analyze(context.Background(), fixture, "分析")
	if err != nil {
		t.Fatal(err)
	}
	if out.Model == "" || !strings.Contains(out.Metadata(), "未返回用量") || strings.Contains(out.Metadata(), "合计 0") {
		t.Error("missing metadata was presented as measured zero usage")
	}
}

func TestAnalyzeRejectsInvalidEvidenceBeforeSending(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, envelope(goodResult))
	}))
	defer srv.Close()
	for _, ids := range [][]string{{""}, {"E1", "E1"}} {
		s := Snapshot{ID: "invalid"}
		for _, id := range ids {
			s.Evidence = append(s.Evidence, Evidence{ID: id, Kind: "read", Data: json.RawMessage(`{}`)})
		}
		if _, err := (&Client{Endpoint: srv.URL, Key: "test-secret", Model: DefaultModel}).Analyze(context.Background(), s, "分析"); err == nil {
			t.Error("invalid local evidence was accepted")
		}
	}
	if requests.Load() != 0 {
		t.Errorf("invalid local evidence made %d billable requests", requests.Load())
	}
}
