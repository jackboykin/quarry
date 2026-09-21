---
name: quarry
description: Search the web and fetch pages from the shell with the quarry CLI. Pages land on disk as markdown, and a question names the lines that answer it. Use for web searches, reading pages, several pages at once, or a set number of results.
---

    quarry -s <query>               search Exa: numbered results with highlights
      -n <count>                    results (default 8)
      -d <host>, -d -<host>         only, or never, this host; repeatable
    quarry -f <url>... [question]   save pages as markdown; print each path and size,
                                    and with a question, the line ranges likely to answer it

Flags and operands mix in any order. Read the ranges it names first, then search the file for anything else.
