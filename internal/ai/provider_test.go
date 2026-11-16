package ai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gauravthakur02/kubesage/internal/config"
)

func collect(t *testing.T, p Provider) string {
	t.Helper()
	out := make(chan string, 100)
	errc := make(chan error, 1)
	go func() {
		errc <- p.Stream(context.Background(), "sys", []Message{{Role: "user", Content: "hi"}}, out)
		close(out)
	}()
	var b strings.Builder
	for s := range out {
		b.WriteString(s)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestOpenAICompatibleSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "bad request "+r.URL.Path, 400)
			return
		}
		for _, s := range []string{"Hel", "lo"} {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", s)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	p, err := New("t", config.AIProfile{Provider: "openai-compatible", BaseURL: srv.URL + "/v1", APIKey: "k", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if got := collect(t, p); got != "Hello" {
		t.Fatalf("got %q", got)
	}
}

func TestAzureURLAndHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openai/deployments/gpt4o/chat/completions" || r.URL.Query().Get("api-version") == "" || r.Header.Get("api-key") != "k" {
			http.Error(w, "bad", 400)
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
	}))
	defer srv.Close()
	p, err := New("t", config.AIProfile{Provider: "azure", BaseURL: srv.URL, APIKey: "k", Model: "gpt4o"})
	if err != nil {
		t.Fatal(err)
	}
	if got := collect(t, p); got != "ok" {
		t.Fatalf("got %q", got)
	}
}

func TestOllamaNDJSONAndAutoModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			fmt.Fprint(w, `{"models":[{"name":"llama3.1:8b"}]}`)
		case "/api/chat":
			fmt.Fprintln(w, `{"message":{"content":"a"},"done":false}`)
			fmt.Fprintln(w, `{"message":{"content":"b"},"done":true}`)
		}
	}))
	defer srv.Close()
	p, _ := New("local", config.AIProfile{Provider: "ollama", BaseURL: srv.URL})
	if got := collect(t, p); got != "ab" {
		t.Fatalf("got %q", got)
	}
	if p.Model() != "llama3.1:8b" {
		t.Fatalf("model not auto-picked: %s", p.Model())
	}
}

func TestGeminiSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "k" || !strings.HasSuffix(r.URL.Path, ":streamGenerateContent") {
			http.Error(w, "bad", 400)
			return
		}
		fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hey\"}]}}]}\n\n")
	}))
	defer srv.Close()
	p, err := New("g", config.AIProfile{Provider: "gemini", BaseURL: srv.URL, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if got := collect(t, p); got != "hey" {
		t.Fatalf("got %q", got)
	}
}

func TestHTTPErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid key"}`, 401)
	}))
	defer srv.Close()
	p, _ := New("t", config.AIProfile{Provider: "openai", BaseURL: srv.URL})
	err := p.Stream(context.Background(), "", nil, make(chan string, 1))
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401 error, got %v", err)
	}
}

func TestUnknownProvider(t *testing.T) {
	if _, err := New("x", config.AIProfile{Provider: "nope"}); err == nil {
		t.Fatal("expected error")
	}
}
