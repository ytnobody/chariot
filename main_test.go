package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestAsk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization header = %q", got)
		}
		var req request
		json.NewDecoder(r.Body).Decode(&req)
		if req.Questions[questionID].Type != "noul" || req.State != "hello" {
			t.Errorf("unexpected request body: %+v", req)
		}
		w.Write([]byte(`{"answers":{"q":{"type":"noul","noul":0.9}}}`))
	}))
	defer srv.Close()

	orig := apiURLVar
	apiURLVar = srv.URL
	defer func() { apiURLVar = orig }()

	answer, err := ask("secret", "jev-latest", question{Type: "noul", Instructions: "is it urgent?"}, "hello")
	if err != nil {
		t.Fatalf("ask returned error: %v", err)
	}
	if !strings.Contains(string(answer), `"noul":0.9`) {
		t.Errorf("answer = %s", answer)
	}
}

func TestParseCriteria(t *testing.T) {
	cases := []struct {
		name  string
		qType string
		in    string
		want  any
	}{
		{"json object", "choice", `{"a":"opt a"}`, map[string]any{"a": "opt a"}},
		{"json array", "score", `["a","b"]`, []any{"a", "b"}},
		{"choice semicolon shorthand", "choice", `returns: exchanges; shipping: delays`, map[string]any{"returns": "exchanges", "shipping": "delays"}},
		{"choice newline shorthand", "choice", "returns: exchanges\nshipping: delays", map[string]any{"returns": "exchanges", "shipping": "delays"}},
		{"choice comma-separated plain names", "choice", "billing, technical, sales", map[string]any{"billing": "billing", "technical": "technical", "sales": "sales"}},
		{"choice mixed plain and key:description", "choice", "billing, technical: has a colon", map[string]any{"billing": "billing", "technical": "has a colon"}},
		{"score plain names become an ordered list", "score", "calm, frustrated, very angry", []any{"calm", "frustrated", "very angry"}},
		{"score key:description keeps only the description, in order", "score", "0: calm; 1: frustrated", []any{"calm", "frustrated"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseCriteria(c.qType, []byte(c.in))
			if err != nil {
				t.Fatalf("parseCriteria(%q, %q) error: %v", c.qType, c.in, err)
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(c.want)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("parseCriteria(%q, %q) = %s, want %s", c.qType, c.in, gotJSON, wantJSON)
			}
		})
	}
}

func TestTSVValue(t *testing.T) {
	cases := []struct {
		name           string
		answer         string
		wantValue      string
		wantConfidence string
	}{
		{"noul has no confidence", `{"type":"noul","noul":0.92}`, "0.92", ""},
		{"choice", `{"type":"choice","choice":"technical","probabilities":{"technical":0.85},"confidence":0.82}`, "technical", "0.82"},
		{"score", `{"type":"score","score":1.6,"legend":{},"probabilities":{},"confidence":0.78}`, "1.6", "0.78"},
		{"unknown falls back to raw JSON", `{"type":"weird"}`, `{"type":"weird"}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			value, confidence := tsvValue([]byte(c.answer))
			if value != c.wantValue || confidence != c.wantConfidence {
				t.Errorf("tsvValue(%s) = (%q, %q), want (%q, %q)", c.answer, value, confidence, c.wantValue, c.wantConfidence)
			}
		})
	}
}

func TestWriteResultsFlushesPerLine(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStdout := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = origStdout }()

	results := make(chan result)
	done := make(chan struct{})
	go func() {
		writeResults(results, "json")
		close(done)
	}()

	results <- result{line: 1, text: "hello", answer: []byte(`{"type":"noul","noul":0.9}`)}

	reader := bufio.NewReader(r)
	lineCh := make(chan string, 1)
	go func() {
		line, _ := reader.ReadString('\n')
		lineCh <- line
	}()

	select {
	case line := <-lineCh:
		if !strings.Contains(line, `"line":1`) || !strings.Contains(line, `"noul":0.9`) {
			t.Errorf("unexpected line: %s", line)
		}
	case <-time.After(time.Second):
		t.Fatal("first result was not flushed before the channel closed; output is buffered until exit")
	}

	close(results)
	<-done
	w.Close()
}

func TestAskErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"type":"RegionError"}`))
	}))
	defer srv.Close()

	orig := apiURLVar
	apiURLVar = srv.URL
	defer func() { apiURLVar = orig }()

	_, err := ask("secret", "jev-latest", question{Type: "noul", Instructions: "x"}, "hello")
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("expected 403 error, got %v", err)
	}
}
