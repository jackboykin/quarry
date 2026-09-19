package main

// Jev, TypeSafe's System One model, answers typed questions with calibrated
// probabilities and writes no text. Each section of a page becomes one yes/no
// question, "would an answer to the request draw on this passage?". Questions are
// scored independently, so a long page spreads over parallel requests and the
// probabilities still compare across them. The answer is only ever a line range
// of the saved file: a pointer to read, never a paraphrase to trust.

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	jevAPI     = "https://api.typesafe.ai/v1/systemone"
	jevBudget  = 150_000 // characters per request: ~50k tokens against Jev's 64k
	sectLong   = 3_000   // a section past this splits at its next blank line
	sectCap    = 6_000   // and at this, wherever it stands
	outlineCap = 8_000   // characters of page outline in the state

	// benchmarked on 44 questions over 12 pages (sept 2026): same answers as
	// reading every section whole, at half the tokens on pages this size
	twoPassSize   = 300_000 // a page past this is shortlisted first...
	shortlistPeek = 250     // ...on each section's first characters
	shortlist     = 15      // ...keeping this many to read whole
)

var (
	jevClient = &http.Client{Timeout: 5 * time.Second} // jev answers in under a second; past 5 the page goes without
	atx       = regexp.MustCompile(`^#{1,6} `)         // not #hashtag
	linkTail  = regexp.MustCompile(`\]\([^)]*\)`)
	mdLink    = regexp.MustCompile(`\[[^\]]*\]\([^)]*\)`)
)

type section struct {
	from, to int
	trail    string // the headings it sits under, outermost first
	text     string
}

// One section per heading outside code fences. Each carries the headings above
// it, so "### Highlights" still knows which release it highlights.
func sections(md []byte) []section {
	lines := strings.Split(string(md), "\n")
	var out []section
	var buf []string
	var trail [6]string // the current heading at each level
	from, size, fenced, cur := 1, 0, false, ""
	flush := func(to int) {
		if strings.TrimSpace(strings.Join(buf, "")) != "" {
			out = append(out, section{from, to, cur, strings.Join(buf, "\n")})
		}
		from, buf, size = to+1, nil, 0
	}
	for i, l := range lines {
		if strings.HasPrefix(l, "```") {
			fenced = !fenced
		}
		heading := !fenced && atx.MatchString(l)
		if len(buf) > 0 && (heading || (strings.TrimSpace(l) == "" && size > sectLong) || size > sectCap) {
			flush(i)
		}
		if heading {
			n := strings.IndexByte(l, ' ')
			trail[n-1] = strings.TrimSpace(linkTail.ReplaceAllString(l[n:], "]"))
			clear(trail[n:])
			cur = strings.Join(slices.DeleteFunc(slices.Clone(trail[:]), func(h string) bool { return h == "" }), " › ")
		}
		buf, size = append(buf, l), size+len(l)+1
	}
	flush(len(lines))
	return out
}

// locate names the line ranges of md likely to answer q, best first
func locate(token string, md []byte, q string) (string, error) {
	// Each section is scored alone, which loses where it sits: every release
	// on a changelog has its "Highlights", and only the page's order says which
	// is latest. The top two heading levels, in page order, give every question
	// that map.
	var secs []section
	var outline []string
	size, headed := 0, 0
	for _, s := range sections(md) {
		// a table of contents names everything and says nothing; the outline
		// carries the page's shape
		if links := len(strings.Join(mdLink.FindAllString(s.text, -1), "")); links*2 > len(s.text) {
			continue
		}
		if h := strings.Count(s.trail, " › "); h <= 1 && (len(outline) == 0 || outline[len(outline)-1] != s.trail) && size < outlineCap {
			outline, size = append(outline, s.trail), size+len(s.trail)
		}
		if atx.MatchString(s.text) {
			headed++
		}
		secs = append(secs, s)
	}
	state := map[string]any{"request": q, "page outline, top to bottom": outline}

	// A huge page is shortlisted first on each section's opening lines, then
	// the shortlist is read whole: half the tokens for the same answers, on a
	// page whose headings say what its sections hold. A page mostly split by
	// size rather than by heading is read whole, however long, since its
	// opening lines show only the first of the things a section holds.
	if len(md) > twoPassSize && headed*4 >= len(secs) {
		p, err := score(token, state, secs, shortlistPeek)
		if err != nil {
			return "", err
		}
		slices.SortFunc(secs, func(a, b section) int { return cmp.Compare(p[b.key()], p[a.key()]) })
		secs = secs[:min(len(secs), shortlist)]
	}
	p, err := score(token, state, secs, 0)
	if err != nil {
		return "", err
	}

	keys := slices.DeleteFunc(slices.Collect(maps.Keys(p)), func(k string) bool { return p[k] < 0.7 })
	if len(keys) == 0 {
		return "no section scores as answering this", nil
	}
	slices.SortFunc(keys, func(a, b string) int { return cmp.Compare(p[b], p[a]) })
	var parts []string
	for _, k := range keys[:min(len(keys), 3)] {
		parts = append(parts, fmt.Sprintf("%s (%.2f)", k, p[k]))
	}
	// a listing page answers an open request everywhere at once; three of
	// fourteen near-ties are picked by noise, so say how wide the field is
	if len(keys)*3 > len(secs) {
		return fmt.Sprintf("%d of %d sections score as answering it, so these pointers are near-ties; best first: lines %s", len(keys), len(secs), strings.Join(parts, ", ")), nil
	}
	return "read lines " + strings.Join(parts, ", "), nil
}

func (s section) key() string { return fmt.Sprintf("%d–%d", s.from, s.to) }

// score asks, for every section, whether it holds what the request asks for,
// reading only its first peek characters when peek > 0; the sections spread
// over parallel requests of jevBudget characters each
func score(token string, state map[string]any, secs []section, peek int) (map[string]float64, error) {
	text := func(s section) string {
		if peek > 0 {
			return clip(s.text, peek)
		}
		return s.text
	}
	var batches [][]section
	size := jevBudget
	for _, s := range secs {
		if size+len(text(s)) > jevBudget {
			batches, size = append(batches, nil), 0
		}
		batches[len(batches)-1] = append(batches[len(batches)-1], s)
		size += len(text(s))
	}

	p := map[string]float64{}
	var (
		firstErr error
		mu       sync.Mutex
		wg       sync.WaitGroup
	)
	for _, b := range batches {
		wg.Go(func() {
			qs := map[string]any{}
			for _, s := range b {
				qs[s.key()] = map[string]any{"type": "noul", "instructions": map[string]string{
					"task":    "The request in the state is a question or an instruction, such as summarize, explain, or list, that someone will carry out by reading the page. Does this passage state something their answer would draw on?",
					"section": s.trail,
					"passage": text(s),
				}, "criteria": map[string]string{
					"true":  "The passage states facts, claims, or details that belong in an answer to the request, even if it covers only part of the request",
					"false": "The passage is about something else, or only names the topic without saying anything about it",
				}}
			}
			var out struct {
				Answers map[string]struct{ Noul float64 }
			}
			err := jevPost(token, map[string]any{"model": "jev-latest", "state": state, "questions": qs}, &out)
			mu.Lock()
			defer mu.Unlock()
			if firstErr == nil {
				firstErr = err
			}
			for k, a := range out.Answers {
				p[k] = a.Noul
			}
		})
	}
	wg.Wait()
	return p, firstErr
}

func jevPost(token string, req map[string]any, out any) error {
	body, _ := json.Marshal(req)
	r, _ := http.NewRequest("POST", jevAPI, bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	resp, err := jevClient.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		var e struct{ Detail struct{ Message string } }
		json.Unmarshal(b, &e)
		return fmt.Errorf("jev %d: %s", resp.StatusCode, cmp.Or(e.Detail.Message, resp.Status))
	}
	return json.Unmarshal(b, out)
}
