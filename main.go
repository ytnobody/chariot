// chariot pipes stdin lines to TypeSafe's Jev (System One) API as the
// `state` of one question, and streams answers back to stdout.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
)

var apiURLVar = "https://api.typesafe.ai/v1/systemone"

const questionID = "q"

type question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

type request struct {
	State     string              `json:"state"`
	Model     string              `json:"model"`
	Questions map[string]question `json:"questions"`
}

type response struct {
	Answers map[string]json.RawMessage `json:"answers"`
}

type job struct {
	line int
	text string
}

type result struct {
	line   int
	text   string
	answer json.RawMessage
	err    error
}

var validTypes = map[string]bool{"choice": true, "score": true, "noul": true}

func main() {
	usage := "usage: chariot <choice|score|noul> [-criteria|-c <json>|@file] [-model|-m jev-latest] [-concurrency|-p 8] [-format|-f json|tsv] [-fifo|-i <path>] <instructions> [option ...]"
	if len(os.Args) < 2 || !validTypes[os.Args[1]] {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	qType := os.Args[1]

	fs := flag.NewFlagSet(qType, flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, usage)
		fmt.Fprintln(os.Stderr, "\noptions (long and short forms are equivalent):")
		fs.PrintDefaults()
	}
	var criteria, model, format, fifo string
	var concurrency int
	criteriaUsage := `criteria: JSON, "a, b, c" for plain names, or "key: description" entries (,/;/newline separated); builds a dict for choice, an ordered list for score; prefix with @ to read from a file (required for choice/score)`
	fs.StringVar(&criteria, "criteria", "", criteriaUsage)
	fs.StringVar(&criteria, "c", "", criteriaUsage+" (shorthand)")
	fs.StringVar(&model, "model", "jev-latest", "System One model")
	fs.StringVar(&model, "m", "jev-latest", "System One model (shorthand)")
	fs.IntVar(&concurrency, "concurrency", 8, "max concurrent requests")
	fs.IntVar(&concurrency, "p", 8, "max concurrent requests (shorthand)")
	fs.StringVar(&format, "format", "json", "output format: json (NDJSON) or tsv")
	fs.StringVar(&format, "f", "json", "output format: json (NDJSON) or tsv (shorthand)")
	fifoUsage := "read from this named pipe instead of stdin; reopens it after writers close so another process can send more input later (mkfifo it first)"
	fs.StringVar(&fifo, "fifo", "", fifoUsage)
	fs.StringVar(&fifo, "i", "", fifoUsage+" (shorthand)")
	fs.Parse(os.Args[2:])

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	instructions := fs.Arg(0)
	options := fs.Args()[1:]
	if len(options) > 0 {
		if criteria != "" {
			fmt.Fprintln(os.Stderr, "chariot: criteria given both via -criteria and as trailing arguments; use one or the other")
			os.Exit(2)
		}
		criteria = strings.Join(options, ", ")
	}

	apiKey := os.Getenv("TYPESAFE_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "chariot: TYPESAFE_API_KEY is not set\n\nGet an API key at https://typesafe.ai, then set it with:\n  export TYPESAFE_API_KEY=your_api_key")
		os.Exit(1)
	}

	q := question{Type: qType, Instructions: instructions}
	if criteria != "" {
		raw := []byte(criteria)
		if path, ok := strings.CutPrefix(criteria, "@"); ok {
			var err error
			raw, err = os.ReadFile(path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "chariot: reading -criteria file: %v\n", err)
				os.Exit(2)
			}
		}
		c, err := parseCriteria(qType, raw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "chariot: invalid -criteria: %v\n", err)
			os.Exit(2)
		}
		q.Criteria = c
	}

	jobs := make(chan job)
	results := make(chan result)

	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Go(func() {
			for j := range jobs {
				answer, err := ask(apiKey, model, q, j.text)
				results <- result{line: j.line, text: j.text, answer: answer, err: err}
			}
		})
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	go func() {
		defer close(jobs)
		n := 0
		emit := func(r io.Reader) {
			scanner := bufio.NewScanner(r)
			scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			for scanner.Scan() {
				text := scanner.Text()
				if text == "" {
					continue
				}
				n++
				jobs <- job{line: n, text: text}
			}
		}

		if fifo == "" {
			if info, err := os.Stdin.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
				fmt.Fprintln(os.Stderr, "chariot: reading input from stdin, one item per line (Ctrl-D to end)")
			}
			emit(os.Stdin)
			return
		}

		// A FIFO reader hits EOF once all writers close it, but reopening
		// blocks until the next writer opens it, so this keeps accepting
		// input from a new process indefinitely. Ctrl-C exits (no writes
		// are lost mid-line, but a mid-request answer may not print).
		for {
			f, err := os.Open(fifo)
			if err != nil {
				fmt.Fprintf(os.Stderr, "chariot: opening -fifo: %v\n", err)
				os.Exit(1)
			}
			emit(f)
			f.Close()
		}
	}()

	writeResults(results, format)
}

// parseCriteria accepts JSON as-is, or a shorthand list of entries
// separated by newlines, semicolons, or commas, with "key: description"
// entries and plain names (which map to themselves) freely mixed. System
// One requires criteria shaped as a dictionary for choice but an ordered
// list for score, so the shorthand builds whichever shape qType needs.
func parseCriteria(qType string, raw []byte) (any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		var v any
		if err := json.Unmarshal(trimmed, &v); err != nil {
			return nil, err
		}
		return v, nil
	}

	sep := "\n"
	switch {
	case bytes.Contains(trimmed, []byte("\n")):
	case bytes.Contains(trimmed, []byte(";")):
		sep = ";"
	default:
		sep = ","
	}

	var entries []string
	for e := range strings.SplitSeq(string(trimmed), sep) {
		if e = strings.TrimSpace(e); e != "" {
			entries = append(entries, e)
		}
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no criteria entries found")
	}

	if qType == "score" {
		list := make([]string, len(entries))
		for i, e := range entries {
			_, val, ok := strings.Cut(e, ":")
			if ok {
				list[i] = strings.TrimSpace(val)
			} else {
				list[i] = e
			}
		}
		return list, nil
	}

	m := map[string]string{}
	for _, e := range entries {
		key, val, ok := strings.Cut(e, ":")
		key = strings.TrimSpace(key)
		if ok {
			val = strings.TrimSpace(val)
		} else {
			val = key
		}
		m[key] = val
	}
	return m, nil
}

func ask(apiKey, model string, q question, state string) (json.RawMessage, error) {
	body, err := json.Marshal(request{
		State:     state,
		Model:     model,
		Questions: map[string]question{questionID: q},
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, apiURLVar, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var raw json.RawMessage
		json.NewDecoder(resp.Body).Decode(&raw)
		return nil, fmt.Errorf("%d: %s", resp.StatusCode, string(raw))
	}

	var r response
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	answer, ok := r.Answers[questionID]
	if !ok {
		return nil, fmt.Errorf("no answer for question %q in response", questionID)
	}
	return answer, nil
}

func writeResults(results <-chan result, format string) {
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	for r := range results {
		if r.err != nil {
			fmt.Fprintf(os.Stderr, "chariot: line %d: %v\n", r.line, r.err)
			continue
		}
		switch format {
		case "tsv":
			value, confidence := tsvValue(r.answer)
			fmt.Fprintf(out, "%d\t%s\t%s\t%s\n", r.line, r.text, value, confidence)
		default:
			line := struct {
				Line   int             `json:"line"`
				Input  string          `json:"input"`
				Answer json.RawMessage `json:"answer"`
			}{r.line, r.text, r.answer}
			enc, _ := json.Marshal(line)
			out.Write(enc)
			out.WriteByte('\n')
		}
		out.Flush()
	}
}

// tsvValue reduces a System One answer to the scalar(s) a TSV row can hold:
// the probability for noul (no confidence field), or the picked option /
// weighted score plus confidence for choice and score. Anything
// unrecognized falls back to raw JSON in the value column.
func tsvValue(answer json.RawMessage) (value, confidence string) {
	var a struct {
		Type       string   `json:"type"`
		Noul       float64  `json:"noul"`
		Choice     string   `json:"choice"`
		Score      float64  `json:"score"`
		Confidence *float64 `json:"confidence"`
	}
	if err := json.Unmarshal(answer, &a); err != nil {
		return string(answer), ""
	}
	if a.Confidence != nil {
		confidence = strconv.FormatFloat(*a.Confidence, 'f', -1, 64)
	}
	switch a.Type {
	case "noul":
		return strconv.FormatFloat(a.Noul, 'f', -1, 64), confidence
	case "choice":
		return a.Choice, confidence
	case "score":
		return strconv.FormatFloat(a.Score, 'f', -1, 64), confidence
	default:
		return string(answer), confidence
	}
}
