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
	"errors"
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
	atx       = regexp.MustCompile(`^ {0,3}(#{1,6})(?:[ \t]+(.*))?$`)
	setext    = regexp.MustCompile(`^ {0,3}(=+|-+)[ \t]*$`)
	fenceLine = regexp.MustCompile("^[ \t]*(`{3,}|~{3,})(.*)$")
	linkTail  = regexp.MustCompile(`\]\([^)]*\)`)
	mdLink    = regexp.MustCompile(`\[[^\]]*\]\([^)]*\)`)
	numHead   = regexp.MustCompile(`^(?:\d+(?:\.\d+)*\.?|Appendix\s+[A-Z]\.?)\s+\S`)
	codeDecl  = regexp.MustCompile(`^(?:pub\s+)?(?:static\s+|const\s+|inline\s+|extern\s+|async\s+)*(?:func|fn|def|class|struct|enum|type|impl|trait|interface|void|int|char|bool|size_t|ssize_t|uint\d+_t|int\d+_t)\b`)
	rfcPage   = regexp.MustCompile(`\[Page \d+\]\s*$|^RFC \d+\s{2,}`)
)

type section struct {
	from, to int
	trail    string // the headings it sits under, outermost first
	text     string
	headed   bool // it opens on a heading, rather than a split by size
}

// One section per heading outside code fences. Each carries the headings above
// it, so "### Highlights" still knows which release it highlights.
func sections(md []byte) []section {
	lines := strings.Split(string(md), "\n")
	var out []section
	var buf []string
	var trail [6]string // the current heading at each level
	from, size, fence, cur, headed := 1, 0, "", "", false
	flush := func(to int) {
		if strings.TrimSpace(strings.Join(buf, "")) != "" {
			out = append(out, section{from, to, cur, strings.Join(buf, "\n"), headed})
		}
		from, buf, size, headed = to+1, nil, 0, false
	}
	mdHeadings := 0
	for i, l := range lines {
		// a fence closes on a run of its own character at least as long;
		// a backtick fence's info string holds no backtick
		f := fenceLine.FindStringSubmatch(l)
		if f != nil && fence == "" && !(f[1][0] == '`' && strings.Contains(f[2], "`")) {
			fence = f[1]
		} else if f != nil && fence != "" && f[1][0] == fence[0] && len(f[1]) >= len(fence) && strings.TrimSpace(f[2]) == "" {
			fence = ""
		}
		level, title := 0, ""
		if m := atx.FindStringSubmatch(l); fence == "" && m != nil {
			level, title = len(m[1]), m[2]
		} else if fence == "" && f == nil && strings.TrimSpace(l) != "" && !setext.MatchString(l) &&
			i+1 < len(lines) && setext.MatchString(lines[i+1]) {
			level, title = 1, l
			if strings.TrimSpace(lines[i+1])[0] == '-' {
				level = 2
			}
		}
		if len(buf) > 0 && (level > 0 || (strings.TrimSpace(l) == "" && size > sectLong) || size > sectCap) {
			flush(i)
		}
		if level > 0 {
			mdHeadings++
			headed = true
			trail[level-1] = strings.TrimSpace(linkTail.ReplaceAllString(title, "]"))
			clear(trail[level:])
			cur = strings.Join(slices.DeleteFunc(slices.Clone(trail[:]), func(h string) bool { return h == "" }), " › ")
		}
		buf, size = append(buf, l), size+len(l)+1
	}
	flush(len(lines))
	if mdHeadings == 0 {
		return outdentSections(lines)
	}
	return out
}

// In plain text without markdown headings (RFCs, C/Zig/Go source), a column-0
// numbered heading or top-level declaration after a blank line opens a section.
// headed stays false so outdent lines never trigger the two-pass shortlist.
func outdentSections(lines []string) []section {
	var out []section
	var buf []string
	from, size, cur := 1, 0, ""
	flush := func(to int) {
		if strings.TrimSpace(strings.Join(buf, "")) != "" {
			out = append(out, section{from, to, cur, strings.Join(buf, "\n"), false})
		}
		from, buf, size, cur = to+1, nil, 0, ""
	}
	prevBlank := true
	for i, l := range lines {
		blank := strings.TrimSpace(l) == ""
		if !blank && prevBlank && isOutdentHeading(l) {
			if len(buf) > 0 && size >= 200 {
				flush(i)
			}
			if len(buf) == 0 {
				title := strings.TrimSpace(l)
				if len(title) <= 30 && !strings.ContainsAny(title, "({#") && i+1 < len(lines) &&
					len(lines[i+1]) > 0 && lines[i+1][0] != ' ' && lines[i+1][0] != '\t' && strings.Contains(lines[i+1], "(") {
					title += " " + strings.TrimSpace(lines[i+1])
				}
				cur = clip(title, 80)
			}
		} else if len(buf) > 0 && ((blank && size > sectLong) || size > sectCap) {
			flush(i)
		}
		buf, size = append(buf, l), size+len(l)+1
		prevBlank = blank
	}
	flush(len(lines))
	return out
}

func isOutdentHeading(l string) bool {
	if len(l) == 0 || l[0] == ' ' || l[0] == '\t' || l[0] == '\f' {
		return false
	}
	t := strings.TrimSpace(l)
	if len(t) < 2 || len(t) > 100 || rfcPage.MatchString(t) {
		return false
	}
	if numHead.MatchString(t) && !strings.HasSuffix(t, ".") {
		return true
	}
	return codeDecl.MatchString(t) || (strings.Contains(t, "(") && !strings.HasSuffix(t, ".") && !strings.HasSuffix(t, ","))
}

// locate names the line ranges of md likely to answer q, best first
func locate(token string, md []byte, q string) (string, error) {
	// Each section is scored alone, which loses where it sits: every release
	// on a changelog has its "Highlights", and only the page's order says which
	// is latest. The top two heading levels, in page order, give every question
	// that map.
	// a table of contents names everything and says nothing; the outline
	// carries the page's shape. But on a page that is mostly links, a
	// front page of stories, the links are the content.
	all := sections(md)
	linky := func(s section) bool { return len(strings.Join(mdLink.FindAllString(s.text, -1), ""))*2 > len(s.text) }
	if kept := slices.DeleteFunc(slices.Clone(all), linky); len(kept)*2 >= len(all) {
		all = kept
	}
	if len(all) <= 3 {
		return fmt.Sprintf("  a short page of %d sections; check it directly", len(all)), nil
	}
	var secs []section
	var outline []string
	size, headed := 0, 0
	for _, s := range all {
		if h := strings.Count(s.trail, " › "); h <= 1 && s.trail != "" && (len(outline) == 0 || outline[len(outline)-1] != s.trail) && size < outlineCap {
			outline, size = append(outline, s.trail), size+len(s.trail)
		}
		if s.headed {
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
	total := len(secs)
	if len(md) > twoPassSize && headed*4 >= len(secs) {
		p, err := score(token, state, secs, shortlistPeek)
		if err != nil {
			return "", err
		}
		slices.SortFunc(secs, func(a, b section) int { return cmp.Compare(p[b.key()], p[a.key()]) })
		secs = secs[:min(len(secs), shortlist)]
	}
	p, partialErr := score(token, state, secs, 0)
	if len(p) == 0 {
		return "", cmp.Or(partialErr, errors.New("no sections scored"))
	}

	scored := slices.DeleteFunc(slices.Clone(secs), func(s section) bool { _, ok := p[s.key()]; return !ok })
	slices.SortFunc(scored, func(a, b section) int { return cmp.Compare(p[b.key()], p[a.key()]) })
	above := 0
	for _, s := range scored {
		if p[s.key()] >= 0.7 {
			above++
		}
	}
	if above == 0 && p[scored[0].key()] < 0.15 {
		return fmt.Sprintf("  no section scores as answering this (highest: %.2f)", p[scored[0].key()]), partialErr
	}
	var lines []string
	if above == 0 {
		lines = append(lines, "  nothing scores above 0.7; nearest:")
	} else if above*3 > total {
		lines = append(lines, fmt.Sprintf("  %d of %d sections score above 0.7 (near-ties):", above, total))
	}
	for _, s := range scored[:min(len(scored), 3)] {
		if s.trail != "" {
			lines = append(lines, fmt.Sprintf("  %s (%.2f)  %s", s.key(), p[s.key()], clip(s.trail, 80)))
		} else {
			lines = append(lines, fmt.Sprintf("  %s (%.2f)", s.key(), p[s.key()]))
		}
	}
	return strings.Join(lines, "\n"), partialErr
}

func (s section) key() string { return fmt.Sprintf("%d–%d", s.from, s.to) }

// score asks, for every section, whether it holds what the request asks for,
// reading only its first peek characters when peek > 0; the sections spread
// over parallel requests of jevBudget characters each
func score(token string, state map[string]any, secs []section, peek int) (map[string]float64, error) {
	stateBytes, _ := json.Marshal(state)
	stateLen := len(stateBytes)
	var batches []map[string]any
	size := jevBudget
	for _, s := range secs {
		text := s.text
		if peek > 0 {
			text = clip(text, peek)
		}
		q := map[string]any{"type": "noul", "instructions": map[string]string{
			"task":    "The request in the state is a question or an instruction, such as summarize, explain, or list, that someone will carry out by reading the page. Does this passage state something their answer would draw on?",
			"section": s.trail,
			"passage": text,
		}, "criteria": map[string]string{
			"true":  "The passage states facts, claims, or details that belong in an answer to the request, even if it covers only part of the request",
			"false": "The passage is about something else, or only names the topic without saying anything about it",
		}}
		b, _ := json.Marshal(q)
		if size+len(b) > jevBudget {
			batches, size = append(batches, map[string]any{}), stateLen
		}
		batches[len(batches)-1][s.key()] = q
		size += len(b)
	}

	p := map[string]float64{}
	var (
		firstErr error
		mu       sync.Mutex
		wg       sync.WaitGroup
	)
	for _, qs := range batches {
		wg.Go(func() {
			got, err := scoreBatch(token, state, qs)
			mu.Lock()
			defer mu.Unlock()
			if firstErr == nil {
				firstErr = err
			}
			maps.Copy(p, got)
		})
	}
	wg.Wait()
	return p, firstErr
}

// scoreBatch posts one batch and bisects on max_tokens_exceeded so dense
// code or hex dumps never fail a request.
func scoreBatch(token string, state map[string]any, qs map[string]any) (map[string]float64, error) {
	var out struct {
		Answers map[string]struct{ Noul float64 }
	}
	err := jevPost(token, map[string]any{"model": "jev-latest", "state": state, "questions": qs}, &out)
	var je *jevError
	if errors.As(err, &je) && je.kind == "max_tokens_exceeded" && len(qs) > 1 {
		keys := slices.Collect(maps.Keys(qs))
		mid := len(keys) / 2
		left, right := make(map[string]any, mid), make(map[string]any, len(keys)-mid)
		for _, k := range keys[:mid] {
			left[k] = qs[k]
		}
		for _, k := range keys[mid:] {
			right[k] = qs[k]
		}
		got := map[string]float64{}
		l, errL := scoreBatch(token, state, left)
		r, errR := scoreBatch(token, state, right)
		maps.Copy(got, l)
		maps.Copy(got, r)
		return got, cmp.Or(errL, errR)
	}
	if err != nil {
		return nil, err
	}
	got := make(map[string]float64, len(out.Answers))
	for k, a := range out.Answers {
		got[k] = a.Noul
	}
	return got, nil
}

type jevError struct {
	status    int
	kind, msg string
}

func (e *jevError) Error() string { return fmt.Sprintf("jev %d: %s", e.status, e.msg) }

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
		var e struct {
			Detail struct {
				Message   string
				ErrorType string `json:"error_type"`
			}
		}
		json.Unmarshal(b, &e)
		return &jevError{
			status: resp.StatusCode,
			kind:   e.Detail.ErrorType,
			msg:    cmp.Or(e.Detail.Message, e.Detail.ErrorType, resp.Status),
		}
	}
	return json.Unmarshal(b, out)
}
