---
name: quarry
description: Search the web or the paper literature and fetch pages from the shell with the quarry CLI. Pages land on disk as markdown, and a question ranks the lines likeliest to answer it. Use for web searches, finding papers, reading pages, several pages at once, or a set number of results.
---

    quarry -s <query>               search the web: numbered results with highlights
    quarry -p <query>               search academic papers: authors, venue, citations
      -n <count>                    results (default 8)
      -d <host[/path]>, -d -<host>  only, or never, this site; repeatable
    quarry -f <url>... [question]   save pages as markdown; print each path and size,
                                    and with a question, the lines likeliest to answer it

Flags and operands mix in any order. Write plain words describing the page you want; quotes hint at exact phrases. The lines named are a ranking, not a verdict: read them first, then search the file.
