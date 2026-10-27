// quarry — search the web, or read a page.
// build: CGO_ENABLED=0 go build -trimpath -ldflags="-s -w"
package main

import (
	"bufio"
	"bytes"
	"cmp"
	"crypto/tls"
	_ "embed"
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

//go:embed skills/quarry/SKILL.md
var skill string

// the help a person reads is the skill a model reads: its description, then its body
var usage = func() string {
	_, fm, _ := strings.Cut(skill, "---\n")
	fm, body, _ := strings.Cut(fm, "---\n")
	_, desc, _ := strings.Cut(fm, "description: ")
	desc, _, _ = strings.Cut(desc, "\n")
	return desc + "\n" + body
}()

const (
	maxChars = 1_000_000
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
	ID, URL, Title, PublishedDate, Text string
	Highlights                          []string
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
	r.Header.Set("Exa-Beta", "dynamic-highlights-2026-08-28")
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
	req := map[string]any{
		"query":      q,
		"numResults": num,
		"contents":   map[string]any{"highlights": map[string]any{"dynamic": true}},
	}
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
	if len(res.Results) == 0 && len(include) > 0 {
		die("no results from %s; exa may not index it", strings.Join(include, ", "))
	} else if len(res.Results) == 0 {
		die("no results")
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
			fmt.Fprintf(w, "  %s\n", spaces.ReplaceAllString(h, " "))
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
	url   string
	ok    bool
	notes []string
	md    []byte
	bin   []byte
}

func (p *page) say(f string, a ...any) { p.notes = append(p.notes, fmt.Sprintf(f, a...)) }

func words(md []byte) int { return len(word.FindAllIndex(linkURL.ReplaceAll(md, []byte("]")), -1)) }

var (
	linkURL = regexp.MustCompile(`\]\([^)]*\)`)
	word    = regexp.MustCompile(`[\p{L}\p{N}]+`)
)

func read(urls []string, question string) {
	// Every page is also asked of exa, and exa's copy kept when it has over
	// twice the words: where both got the page they agree within a few
	// percent, and where one did not, the other has many times the text.
	// Nothing reaches disk before that, so a page that 403s today keeps
	// yesterday's good copy.
	pages := make([]*page, len(urls))
	sem := make(chan struct{}, par)
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Go(func() { sem <- struct{}{}; pages[i] = fetch(u); <-sem })
	}
	exa := &exaResponse{}
	// exa's cache, crawling only on a miss: forcing a crawl timed out on
	// ~1 in 4 wikipedia reads
	if apiKey != "" {
		wg.Go(func() {
			r, err := post("contents", map[string]any{"urls": urls, "text": map[string]any{"maxCharacters": maxChars}})
			if err != nil {
				fmt.Fprintf(os.Stderr, "quarry: %v\n", err)
				return
			}
			exa = r
		})
	}
	wg.Wait()

	jevKey := key("TYPESAFE_API_KEY", "typesafe-api-key")
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	for _, p := range pages {
		out := outDir + "/" + slug(p.url) + ".md"
		var hit exaResult
		for _, r := range exa.Results {
			if r.ID == p.url || r.URL == p.url {
				hit = r
			}
		}
		var st exaStatus
		for _, s := range exa.Statuses {
			if s.ID == p.url {
				st = s
			}
		}

		mine := 0
		if p.ok {
			mine = words(p.md)
			if mine == 0 && p.bin == nil {
				p.say("the page as served has no text")
			}
		}
		if theirs := words([]byte(hit.Text)); theirs > 2*mine {
			if mine > 0 {
				p.say("the page as served had %d words, exa's copy %d; kept exa's", mine, theirs)
			}
			p.md, p.bin, p.ok = []byte(hit.Text), nil, true
			p.say("text came from exa (%s)", cmp.Or(st.Source, "unknown source"))
		} else if mine == 0 && p.bin == nil {
			p.ok = false
			p.say("exa could not get it either (%s)", strings.ToLower(cmp.Or(st.Error.Tag, "no text")))
		}

		saved := false
		if p.ok {
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
	p := &page{url: u}
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
		var dnsErr *net.DNSError
		var certErr *tls.CertificateVerificationError
		var netErr net.Error
		switch {
		case errors.As(err, &dnsErr):
			p.say("dns did not resolve — check the hostname")
		case errors.Is(err, syscall.ECONNREFUSED):
			p.say("connection refused — nothing is listening there")
		case errors.As(err, &netErr) && netErr.Timeout():
			p.say("timed out after 45s — the server hung")
		case errors.As(err, &certErr):
			p.say("tls handshake failed — cert or protocol mismatch")
		default:
			p.say("could not be reached (%v)", err)
		}
		return p
	}
	if c := resp.StatusCode; c/100 != 2 {
		p.say("HTTP %d", c)
		return p
	}
	if final := resp.Request.URL.String(); final != req.URL.String() {
		p.say("redirected to %s", final)
	}

	p.ok = true
	switch ctype := resp.Header.Get("Content-Type"); {
	case strings.Contains(ctype, "html"):
		p.md = fromHTML(body, resp.Request.URL.String())
	case strings.Contains(ctype, "json"):
		var b bytes.Buffer
		if json.Indent(&b, body, "", "  ") == nil {
			p.md = append(b.Bytes(), '\n')
		} else {
			p.md = body
		}
	case strings.Contains(ctype, "pdf"):
		p.md = fromPDF(body)
	case strings.HasPrefix(ctype, "text/"), strings.Contains(ctype, "markdown"), strings.Contains(ctype, "xml"),
		strings.Contains(ctype, "x-sh"), strings.Contains(ctype, "javascript"):
		p.md = body
	default:
		p.bin = body
		p.md = fmt.Appendf(nil, "# binary content\n%s, %d bytes\n", ctype, len(body))
	}
	return p
}

// The reader greps, so text stays as written: no escapes, and < > ride
// through the converter as noncharacters, which no page can contain.
var (
	hideAngles = strings.NewReplacer("<", "\uFDD0", ">", "\uFDD1")
	showAngles = strings.NewReplacer("\uFDD0", "<", "\uFDD1", ">")
)

var conv = func() *converter.Converter {
	c := converter.NewConverter(converter.WithEscapeMode(converter.EscapeModeDisabled), converter.WithPlugins(
		base.NewBasePlugin(), commonmark.NewCommonmarkPlugin(), table.NewTablePlugin()))
	c.Register.TextTransformer(func(_ converter.Context, s string) string { return hideAngles.Replace(s) }, converter.PriorityEarly)
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
	// a forum's posts and a listing's teasers are articles too, so an article
	// is the content only when it is the page's one article
	articles := 0
	for _, n := range all {
		if n.Type == html.ElementNode && n.Data == "article" {
			articles++
		}
	}
	root := doc
narrow:
	for _, match := range []func(*html.Node) bool{
		func(n *html.Node) bool { return n.Data == "main" },
		func(n *html.Node) bool { return dom.GetAttributeOr(n, "role", "") == "main" },
		func(n *html.Node) bool { return n.Data == "article" && articles == 1 },
	} {
		for _, n := range all {
			if n.Type == html.ElementNode && match(n) && len(strings.TrimSpace(dom.CollectText(n))) > 500 {
				root = n
				break narrow
			}
		}
	}
	md, _ := conv.ConvertNode(root, converter.WithDomain(final))
	return append([]byte(showAngles.Replace(string(md))), '\n')
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
