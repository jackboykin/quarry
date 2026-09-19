// quarry — search the web, or read a page.
// build: CGO_ENABLED=0 go build -trimpath -ldflags="-s -w"
package main

import (
	"bufio"
	"bytes"
	"cmp"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/JohannesKaufmann/dom"
	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
	"golang.org/x/net/html"
)

const usage = `quarry -s <query>      search: numbered results with highlights
quarry -f <url>... [question]
                       fetch to disk: each saved path and its size,
                       and with a question, the lines likely to answer it
  -n  number of results (default 8)
  -d  only this domain, or -d -host to exclude it (repeatable)
`

const (
	maxChars = 200000 // of a page's text, when exa fetches it for us
	maxBody  = 32 << 20
	par      = 8 // pages fetched at once
)

var (
	outDir           string
	include, exclude []string
	num              int
	apiKey           string
)

func main() {
	fs := flag.NewFlagSet("quarry", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage); os.Exit(2) }
	fs.Func("d", "", func(d string) error {
		if h, ok := strings.CutPrefix(d, "-"); ok {
			exclude = append(exclude, h)
		} else {
			include = append(include, d)
		}
		return nil
	})
	fs.IntVar(&num, "n", 8, "")
	get, find := fs.Bool("f", false, ""), fs.Bool("s", false, "")

	// flag stops at the first operand, so a flag after a url would be fetched as
	// a page; resume after each operand so flags and urls mix in any order
	var args []string
	for rest := os.Args[1:]; ; {
		fs.Parse(rest)
		if fs.NArg() == 0 {
			break
		}
		args, rest = append(args, fs.Arg(0)), fs.Args()[1:]
	}
	// the operation is named, never guessed from the operands
	if len(args) == 0 || *get == *find {
		fs.Usage()
	}
	apiKey = key("EXA_API_KEY", "exa-api-key")
	// the user's own cache, ~/.cache or ~/Library/Caches, not the /tmp every
	// user on the machine can read
	cache, err := os.UserCacheDir()
	if err != nil {
		die("%v", err)
	}
	outDir = cache + "/quarry"
	os.MkdirAll(outDir, 0o700)
	// a page not fetched again within a week goes; nothing else clears a cache
	if es, err := os.ReadDir(outDir); err == nil {
		for _, e := range es {
			if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > 7*24*time.Hour {
				os.Remove(outDir + "/" + e.Name())
			}
		}
	}

	if *find {
		search(strings.Join(args, " "))
		return
	}
	// the words among the urls are a question about them
	urls := slices.DeleteFunc(slices.Clone(args), func(a string) bool { return !isURL(a) })
	if len(urls) == 0 {
		die("-f needs at least one http(s) url")
	}
	read(urls, strings.Join(slices.DeleteFunc(args, isURL), " "))
}

func die(f string, a ...any) { fmt.Fprintf(os.Stderr, "quarry: "+f+"\n", a...); os.Exit(1) }

// key reads an api key from $env, else from ~/.config/quarry/file, on every
// platform, as gh and most command-line tools do
func key(env, file string) string {
	if k := os.Getenv(env); k != "" {
		return k
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = home + "/.config"
	}
	b, _ := os.ReadFile(dir + "/quarry/" + file)
	return strings.TrimSpace(string(b))
}

func isURL(s string) bool { return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") }

// ─── exa ───────────────────────────────────────────────────────────────────

type exaResult struct {
	URL, Title, PublishedDate, Text string
	Highlights                      []string
}

type exaResponse struct {
	Results  []exaResult
	Statuses []exaStatus
	Error    string
}

type exaStatus struct {
	ID, Status, Source string
	Error              struct{ Tag string }
}

var exaClient = &http.Client{Timeout: 120 * time.Second}

func post(endpoint string, req map[string]any) (*exaResponse, error) {
	if apiKey == "" {
		die("no exa api key ($EXA_API_KEY or ~/.config/quarry/exa-api-key)")
	}
	body, _ := json.Marshal(req)
	r, _ := http.NewRequest("POST", "https://api.exa.ai/"+endpoint, bytes.NewReader(body))
	r.Header.Set("x-api-key", apiKey)
	r.Header.Set("Content-Type", "application/json")
	resp, err := exaClient.Do(r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out exaResponse
	err = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 {
		// exa explains a refusal in the body
		return nil, fmt.Errorf("exa %d: %s", resp.StatusCode, out.Error)
	}
	return &out, err
}

// ─── search: the results, each with its highlights ─────────────────────────

var spaces = regexp.MustCompile(`\s+`)

func search(q string) {
	req := map[string]any{"query": q, "numResults": num, "contents": map[string]any{"highlights": true}}
	if len(include) > 0 {
		req["includeDomains"] = include
	}
	if len(exclude) > 0 {
		req["excludeDomains"] = exclude
	}
	res, err := post("search", req)
	if err != nil {
		die("%v", err)
	}
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	for i, r := range res.Results {
		title := strings.TrimSpace(spaces.ReplaceAllString(r.Title, " "))
		if title == "" {
			title = "untitled"
		}
		if len(r.PublishedDate) >= 10 {
			title += "  (" + r.PublishedDate[:10] + ")"
		}
		fmt.Fprintf(w, "[%d] %s\n%s\n", i+1, title, r.URL)
		for _, h := range r.Highlights {
			fmt.Fprintf(w, "  %s\n", clip(spaces.ReplaceAllString(h, " "), 300))
		}
		fmt.Fprintln(w)
	}
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// ─── read ──────────────────────────────────────────────────────────────────

type page struct {
	url, status string // status: ok | retry | dead
	notes       []string
	md, bin     []byte
}

func (p *page) say(f string, a ...any) { p.notes = append(p.notes, fmt.Sprintf(f, a...)) }

func read(urls []string, question string) {
	// Phase 1 — fetch every url concurrently. Nothing reaches disk here: an error
	// page is a successful transfer as far as http is concerned, so a page that
	// 403s today would otherwise overwrite the good extract it gave yesterday.
	// Status decides publication, in phase 3.
	pages := make([]*page, len(urls))
	sem := make(chan struct{}, par)
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		sem <- struct{}{}
		go func() { defer wg.Done(); pages[i] = fetch(u); <-sem }()
	}
	wg.Wait()

	// Phase 2 — one exa call for the urls the local fetch could not get. It reads
	// exa's cache first and crawls only on a miss, as exa recommends: forcing a
	// crawl (maxAgeHours 0) timed out on ~1 in 4 wikipedia reads, and a slightly
	// older copy beats none.
	var want []string
	for _, p := range pages {
		if p.status == "retry" {
			want = append(want, p.url)
		}
	}
	exa := &exaResponse{}
	if apiKey != "" && len(want) > 0 {
		req := map[string]any{"urls": want, "text": map[string]any{"maxCharacters": maxChars}}
		if r, err := post("contents", req); err != nil {
			fmt.Fprintf(os.Stderr, "quarry: %v\n", err)
		} else {
			exa = r
		}
	}

	// Phase 3 — publish and report, in the order the urls were given.
	jevKey := key("TYPESAFE_API_KEY", "typesafe-api-key")
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	for _, p := range pages {
		out := outDir + "/" + slug(p.url) + ".md"
		var hit exaResult
		for _, r := range exa.Results {
			if r.URL == p.url {
				hit = r
			}
		}
		var st exaStatus
		for _, s := range exa.Statuses {
			if s.ID == p.url {
				st = s
			}
		}

		if p.status == "retry" {
			if hit.Text != "" {
				p.md, p.bin, p.status = []byte(hit.Text), nil, "ok"
				// a rescue from exa's cache may lag the live page; say which it was
				p.say("text came from exa (%s)", cmp.Or(st.Source, "unknown source"))
			} else {
				p.say("exa could not reach it either — likely private, geoblocked, or JS-only; open it in a browser")
			}
		}

		saved := false
		if p.status == "ok" {
			if p.bin != nil {
				bin := strings.TrimSuffix(out, ".md") + ".bin"
				os.WriteFile(bin, p.bin, 0o644)
				p.md = fmt.Appendf(p.md, "saved to %s\n", bin)
			}
			saved = os.WriteFile(out, p.md, 0o644) == nil
		} else if fi, err := os.Stat(out); err == nil {
			p.say("kept the copy saved %s", fi.ModTime().Format("2006-01-02 15:04"))
			p.md, _ = os.ReadFile(out)
			saved = true
		}
		// stdout holds only what was saved, so a line there is a file to read;
		// what went wrong goes to stderr
		if saved {
			fmt.Fprintf(w, "%s  (%d B, %d lines)\n", out, len(p.md), bytes.Count(p.md, []byte("\n")))
			if question != "" && jevKey != "" {
				if where, err := locate(jevKey, p.md, question); err != nil {
					p.say("%v", err)
				} else {
					fmt.Fprintf(w, "  %s\n", where)
				}
			}
		}
		if len(p.notes) > 0 {
			fmt.Fprintf(os.Stderr, "! %s: %s\n", p.url, strings.Join(p.notes, "; "))
		}
	}
}

// A current desktop Chrome with the headers Chrome sends alongside its
// user agent. On 22 bot-walled sites (sept 2026) this passed 12 against 9 for
// the user agent alone and 3 to 5 for named crawlers, ChatGPT-User and
// Googlebot among them; TLS impersonation passed no more. Asking for markdown
// first costs nothing and gets it from sites that serve it. Keep the version
// current: a stale Chrome is itself a bot signal.
var browser = map[string]string{
	"User-Agent":                "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36",
	"Accept":                    "text/markdown, text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
	"Accept-Language":           "en-US,en;q=0.9",
	"Sec-Ch-Ua":                 `"Chromium";v="153", "Not-A.Brand";v="24", "Google Chrome";v="153"`,
	"Sec-Ch-Ua-Mobile":          "?0",
	"Sec-Ch-Ua-Platform":        `"Windows"`,
	"Sec-Fetch-Dest":            "document",
	"Sec-Fetch-Mode":            "navigate",
	"Sec-Fetch-Site":            "none",
	"Sec-Fetch-User":            "?1",
	"Upgrade-Insecure-Requests": "1",
}

var (
	unslug    = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
	scheme    = regexp.MustCompile(`^https?://`)
	jsShell   = regexp.MustCompile(`(?i)does not seem to support JavaScript|enable JavaScript to (run|use|view|continue)|You need to enable JavaScript`)
	webClient = &http.Client{Timeout: 45 * time.Second}
)

// slug names a url's file; one cut short carries a hash of the whole url, so
// two long urls sharing a prefix don't overwrite each other
func slug(u string) string {
	s := unslug.ReplaceAllString(scheme.ReplaceAllString(u, ""), "-")
	if len(s) <= 72 {
		return s
	}
	h := fnv.New32a()
	h.Write([]byte(u))
	return fmt.Sprintf("%s-%08x", s[:63], h.Sum32())
}

func fetch(u string) *page {
	p := &page{url: u, status: "ok"}
	req, _ := http.NewRequest("GET", u, nil)
	for k, v := range browser {
		req.Header.Set(k, v)
	}
	resp, err := webClient.Do(req)
	var body []byte
	if err == nil {
		defer resp.Body.Close()
		body, err = io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		if err == nil && len(body) > maxBody {
			err = errors.New("larger than 32 MiB")
		}
	}
	if err != nil {
		// name the failure, not a bare "could not be reached"; retry either way,
		// so exa tries next and appends its verdict in phase 3
		p.status = "retry"
		var dnsErr *net.DNSError
		var certErr *tls.CertificateVerificationError
		var netErr net.Error
		switch {
		case errors.As(err, &dnsErr):
			p.say("dns did not resolve — check the hostname")
		case errors.Is(err, syscall.ECONNREFUSED):
			p.say("connection refused — nothing is listening there")
		case errors.As(err, &netErr) && netErr.Timeout():
			p.say("timed out after 45s — the server hung; exa's cache may still hold it")
		case errors.As(err, &certErr):
			p.say("tls handshake failed — cert or protocol mismatch")
		default:
			p.say("could not be reached (%v)", err)
		}
		return p
	}

	ctype := resp.Header.Get("Content-Type")
	final := resp.Request.URL.String()
	usable, thin, shell := true, false, false
	switch {
	case strings.Contains(ctype, "html"):
		p.md = fromHTML(body, final)
		// Two bars, because saying "this looks thin" is free and reaching for
		// someone else's crawler is not. A short page is common and legitimate,
		// so thin means little text out of a lot of markup; a page that yields
		// nothing at all is the only shape exa can improve on.
		thin, usable = len(p.md) < 400 && len(body) > 50*len(p.md), len(p.md) >= 120
		// its own "enable JavaScript" plea betrays a shell whose content never loaded
		shell = jsShell.Match(p.md)
	case strings.Contains(ctype, "json"):
		var b bytes.Buffer
		if json.Indent(&b, body, "", "  ") == nil {
			p.md = append(b.Bytes(), '\n')
		} else {
			p.md = body
		}
	case strings.Contains(ctype, "pdf"):
		p.md = fromPDF(body)
		usable = len(p.md) > 0
	case strings.HasPrefix(ctype, "text/"), strings.Contains(ctype, "markdown"), strings.Contains(ctype, "xml"),
		strings.Contains(ctype, "x-sh"), strings.Contains(ctype, "javascript"):
		p.md = body
	default:
		p.bin = body
		p.md = fmt.Appendf(nil, "# binary content\n%s, %d bytes\n", ctype, len(body))
	}

	// Escalate only on evidence that another crawler could do better: a block, a
	// rate limit, an outage, or markup that rendered to nothing. A 404 is not a
	// failure to fetch, it is a successful report of absence — retrying is theatre.
	switch c := resp.StatusCode; {
	case c/100 == 2:
		switch {
		case !usable:
			p.status = "retry"
			p.say("the page rendered to no text at all — fetching via exa's crawler")
		case shell:
			// a shell is empty in substance, the one shape exa reliably improves on
			p.status = "retry"
			p.say("javascript-rendered shell — the content never loaded; fetching via exa's crawler")
		case thin:
			p.say("thin extraction — the page may be JS-rendered or mostly outside its main content")
		}
	case c == 404 || c == 410:
		p.status = "dead"
		p.say("HTTP %d — the document is gone", c)
	default:
		p.status = "retry"
		p.say("HTTP %d — the server returned an error page, not the document", c)
	}
	if final != u {
		p.say("redirected to %s", final)
	}
	return p
}

var conv = func() *converter.Converter {
	c := converter.NewConverter(converter.WithPlugins(
		base.NewBasePlugin(), commonmark.NewCommonmarkPlugin(), table.NewTablePlugin()))
	// image urls are unreadable noise; alt text is the only part worth keeping
	c.Register.RendererFor("img", converter.TagTypeInline, func(_ converter.Context, w converter.Writer, n *html.Node) converter.RenderStatus {
		if alt := strings.TrimSpace(dom.GetAttributeOr(n, "alt", "")); alt != "" {
			w.WriteString("![" + alt + "]")
		}
		return converter.RenderSuccess
	}, converter.PriorityEarly)
	// a link around nothing readable — a heading permalink's icon, an image
	// without alt text — would otherwise come out as a bare [](…)
	c.Register.RendererFor("a", converter.TagTypeInline, func(_ converter.Context, _ converter.Writer, n *html.Node) converter.RenderStatus {
		if strings.TrimSpace(dom.CollectText(n)) != "" || slices.ContainsFunc(dom.AllNodes(n), func(m *html.Node) bool {
			return m.Data == "img" && strings.TrimSpace(dom.GetAttributeOr(m, "alt", "")) != ""
		}) {
			return converter.RenderTryNext
		}
		return converter.RenderSuccess
	}, converter.PriorityEarly)
	return c
}()

func fromHTML(body []byte, final string) []byte {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil
	}
	all := dom.AllNodes(doc)
	// The page's own semantic markup, in order of how tightly it wraps the
	// content. Falling back to the whole document means the worst case is more
	// text, never less.
	root := doc
narrow:
	for _, match := range []func(*html.Node) bool{
		func(n *html.Node) bool { return n.Data == "article" },
		func(n *html.Node) bool { return n.Data == "main" },
		func(n *html.Node) bool { return dom.GetAttributeOr(n, "role", "") == "main" },
	} {
		for _, n := range all {
			if n.Type == html.ElementNode && match(n) && len(strings.TrimSpace(dom.CollectText(n))) > 500 {
				root = n
				break narrow
			}
		}
	}
	md, _ := conv.ConvertNode(root, converter.WithDomain(final))
	return append(md, '\n')
}

func fromPDF(body []byte) []byte {
	f, err := os.CreateTemp("", "quarry-*.pdf")
	if err != nil {
		return nil
	}
	defer os.Remove(f.Name())
	f.Write(body)
	f.Close()
	text, _ := exec.Command("pdftotext", "-layout", f.Name(), "-").Output()
	return text
}
