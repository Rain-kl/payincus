// Command conformance replays a list of HTTP requests against two backends (the
// running Node server and the new Go server) and reports status/body differences.
//
// This is the primary compatibility gate for the Go port: the Node "guard" tests
// are static source scans of the TypeScript files and cannot validate a Go binary,
// so behavioural parity must be proven by dual-running both servers against the
// same PostgreSQL database and diffing responses.
//
// Usage:
//
//	go run ./test/conformance -node http://127.0.0.1:3001 -go http://127.0.0.1:3002 \
//	    -file ./test/conformance/requests.example.json
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"
)

type requestCase struct {
	Name    string            `json:"name"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
	// Token, when set, is sent as "Authorization: Bearer <token>".
	Token string `json:"token,omitempty"`
	// SkipBody compares only the status code (useful for volatile payloads).
	SkipBody bool `json:"skipBody,omitempty"`
}

type response struct {
	Status      int
	ContentType string
	Body        []byte
}

func main() {
	nodeBase := flag.String("node", "http://127.0.0.1:3001", "base URL of the Node backend")
	goBase := flag.String("go", "http://127.0.0.1:3002", "base URL of the Go backend")
	file := flag.String("file", "", "path to the request list JSON")
	ignoreKeys := flag.String("ignore-keys", "timestamp", "comma-separated JSON keys to drop before comparing")
	timeout := flag.Duration("timeout", 15*time.Second, "per-request timeout")
	flag.Parse()

	if *file == "" {
		fmt.Fprintln(os.Stderr, "conformance: -file is required")
		os.Exit(2)
	}

	raw, err := os.ReadFile(*file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "conformance: read requests: %v\n", err)
		os.Exit(2)
	}
	var cases []requestCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		fmt.Fprintf(os.Stderr, "conformance: parse requests: %v\n", err)
		os.Exit(2)
	}

	ignored := map[string]bool{}
	for _, key := range strings.Split(*ignoreKeys, ",") {
		if trimmed := strings.TrimSpace(key); trimmed != "" {
			ignored[trimmed] = true
		}
	}

	client := &http.Client{Timeout: *timeout}
	failures := 0

	for _, tc := range cases {
		nodeResp, nodeErr := do(client, *nodeBase, tc)
		goResp, goErr := do(client, *goBase, tc)

		if nodeErr != nil || goErr != nil {
			fmt.Printf("FAIL %-40s transport error: node=%v go=%v\n", tc.Name, nodeErr, goErr)
			failures++
			continue
		}

		if nodeResp.Status != goResp.Status {
			fmt.Printf("FAIL %-40s status: node=%d go=%d\n", tc.Name, nodeResp.Status, goResp.Status)
			failures++
			continue
		}

		if tc.SkipBody {
			fmt.Printf("ok   %-40s status=%d (body skipped)\n", tc.Name, nodeResp.Status)
			continue
		}

		nodeJSON, nodeOK := normalizeJSON(nodeResp.Body, ignored)
		goJSON, goOK := normalizeJSON(goResp.Body, ignored)
		if !nodeOK || !goOK {
			// Non-JSON: fall back to raw byte comparison.
			if !bytes.Equal(nodeResp.Body, goResp.Body) {
				fmt.Printf("FAIL %-40s non-JSON body differs\n", tc.Name)
				failures++
				continue
			}
		} else if !reflect.DeepEqual(nodeJSON, goJSON) {
			fmt.Printf("FAIL %-40s body differs\n  node: %s\n  go:   %s\n", tc.Name, nodeResp.Body, goResp.Body)
			failures++
			continue
		}

		fmt.Printf("ok   %-40s status=%d\n", tc.Name, nodeResp.Status)
	}

	fmt.Printf("\n%d/%d cases matched\n", len(cases)-failures, len(cases))
	if failures > 0 {
		os.Exit(1)
	}
}

func do(client *http.Client, base string, tc requestCase) (response, error) {
	var body io.Reader
	if len(tc.Body) > 0 {
		body = bytes.NewReader(tc.Body)
	}
	request, err := http.NewRequest(strings.ToUpper(tc.Method), base+tc.Path, body)
	if err != nil {
		return response{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	if tc.Token != "" {
		request.Header.Set("Authorization", "Bearer "+tc.Token)
	}
	for key, value := range tc.Headers {
		request.Header.Set(key, value)
	}

	resp, err := client.Do(request)
	if err != nil {
		return response{}, err
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return response{}, err
	}
	return response{
		Status:      resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		Body:        payload,
	}, nil
}

// normalizeJSON decodes a JSON body into a comparable value, dropping ignored keys.
func normalizeJSON(payload []byte, ignored map[string]bool) (any, bool) {
	var decoded any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, false
	}
	return dropKeys(decoded, ignored), true
}

func dropKeys(value any, ignored map[string]bool) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if ignored[key] {
				continue
			}
			out[key] = dropKeys(typed[key], ignored)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = dropKeys(item, ignored)
		}
		return out
	default:
		return value
	}
}
