#!/usr/bin/env python3
"""Regenerate server-go/src/config/rate_limit_gen.go from server/src/config/rate-limit.ts.

The TypeScript file is the single source of truth for rate-limit rules. This
script extracts the exported `rateLimitRules`, `globalRateLimit` and
`rateLimitWhitelist` literals and emits the Go mirror.

Usage:
    python3 tools/gen-ratelimit.py

Run it whenever server/src/config/rate-limit.ts changes and commit both files
together, so the two backends can never drift apart.
"""
import re
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent.parent
TS_PATH = REPO / "server" / "src" / "config" / "rate-limit.ts"
GO_PATH = REPO / "server-go" / "src" / "config" / "rate_limit_gen.go"


def extract_rules(src: str):
    # Slice the rateLimitRules array body. Skip the RateLimitRule[] type annotation:
    # anchor directly on the "= [" that opens the literal.
    anchor = src.index("export const rateLimitRules: RateLimitRule[] = [")
    start = src.index("[", anchor + len("export const rateLimitRules: RateLimitRule[] =") )
    depth = 0
    for i in range(start, len(src)):
        if src[i] == "[":
            depth += 1
        elif src[i] == "]":
            depth -= 1
            if depth == 0:
                body = src[start:i]
                break
    rules = []
    for m in re.finditer(
        r"path:\s*'([^']+)'\s*,\s*"
        r"method:\s*'([^']+)'\s*,\s*"
        r"max:\s*(\d+)\s*,\s*"
        r"timeWindow:\s*'([^']+)'\s*,\s*"
        r"description:\s*'((?:[^'\\]|\\.)*)'",
        body,
    ):
        rules.append({
            "path": m.group(1),
            "method": m.group(2),
            "max": int(m.group(3)),
            "timeWindow": m.group(4),
            "description": m.group(5).replace("\\'", "'"),
        })
    return rules


def extract_global(src: str):
    m = re.search(
        r"export const globalRateLimit = \{\s*"
        r"(?:/\*\*.*?\*/\s*)?max:\s*(\d+)\s*,\s*"
        r"(?:/\*\*.*?\*/\s*)?timeWindow:\s*'([^']+)'\s*,\s*"
        r"(?:/\*\*.*?\*/\s*)?description:\s*'([^']+)'",
        src,
        re.S,
    )
    if not m:
        sys.exit("gen-ratelimit: could not parse globalRateLimit")
    return {"max": int(m.group(1)), "timeWindow": m.group(2), "description": m.group(3)}


def extract_whitelist(src: str):
    m = re.search(r"pathPrefixes:\s*\[(.*?)\]", src, re.S)
    prefixes = re.findall(r"'([^']+)'", m.group(1)) if m else []
    m = re.search(r"exactPaths:\s*\[(.*?)\]", src, re.S)
    exact = re.findall(r"'([^']+)'", m.group(1)) if m else []
    return prefixes, exact


def go_str(s: str) -> str:
    return '"' + s.replace("\\", "\\\\").replace('"', '\\"') + '"'


def main():
    src = TS_PATH.read_text(encoding="utf-8")
    rules = extract_rules(src)
    glob = extract_global(src)
    prefixes, exact = extract_whitelist(src)

    out = []
    out.append("// Code generated from server/src/config/rate-limit.ts by tools/gen-ratelimit.py. DO NOT EDIT.")
    out.append("//")
    out.append("// Regenerate with `python3 tools/gen-ratelimit.py` after changing the")
    out.append("// TypeScript source; the two files must always be committed together.")
    out.append("")
    out.append("package config")
    out.append("")
    out.append("// RateLimitRule mirrors RateLimitRule in the Node backend.")
    out.append("type RateLimitRule struct {")
    out.append("\tPath        string")
    out.append("\tMethod      string")
    out.append("\tMax         int")
    out.append("\tTimeWindow  string")
    out.append("\tDescription string")
    out.append("}")
    out.append("")
    out.append("// GlobalRateLimit is the default per-IP limit applied to every request.")
    out.append("var GlobalRateLimit = RateLimitRule{")
    out.append(f'\tPath:        "*",')
    out.append(f'\tMethod:      "*",')
    out.append(f'\tMax:         {glob["max"]},')
    out.append(f'\tTimeWindow:  {go_str(glob["timeWindow"])},')
    out.append(f'\tDescription: {go_str(glob["description"])},')
    out.append("}")
    out.append("")
    out.append("// RateLimitRules lists the per-route overrides in declaration order.")
    out.append("var RateLimitRules = []RateLimitRule{")
    for r in rules:
        out.append("\t{")
        out.append(f'\t\tPath:        {go_str(r["path"])},')
        out.append(f'\t\tMethod:      {go_str(r["method"])},')
        out.append(f'\t\tMax:         {r["max"]},')
        out.append(f'\t\tTimeWindow:  {go_str(r["timeWindow"])},')
        out.append(f'\t\tDescription: {go_str(r["description"])},')
        out.append("\t},")
    out.append("}")
    out.append("")
    out.append("// RateLimitWhitelistExactPaths skip rate limiting entirely.")
    if exact:
        out.append("var RateLimitWhitelistExactPaths = []string{")
        for p in exact:
            out.append(f"\t{go_str(p)},")
        out.append("}")
    else:
        out.append("var RateLimitWhitelistExactPaths = []string{}")
    out.append("")
    out.append("// RateLimitWhitelistPathPrefixes skip rate limiting for matching prefixes.")
    if prefixes:
        out.append("var RateLimitWhitelistPathPrefixes = []string{")
        for p in prefixes:
            out.append(f"\t{go_str(p)},")
        out.append("}")
    else:
        out.append("var RateLimitWhitelistPathPrefixes = []string{}")
    out.append("")

    GO_PATH.write_text("\n".join(out), encoding="utf-8")
    print(f"gen-ratelimit: wrote {len(rules)} rules to {GO_PATH.relative_to(REPO)}")


if __name__ == "__main__":
    main()
